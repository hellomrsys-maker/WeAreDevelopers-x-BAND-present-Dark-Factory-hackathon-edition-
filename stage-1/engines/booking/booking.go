package booking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/engines/calendar"
	"tablekeeper/store"
)

var (
	ErrNotFound             = errors.New("not_found")
	ErrTableUnavailable     = errors.New("table_unavailable")
	ErrCutoffPassed         = errors.New("cutoff_passed")
	ErrReservationCancelled = errors.New("reservation_cancelled")
	ErrNotOnSlotGrid        = errors.New("not_on_slot_grid")
	ErrOutsideOpeningHours  = errors.New("outside_opening_hours")
	ErrPartyExceedsCapacity = errors.New("party_exceeds_capacity")
	ErrInvalidLocalTime     = errors.New("invalid_local_time")
	ErrValidationFailed     = errors.New("validation_failed")
)

type Engine struct {
	store    *store.Store
	calendar contracts.CalendarEngine
	audit    contracts.AuditEngine
	mu       sync.Mutex
}

func NewEngine(s *store.Store, c contracts.CalendarEngine, a contracts.AuditEngine) *Engine {
	return &Engine{
		store:    s,
		calendar: c,
		audit:    a,
	}
}

// Generate unique 6-character uppercase alphanumeric reference
func generateReference() (string, error) {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		sb.WriteByte(chars[n.Int64()])
	}
	return sb.String(), nil
}

func generateID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%x", prefix, b), nil
}

func (e *Engine) CreateReservation(ctx context.Context, req contracts.CreateReservationParams) (*contracts.Reservation, error) {
	if req.PartySize < 1 {
		return nil, ErrValidationFailed
	}

	rest, err := e.store.GetRestaurant(ctx, req.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	// Find the requested table
	var table *contracts.Table
	for i := range rest.Tables {
		if rest.Tables[i].ID == req.TableID {
			table = &rest.Tables[i]
			break
		}
	}
	if table == nil {
		return nil, ErrNotFound
	}

	// Check party capacity
	if req.PartySize > table.Capacity {
		return nil, ErrPartyExceedsCapacity
	}

	// Parse local time
	startsAt, err := e.calendar.ParseLocalTime(rest.Timezone, req.StartsAtLocal)
	if err != nil {
		if errors.Is(err, calendar.ErrInvalidLocalTime) {
			return nil, ErrInvalidLocalTime
		}
		return nil, ErrValidationFailed
	}

	// Verify opening hours and slot grid
	if !e.calendar.IsWithinOpeningHours(rest, startsAt) {
		return nil, ErrOutsideOpeningHours
	}
	if !e.calendar.IsOnSlotGrid(rest, startsAt) {
		return nil, ErrNotOnSlotGrid
	}

	duration := time.Duration(rest.ReservationDurationMinutes) * time.Minute
	endsAt := startsAt.Add(duration)

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check table availability during interval [startsAt, endsAt)
	var overlapCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations 
		WHERE restaurant_id = ? AND table_id = ? AND status = 'confirmed' 
		  AND starts_at_utc < ? AND ends_at_utc > ?`,
		req.RestaurantID, req.TableID, endsAt.Unix(), startsAt.Unix()).Scan(&overlapCount)
	if err != nil {
		return nil, err
	}
	if overlapCount > 0 {
		return nil, ErrTableUnavailable
	}

	ref, err := generateReference()
	if err != nil {
		return nil, err
	}
	resID, err := generateID("res")
	if err != nil {
		return nil, err
	}

	createdAt := req.Now
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reservations(id, reference, restaurant_id, table_id, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc)
		VALUES(?, ?, ?, ?, ?, ?, 'confirmed', ?, ?, ?, ?)`,
		resID, ref, req.RestaurantID, req.TableID, req.UserID, req.PartySize, req.StartsAtLocal, startsAt.Unix(), endsAt.Unix(), createdAt.Unix())
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if e.audit != nil {
		_ = e.audit.LogEvent(ctx, "reservation", ref, "create", fmt.Sprintf("created reservation %s for table %s", ref, req.TableID))
	}

	return &contracts.Reservation{
		ID:            resID,
		Reference:     ref,
		RestaurantID:  req.RestaurantID,
		TableID:       req.TableID,
		UserID:        req.UserID,
		PartySize:     req.PartySize,
		Status:        "confirmed",
		StartsAtLocal: req.StartsAtLocal,
		StartsAt:      startsAt,
		EndsAt:        endsAt,
		CreatedAt:     createdAt,
	}, nil
}

func (e *Engine) GetReservation(ctx context.Context, reference string) (*contracts.Reservation, error) {
	row := e.store.DB().QueryRowContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.reference = ?`, reference)
	var r contracts.Reservation
	var startU, endU, createdU int64
	var tz string
	if err := row.Scan(&r.ID, &r.Reference, &r.RestaurantID, &r.TableID, &r.UserID, &r.PartySize, &r.Status, &r.StartsAtLocal, &startU, &endU, &createdU, &tz); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	loc, _ := time.LoadLocation(tz)
	if loc != nil {
		r.StartsAt = time.Unix(startU, 0).In(loc)
		r.EndsAt = time.Unix(endU, 0).In(loc)
	} else {
		r.StartsAt = time.Unix(startU, 0)
		r.EndsAt = time.Unix(endU, 0)
	}
	r.CreatedAt = time.Unix(createdU, 0).UTC()
	return &r, nil
}

func (e *Engine) ListReservations(ctx context.Context, userID string) ([]*contracts.Reservation, error) {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.user_id = ? ORDER BY r.starts_at_utc DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*contracts.Reservation
	for rows.Next() {
		var r contracts.Reservation
		var startU, endU, createdU int64
		var tz string
		if err := rows.Scan(&r.ID, &r.Reference, &r.RestaurantID, &r.TableID, &r.UserID, &r.PartySize, &r.Status, &r.StartsAtLocal, &startU, &endU, &createdU, &tz); err != nil {
			return nil, err
		}
		loc, _ := time.LoadLocation(tz)
		if loc != nil {
			r.StartsAt = time.Unix(startU, 0).In(loc)
			r.EndsAt = time.Unix(endU, 0).In(loc)
		} else {
			r.StartsAt = time.Unix(startU, 0)
			r.EndsAt = time.Unix(endU, 0)
		}
		r.CreatedAt = time.Unix(createdU, 0).UTC()
		list = append(list, &r)
	}
	return list, nil
}

func (e *Engine) CancelReservation(ctx context.Context, reference string, now time.Time) (*contracts.Reservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, err
	}

	if res.Status == "cancelled" {
		return res, nil // idempotent
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, err
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	if e.calendar.IsCutoffPassed(res.StartsAt, rest.CancellationCutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	_, err = e.store.DB().ExecContext(ctx, "UPDATE reservations SET status = 'cancelled' WHERE reference = ?", reference)
	if err != nil {
		return nil, err
	}

	res.Status = "cancelled"
	if e.audit != nil {
		_ = e.audit.LogEvent(ctx, "reservation", reference, "cancel", "cancelled reservation")
	}
	return res, nil
}

func (e *Engine) PatchReservation(ctx context.Context, reference string, patch contracts.PatchReservationParams, now time.Time) (*contracts.Reservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, err
	}

	if res.Status == "cancelled" {
		return nil, ErrReservationCancelled
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, err
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Cutoff checked against current reservation start time
	if e.calendar.IsCutoffPassed(res.StartsAt, rest.CancellationCutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	targetTableID := res.TableID
	if patch.TableID != nil {
		targetTableID = *patch.TableID
	}

	targetPartySize := res.PartySize
	if patch.PartySize != nil {
		targetPartySize = *patch.PartySize
		if targetPartySize < 1 {
			return nil, ErrValidationFailed
		}
	}

	// Find target table
	var table *contracts.Table
	for i := range rest.Tables {
		if rest.Tables[i].ID == targetTableID {
			table = &rest.Tables[i]
			break
		}
	}
	if table == nil {
		return nil, ErrNotFound
	}

	if targetPartySize > table.Capacity {
		return nil, ErrPartyExceedsCapacity
	}

	targetStartsAt := res.StartsAt
	targetStartsAtLocal := res.StartsAtLocal
	if patch.StartsAtLocal != nil {
		targetStartsAtLocal = *patch.StartsAtLocal
		parsed, err := e.calendar.ParseLocalTime(rest.Timezone, targetStartsAtLocal)
		if err != nil {
			if errors.Is(err, calendar.ErrInvalidLocalTime) {
				return nil, ErrInvalidLocalTime
			}
			return nil, ErrValidationFailed
		}
		if !e.calendar.IsWithinOpeningHours(rest, parsed) {
			return nil, ErrOutsideOpeningHours
		}
		if !e.calendar.IsOnSlotGrid(rest, parsed) {
			return nil, ErrNotOnSlotGrid
		}
		targetStartsAt = parsed
	}

	duration := time.Duration(rest.ReservationDurationMinutes) * time.Minute
	targetEndsAt := targetStartsAt.Add(duration)

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check table availability excluding current reservation
	var overlapCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations 
		WHERE restaurant_id = ? AND table_id = ? AND status = 'confirmed' AND id != ?
		  AND starts_at_utc < ? AND ends_at_utc > ?`,
		res.RestaurantID, targetTableID, res.ID, targetEndsAt.Unix(), targetStartsAt.Unix()).Scan(&overlapCount)
	if err != nil {
		return nil, err
	}
	if overlapCount > 0 {
		return nil, ErrTableUnavailable
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE reservations SET table_id = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?
		WHERE id = ?`,
		targetTableID, targetPartySize, targetStartsAtLocal, targetStartsAt.Unix(), targetEndsAt.Unix(), res.ID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	res.TableID = targetTableID
	res.PartySize = targetPartySize
	res.StartsAtLocal = targetStartsAtLocal
	res.StartsAt = targetStartsAt
	res.EndsAt = targetEndsAt

	if e.audit != nil {
		_ = e.audit.LogEvent(ctx, "reservation", reference, "patch", fmt.Sprintf("updated reservation table=%s party=%d", targetTableID, targetPartySize))
	}
	return res, nil
}

func (e *Engine) MoveReservations(ctx context.Context, userID string, moves []contracts.ReservationMoveRequest, now time.Time) ([]*contracts.Reservation, error) {
	if len(moves) == 0 || len(moves) > 8 {
		return nil, ErrValidationFailed
	}

	// Distinct references check
	refSet := make(map[string]bool)
	for _, m := range moves {
		if m.Reference == "" || refSet[m.Reference] {
			return nil, ErrValidationFailed
		}
		refSet[m.Reference] = true
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Load existing reservations and verify ownership and single restaurant
	var restID string
	var existing []*contracts.Reservation
	for _, m := range moves {
		res, err := e.GetReservation(ctx, m.Reference)
		if err != nil {
			return nil, ErrNotFound
		}
		if res.UserID != userID {
			return nil, ErrNotFound
		}
		if res.Status == "cancelled" {
			return nil, ErrReservationCancelled
		}
		if restID == "" {
			restID = res.RestaurantID
		} else if restID != res.RestaurantID {
			return nil, ErrValidationFailed
		}
		existing = append(existing, res)
	}

	rest, err := e.store.GetRestaurant(ctx, restID)
	if err != nil {
		return nil, ErrNotFound
	}

	// Calculate target values for each move
	type targetState struct {
		res      *contracts.Reservation
		tableID  string
		startsAt time.Time
		endsAt   time.Time
		localStr string
		party    int
	}

	var targets []targetState
	movedIDs := make(map[string]bool)

	for i, m := range moves {
		cur := existing[i]
		movedIDs[cur.ID] = true

		// Cutoff check on current booking
		if e.calendar.IsCutoffPassed(cur.StartsAt, rest.CancellationCutoffMinutes, now) {
			return nil, ErrCutoffPassed
		}

		tblID := cur.TableID
		if m.TableID != nil {
			tblID = *m.TableID
		}

		party := cur.PartySize
		if m.PartySize != nil {
			party = *m.PartySize
			if party < 1 {
				return nil, ErrValidationFailed
			}
		}

		var tbl *contracts.Table
		for j := range rest.Tables {
			if rest.Tables[j].ID == tblID {
				tbl = &rest.Tables[j]
				break
			}
		}
		if tbl == nil {
			return nil, ErrNotFound
		}
		if party > tbl.Capacity {
			return nil, ErrPartyExceedsCapacity
		}

		st := cur.StartsAt
		stLocal := cur.StartsAtLocal
		if m.StartsAtLocal != nil {
			stLocal = *m.StartsAtLocal
			parsed, err := e.calendar.ParseLocalTime(rest.Timezone, stLocal)
			if err != nil {
				if errors.Is(err, calendar.ErrInvalidLocalTime) {
					return nil, ErrInvalidLocalTime
				}
				return nil, ErrValidationFailed
			}
			if !e.calendar.IsWithinOpeningHours(rest, parsed) {
				return nil, ErrOutsideOpeningHours
			}
			if !e.calendar.IsOnSlotGrid(rest, parsed) {
				return nil, ErrNotOnSlotGrid
			}
			st = parsed
		}

		duration := time.Duration(rest.ReservationDurationMinutes) * time.Minute
		end := st.Add(duration)

		targets = append(targets, targetState{
			res:      cur,
			tableID:  tblID,
			startsAt: st,
			endsAt:   end,
			localStr: stLocal,
			party:    party,
		})
	}

	// Mutual collision check among moved reservations
	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			if targets[i].tableID == targets[j].tableID {
				// Overlap condition: startA < endB && startB < endA
				if targets[i].startsAt.Before(targets[j].endsAt) && targets[j].startsAt.Before(targets[i].endsAt) {
					return nil, ErrTableUnavailable
				}
			}
		}
	}

	// Transaction to update all moves atomically and check collision with unlisted reservations
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check each target against database unlisted reservations
	for _, tgt := range targets {
		var overlapCount int
		queryArgs := make([]interface{}, 0, 2+len(existing)+2)
		queryArgs = append(queryArgs, rest.ID, tgt.tableID)
		for _, r := range existing {
			queryArgs = append(queryArgs, r.ID)
		}
		queryArgs = append(queryArgs, tgt.endsAt.Unix(), tgt.startsAt.Unix())

		err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM reservations
			WHERE restaurant_id = ? AND table_id = ? AND status = 'confirmed' AND id NOT IN (`+strings.Repeat("?,", len(existing)-1)+`?)
			  AND starts_at_utc < ? AND ends_at_utc > ?`,
			queryArgs...).Scan(&overlapCount)
		if err != nil {
			return nil, err
		}
		if overlapCount > 0 {
			return nil, ErrTableUnavailable
		}
	}

	// Apply all updates
	var results []*contracts.Reservation
	for _, tgt := range targets {
		_, err := tx.ExecContext(ctx, `
			UPDATE reservations SET table_id = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?
			WHERE id = ?`,
			tgt.tableID, tgt.party, tgt.localStr, tgt.startsAt.Unix(), tgt.endsAt.Unix(), tgt.res.ID)
		if err != nil {
			return nil, err
		}

		resCopy := *tgt.res
		resCopy.TableID = tgt.tableID
		resCopy.PartySize = tgt.party
		resCopy.StartsAtLocal = tgt.localStr
		resCopy.StartsAt = tgt.startsAt
		resCopy.EndsAt = tgt.endsAt
		results = append(results, &resCopy)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return results, nil
}

func expandIDs(list []*contracts.Reservation) []interface{} {
	ids := make([]interface{}, len(list))
	for i, r := range list {
		ids[i] = r.ID
	}
	return ids
}

func (e *Engine) GetAvailability(ctx context.Context, restaurantID string, dateStr string, partySize int) (*contracts.AvailabilityResult, error) {
	if partySize < 1 {
		return nil, ErrValidationFailed
	}

	rest, err := e.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	slots, err := e.calendar.GenerateSlots(rest, dateStr)
	if err != nil {
		if errors.Is(err, calendar.ErrValidationFailed) {
			return nil, ErrValidationFailed
		}
		return nil, err
	}

	// If closed day, slots is empty
	if len(slots) == 0 {
		return &contracts.AvailabilityResult{
			RestaurantID: restaurantID,
			Date:         dateStr,
			Timezone:     rest.Timezone,
			Slots:        []contracts.AvailabilitySlot{},
		}, nil
	}

	// Query confirmed reservations for the day in this restaurant
	// Load all confirmed reservations that could overlap
	firstSlotStart := slots[0].StartsAt.Unix()
	lastSlotEnd := slots[len(slots)-1].EndsAt.Unix()

	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT table_id, starts_at_utc, ends_at_utc FROM reservations
		WHERE restaurant_id = ? AND status = 'confirmed'
		  AND starts_at_utc < ? AND ends_at_utc > ?`,
		restaurantID, lastSlotEnd, firstSlotStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type resInterval struct {
		tableID string
		start   int64
		end     int64
	}
	var resList []resInterval
	for rows.Next() {
		var ri resInterval
		if err := rows.Scan(&ri.tableID, &ri.start, &ri.end); err != nil {
			return nil, err
		}
		resList = append(resList, ri)
	}

	var availabilitySlots []contracts.AvailabilitySlot
	for _, s := range slots {
		slotStartU := s.StartsAt.Unix()
		slotEndU := s.EndsAt.Unix()

		var availableTables []string
		// Must iterate in fixture order!
		for _, t := range rest.Tables {
			if t.Capacity < partySize {
				continue
			}

			// Check overlap
			hasOverlap := false
			for _, r := range resList {
				if r.tableID == t.ID {
					if r.start < slotEndU && slotStartU < r.end {
						hasOverlap = true
						break
					}
				}
			}

			if !hasOverlap {
				availableTables = append(availableTables, t.ID)
			}
		}

		if availableTables == nil {
			availableTables = []string{}
		}

		availabilitySlots = append(availabilitySlots, contracts.AvailabilitySlot{
			StartsAtLocal:     s.StartsAtLocal,
			StartsAt:          e.calendar.FormatRFC3339(s.StartsAt),
			AvailableTableIDs: availableTables,
		})
	}

	return &contracts.AvailabilityResult{
		RestaurantID: restaurantID,
		Date:         dateStr,
		Timezone:     rest.Timezone,
		Slots:        availabilitySlots,
	}, nil
}
