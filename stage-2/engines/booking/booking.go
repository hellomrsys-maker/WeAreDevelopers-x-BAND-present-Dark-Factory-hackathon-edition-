package booking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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
	ErrNotFound              = errors.New("not_found")
	ErrTableUnavailable      = errors.New("table_unavailable")
	ErrCutoffPassed          = errors.New("cutoff_passed")
	ErrReservationCancelled  = errors.New("reservation_cancelled")
	ErrNotOnSlotGrid         = errors.New("not_on_slot_grid")
	ErrOutsideOpeningHours   = errors.New("outside_opening_hours")
	ErrPartyExceedsCapacity  = errors.New("party_exceeds_capacity")
	ErrInvalidLocalTime      = errors.New("invalid_local_time")
	ErrValidationFailed      = errors.New("validation_failed")
	ErrCombinationNotAllowed = errors.New("combination_not_allowed")
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

// Generate unique 6-8 character uppercase alphanumeric reference
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

// findCombinablePair checks if t1 and t2 form an allowed combinable pair in the restaurant.
// Returns the pair in the restaurant's declared combinable order.
func findCombinablePair(rest *contracts.Restaurant, t1, t2 string) ([]string, bool) {
	for _, pair := range rest.Combinable {
		if len(pair) == 2 {
			if (pair[0] == t1 && pair[1] == t2) || (pair[0] == t2 && pair[1] == t1) {
				return []string{pair[0], pair[1]}, true
			}
		}
	}
	return nil, false
}

func (e *Engine) CreateReservation(ctx context.Context, req contracts.CreateReservationParams) (*contracts.Reservation, error) {
	if req.PartySize < 1 {
		return nil, ErrValidationFailed
	}

	// Normalize table inputs: must have either table_id or table_ids, not both
	if req.TableID != "" && len(req.TableIDs) > 0 {
		return nil, ErrValidationFailed
	}
	if req.TableID != "" {
		req.TableIDs = []string{req.TableID}
	}
	if len(req.TableIDs) == 0 {
		return nil, ErrValidationFailed
	}

	// Check for duplicates in table_ids
	seen := make(map[string]bool)
	for _, tid := range req.TableIDs {
		if seen[tid] {
			return nil, ErrValidationFailed
		}
		seen[tid] = true
	}

	if len(req.TableIDs) > 2 {
		return nil, ErrCombinationNotAllowed
	}

	rest, err := e.store.GetRestaurant(ctx, req.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	// Map of tables for quick lookup
	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	var combinedCapacity int
	if len(req.TableIDs) == 1 {
		t, ok := tableMap[req.TableIDs[0]]
		if !ok {
			return nil, ErrNotFound
		}
		combinedCapacity = t.Capacity
	} else { // len == 2
		canonicalPair, ok := findCombinablePair(rest, req.TableIDs[0], req.TableIDs[1])
		if !ok {
			return nil, ErrCombinationNotAllowed
		}
		req.TableIDs = canonicalPair // Use combinable order

		tA, okA := tableMap[req.TableIDs[0]]
		tB, okB := tableMap[req.TableIDs[1]]
		if !okA || !okB {
			return nil, ErrNotFound
		}
		combinedCapacity = tA.Capacity + tB.Capacity
	}

	// Check party capacity
	if req.PartySize > combinedCapacity {
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
	// Any table in the set overlapping a confirmed reservation triggers table_unavailable
	queryPlaceholders := strings.Repeat("?,", len(req.TableIDs)-1) + "?"
	args := make([]interface{}, 0, 1+len(req.TableIDs)+2)
	args = append(args, req.RestaurantID)
	for _, tid := range req.TableIDs {
		args = append(args, tid)
	}
	args = append(args, endsAt.Unix(), startsAt.Unix())

	var overlapCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations r
		JOIN reservation_tables rt ON r.id = rt.reservation_id
		WHERE r.restaurant_id = ? AND rt.table_id IN (`+queryPlaceholders+`) AND r.status = 'confirmed'
		  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
		args...).Scan(&overlapCount)
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

	nowUTC := req.Now.UTC()
	if nowUTC.IsZero() {
		nowUTC = time.Now().UTC()
	}

	var singleTID *string
	if len(req.TableIDs) == 1 {
		singleTID = &req.TableIDs[0]
	}
	rawTableIDsJSON, _ := json.Marshal(req.TableIDs)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reservations(id, reference, restaurant_id, table_id, table_ids_json, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc)
		VALUES(?, ?, ?, ?, ?, ?, ?, 'confirmed', ?, ?, ?, ?)`,
		resID, ref, req.RestaurantID, singleTID, string(rawTableIDsJSON), req.UserID, req.PartySize, req.StartsAtLocal, startsAt.Unix(), endsAt.Unix(), nowUTC.Unix())
	if err != nil {
		return nil, err
	}

	for idx, tid := range req.TableIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, resID, tid, idx); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	res := &contracts.Reservation{
		ID:            resID,
		Reference:     ref,
		RestaurantID:  req.RestaurantID,
		TableIDs:      req.TableIDs,
		TableID:       singleTID,
		UserID:        req.UserID,
		PartySize:     req.PartySize,
		Status:        "confirmed",
		StartsAtLocal: req.StartsAtLocal,
		StartsAtUTC:   startsAt.Unix(),
		EndsAtUTC:     endsAt.Unix(),
		CreatedAtUTC:  nowUTC.Unix(),
		StartsAt:      startsAt,
		EndsAt:        endsAt,
		CreatedAt:     nowUTC,
	}

	return res, nil
}

func (e *Engine) GetReservation(ctx context.Context, reference string) (*contracts.Reservation, error) {
	row := e.store.DB().QueryRowContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.table_ids_json, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.reference = ? OR r.id = ?`, reference, reference)

	var res contracts.Reservation
	var sU, eU, cU int64
	var tidNull sql.NullString
	var tidsJSON string
	var tz string

	err := row.Scan(&res.ID, &res.Reference, &res.RestaurantID, &tidNull, &tidsJSON, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU, &tz)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	_ = json.Unmarshal([]byte(tidsJSON), &res.TableIDs)
	if len(res.TableIDs) == 1 {
		res.TableID = &res.TableIDs[0]
	} else {
		res.TableID = nil
	}

	loc, _ := time.LoadLocation(tz)
	if loc != nil {
		res.StartsAt = time.Unix(sU, 0).In(loc)
		res.EndsAt = time.Unix(eU, 0).In(loc)
	} else {
		res.StartsAt = time.Unix(sU, 0)
		res.EndsAt = time.Unix(eU, 0)
	}
	res.CreatedAt = time.Unix(cU, 0).UTC()
	res.StartsAtUTC = sU
	res.EndsAtUTC = eU
	res.CreatedAtUTC = cU

	return &res, nil
}

func (e *Engine) ListReservations(ctx context.Context, userID string) ([]*contracts.Reservation, error) {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.table_ids_json, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.user_id = ? ORDER BY r.starts_at_utc DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*contracts.Reservation
	for rows.Next() {
		var res contracts.Reservation
		var sU, eU, cU int64
		var tidNull sql.NullString
		var tidsJSON string
		var tz string

		err := rows.Scan(&res.ID, &res.Reference, &res.RestaurantID, &tidNull, &tidsJSON, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU, &tz)
		if err != nil {
			return nil, err
		}

		_ = json.Unmarshal([]byte(tidsJSON), &res.TableIDs)
		if len(res.TableIDs) == 1 {
			res.TableID = &res.TableIDs[0]
		} else {
			res.TableID = nil
		}

		loc, _ := time.LoadLocation(tz)
		if loc != nil {
			res.StartsAt = time.Unix(sU, 0).In(loc)
			res.EndsAt = time.Unix(eU, 0).In(loc)
		} else {
			res.StartsAt = time.Unix(sU, 0)
			res.EndsAt = time.Unix(eU, 0)
		}
		res.CreatedAt = time.Unix(cU, 0).UTC()
		res.StartsAtUTC = sU
		res.EndsAtUTC = eU
		res.CreatedAtUTC = cU

		list = append(list, &res)
	}
	return list, nil
}

func (e *Engine) CancelReservation(ctx context.Context, reference string, now time.Time) (*contracts.Reservation, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, err
	}

	if res.Status == "cancelled" {
		return res, nil // idempotent
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	cutoff := time.Duration(rest.CancellationCutoffMinutes) * time.Minute
	cutoffDeadline := res.StartsAt.Add(-cutoff)
	if now.After(cutoffDeadline) {
		return nil, ErrCutoffPassed
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, err = e.store.DB().ExecContext(ctx, "UPDATE reservations SET status = 'cancelled' WHERE id = ?", res.ID)
	if err != nil {
		return nil, err
	}

	res.Status = "cancelled"
	return res, nil
}

func (e *Engine) PatchReservation(ctx context.Context, reference string, patch contracts.PatchReservationParams, now time.Time) (*contracts.Reservation, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, err
	}

	if res.Status == "cancelled" {
		return nil, ErrReservationCancelled
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Cutoff check
	cutoff := time.Duration(rest.CancellationCutoffMinutes) * time.Minute
	cutoffDeadline := res.StartsAt.Add(-cutoff)
	if now.After(cutoffDeadline) {
		return nil, ErrCutoffPassed
	}

	// Normalize table inputs
	if patch.TableID != nil && len(patch.TableIDs) > 0 {
		return nil, ErrValidationFailed
	}

	targetTableIDs := res.TableIDs
	if patch.TableID != nil {
		targetTableIDs = []string{*patch.TableID}
	} else if len(patch.TableIDs) > 0 {
		targetTableIDs = patch.TableIDs
	}

	// Check duplicates
	seen := make(map[string]bool)
	for _, tid := range targetTableIDs {
		if seen[tid] {
			return nil, ErrValidationFailed
		}
		seen[tid] = true
	}

	if len(targetTableIDs) > 2 {
		return nil, ErrCombinationNotAllowed
	}

	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	var combinedCapacity int
	if len(targetTableIDs) == 1 {
		t, ok := tableMap[targetTableIDs[0]]
		if !ok {
			return nil, ErrNotFound
		}
		combinedCapacity = t.Capacity
	} else { // len == 2
		canonicalPair, ok := findCombinablePair(rest, targetTableIDs[0], targetTableIDs[1])
		if !ok {
			return nil, ErrCombinationNotAllowed
		}
		targetTableIDs = canonicalPair

		tA, okA := tableMap[targetTableIDs[0]]
		tB, okB := tableMap[targetTableIDs[1]]
		if !okA || !okB {
			return nil, ErrNotFound
		}
		combinedCapacity = tA.Capacity + tB.Capacity
	}

	targetParty := res.PartySize
	if patch.PartySize != nil {
		if *patch.PartySize < 1 {
			return nil, ErrValidationFailed
		}
		targetParty = *patch.PartySize
	}

	if targetParty > combinedCapacity {
		return nil, ErrPartyExceedsCapacity
	}

	targetStartsAt := res.StartsAt
	targetStartsAtLocal := res.StartsAtLocal
	if patch.StartsAtLocal != nil {
		parsed, err := e.calendar.ParseLocalTime(rest.Timezone, *patch.StartsAtLocal)
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
		targetStartsAtLocal = *patch.StartsAtLocal
	}

	targetEndsAt := targetStartsAt.Add(time.Duration(rest.ReservationDurationMinutes) * time.Minute)

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Collision check excluding current reservation
	queryPlaceholders := strings.Repeat("?,", len(targetTableIDs)-1) + "?"
	args := make([]interface{}, 0, 1+len(targetTableIDs)+3)
	args = append(args, rest.ID)
	for _, tid := range targetTableIDs {
		args = append(args, tid)
	}
	args = append(args, res.ID, targetEndsAt.Unix(), targetStartsAt.Unix())

	var overlapCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations r
		JOIN reservation_tables rt ON r.id = rt.reservation_id
		WHERE r.restaurant_id = ? AND rt.table_id IN (`+queryPlaceholders+`) AND r.status = 'confirmed' AND r.id != ?
		  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
		args...).Scan(&overlapCount)
	if err != nil {
		return nil, err
	}
	if overlapCount > 0 {
		return nil, ErrTableUnavailable
	}

	var singleTID *string
	if len(targetTableIDs) == 1 {
		singleTID = &targetTableIDs[0]
	}
	rawTableIDsJSON, _ := json.Marshal(targetTableIDs)

	_, err = tx.ExecContext(ctx, `
		UPDATE reservations
		SET table_id = ?, table_ids_json = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?
		WHERE id = ?`,
		singleTID, string(rawTableIDsJSON), targetParty, targetStartsAtLocal, targetStartsAt.Unix(), targetEndsAt.Unix(), res.ID)
	if err != nil {
		return nil, err
	}

	// Update reservation_tables
	if _, err := tx.ExecContext(ctx, `DELETE FROM reservation_tables WHERE reservation_id = ?`, res.ID); err != nil {
		return nil, err
	}
	for idx, tid := range targetTableIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, res.ID, tid, idx); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	res.TableIDs = targetTableIDs
	res.TableID = singleTID
	res.PartySize = targetParty
	res.StartsAtLocal = targetStartsAtLocal
	res.StartsAt = targetStartsAt
	res.EndsAt = targetEndsAt
	res.StartsAtUTC = targetStartsAt.Unix()
	res.EndsAtUTC = targetEndsAt.Unix()

	return res, nil
}

func (e *Engine) MoveReservations(ctx context.Context, userID string, moves []contracts.ReservationMoveRequest, now time.Time) ([]*contracts.Reservation, error) {
	if len(moves) < 1 || len(moves) > 8 {
		return nil, ErrValidationFailed
	}

	refMap := make(map[string]bool)
	for _, m := range moves {
		if m.Reference == "" || refMap[m.Reference] {
			return nil, ErrValidationFailed
		}
		refMap[m.Reference] = true
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	var existing []*contracts.Reservation
	var restID string

	for _, m := range moves {
		cur, err := e.GetReservation(ctx, m.Reference)
		if err != nil {
			return nil, ErrNotFound
		}
		if cur.UserID != userID {
			return nil, ErrNotFound
		}
		if cur.Status == "cancelled" {
			return nil, ErrReservationCancelled
		}
		if restID == "" {
			restID = cur.RestaurantID
		} else if restID != cur.RestaurantID {
			return nil, ErrValidationFailed
		}
		existing = append(existing, cur)
	}

	rest, err := e.store.GetRestaurant(ctx, restID)
	if err != nil {
		return nil, ErrNotFound
	}

	cutoff := time.Duration(rest.CancellationCutoffMinutes) * time.Minute
	for _, cur := range existing {
		cutoffDeadline := cur.StartsAt.Add(-cutoff)
		if now.After(cutoffDeadline) {
			return nil, ErrCutoffPassed
		}
	}

	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	type targetState struct {
		res      *contracts.Reservation
		tableIDs []string
		startsAt time.Time
		endsAt   time.Time
		localStr string
		party    int
	}

	var targets []targetState

	for i, m := range moves {
		cur := existing[i]

		if m.TableID != nil && len(m.TableIDs) > 0 {
			return nil, ErrValidationFailed
		}

		tblIDs := cur.TableIDs
		if m.TableID != nil {
			tblIDs = []string{*m.TableID}
		} else if len(m.TableIDs) > 0 {
			tblIDs = m.TableIDs
		}

		seenT := make(map[string]bool)
		for _, tid := range tblIDs {
			if seenT[tid] {
				return nil, ErrValidationFailed
			}
			seenT[tid] = true
		}

		if len(tblIDs) > 2 {
			return nil, ErrCombinationNotAllowed
		}

		var combinedCap int
		if len(tblIDs) == 1 {
			t, ok := tableMap[tblIDs[0]]
			if !ok {
				return nil, ErrNotFound
			}
			combinedCap = t.Capacity
		} else { // len == 2
			canonicalPair, ok := findCombinablePair(rest, tblIDs[0], tblIDs[1])
			if !ok {
				return nil, ErrCombinationNotAllowed
			}
			tblIDs = canonicalPair

			tA, okA := tableMap[tblIDs[0]]
			tB, okB := tableMap[tblIDs[1]]
			if !okA || !okB {
				return nil, ErrNotFound
			}
			combinedCap = tA.Capacity + tB.Capacity
		}

		party := cur.PartySize
		if m.PartySize != nil {
			if *m.PartySize < 1 {
				return nil, ErrValidationFailed
			}
			party = *m.PartySize
		}
		if party > combinedCap {
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
			tableIDs: tblIDs,
			startsAt: st,
			endsAt:   end,
			localStr: stLocal,
			party:    party,
		})
	}

	// Mutual collision check among moved reservations:
	// If targets[i] and targets[j] share any table and their intervals overlap, fail!
	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			sharesTable := false
			for _, ti := range targets[i].tableIDs {
				for _, tj := range targets[j].tableIDs {
					if ti == tj {
						sharesTable = true
						break
					}
				}
				if sharesTable {
					break
				}
			}
			if sharesTable {
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

	// Check each target against unlisted database reservations
	for _, tgt := range targets {
		var overlapCount int
		placeholders := strings.Repeat("?,", len(tgt.tableIDs)-1) + "?"
		notInPlaceholders := strings.Repeat("?,", len(existing)-1) + "?"

		queryArgs := make([]interface{}, 0, 1+len(tgt.tableIDs)+len(existing)+2)
		queryArgs = append(queryArgs, rest.ID)
		for _, tid := range tgt.tableIDs {
			queryArgs = append(queryArgs, tid)
		}
		for _, r := range existing {
			queryArgs = append(queryArgs, r.ID)
		}
		queryArgs = append(queryArgs, tgt.endsAt.Unix(), tgt.startsAt.Unix())

		err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM reservations r
			JOIN reservation_tables rt ON r.id = rt.reservation_id
			WHERE r.restaurant_id = ? AND rt.table_id IN (`+placeholders+`) AND r.status = 'confirmed' AND r.id NOT IN (`+notInPlaceholders+`)
			  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
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
		var singleTID *string
		if len(tgt.tableIDs) == 1 {
			singleTID = &tgt.tableIDs[0]
		}
		rawTableIDsJSON, _ := json.Marshal(tgt.tableIDs)

		_, err := tx.ExecContext(ctx, `
			UPDATE reservations
			SET table_id = ?, table_ids_json = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?
			WHERE id = ?`,
			singleTID, string(rawTableIDsJSON), tgt.party, tgt.localStr, tgt.startsAt.Unix(), tgt.endsAt.Unix(), tgt.res.ID)
		if err != nil {
			return nil, err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM reservation_tables WHERE reservation_id = ?`, tgt.res.ID); err != nil {
			return nil, err
		}
		for idx, tid := range tgt.tableIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, tgt.res.ID, tid, idx); err != nil {
				return nil, err
			}
		}

		resCopy := *tgt.res
		resCopy.TableIDs = tgt.tableIDs
		resCopy.TableID = singleTID
		resCopy.PartySize = tgt.party
		resCopy.StartsAtLocal = tgt.localStr
		resCopy.StartsAt = tgt.startsAt
		resCopy.EndsAt = tgt.endsAt
		resCopy.StartsAtUTC = tgt.startsAt.Unix()
		resCopy.EndsAtUTC = tgt.endsAt.Unix()
		results = append(results, &resCopy)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return results, nil
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

	// Query confirmed reservations for the day in this restaurant via reservation_tables
	firstSlotStart := slots[0].StartsAt.Unix()
	lastSlotEnd := slots[len(slots)-1].EndsAt.Unix()

	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT rt.table_id, r.starts_at_utc, r.ends_at_utc
		FROM reservations r
		JOIN reservation_tables rt ON r.id = rt.reservation_id
		WHERE r.restaurant_id = ? AND r.status = 'confirmed'
		  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
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

	// Map tables for capacity lookups
	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	var availabilitySlots []contracts.AvailabilitySlot
	for _, s := range slots {
		slotStartU := s.StartsAt.Unix()
		slotEndU := s.EndsAt.Unix()

		// 1. Single tables in fixture order
		var availableTableIDs []string
		var availableOptions []contracts.AvailabilityOption

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
				availableTableIDs = append(availableTableIDs, t.ID)
				availableOptions = append(availableOptions, contracts.AvailabilityOption{
					TableIDs: []string{t.ID},
					Capacity: t.Capacity,
				})
			}
		}

		// 2. Combinable pairs in declared combinable order
		for _, pair := range rest.Combinable {
			if len(pair) != 2 {
				continue
			}
			tA, okA := tableMap[pair[0]]
			tB, okB := tableMap[pair[1]]
			if !okA || !okB {
				continue
			}

			combinedCap := tA.Capacity + tB.Capacity
			if combinedCap < partySize {
				continue
			}

			// Check overlap for table A and table B
			hasOverlapA := false
			hasOverlapB := false
			for _, r := range resList {
				if r.start < slotEndU && slotStartU < r.end {
					if r.tableID == tA.ID {
						hasOverlapA = true
					}
					if r.tableID == tB.ID {
						hasOverlapB = true
					}
				}
			}

			if !hasOverlapA && !hasOverlapB {
				availableOptions = append(availableOptions, contracts.AvailabilityOption{
					TableIDs: []string{pair[0], pair[1]},
					Capacity: combinedCap,
				})
			}
		}

		if availableTableIDs == nil {
			availableTableIDs = []string{}
		}
		if availableOptions == nil {
			availableOptions = []contracts.AvailabilityOption{}
		}

		availabilitySlots = append(availabilitySlots, contracts.AvailabilitySlot{
			StartsAtLocal:     s.StartsAtLocal,
			StartsAt:          e.calendar.FormatRFC3339(s.StartsAt),
			AvailableTableIDs: availableTableIDs,
			AvailableOptions:  availableOptions,
		})
	}

	return &contracts.AvailabilityResult{
		RestaurantID: restaurantID,
		Date:         dateStr,
		Timezone:     rest.Timezone,
		Slots:        availabilitySlots,
	}, nil
}
