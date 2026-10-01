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
	ErrForbidden             = errors.New("forbidden")
	ErrTableUnavailable      = errors.New("table_unavailable")
	ErrCutoffPassed          = errors.New("cutoff_passed")
	ErrReservationCancelled  = errors.New("reservation_cancelled")
	ErrNotOnSlotGrid         = errors.New("not_on_slot_grid")
	ErrOutsideOpeningHours   = errors.New("outside_opening_hours")
	ErrPartyExceedsCapacity  = errors.New("party_exceeds_capacity")
	ErrInvalidLocalTime      = errors.New("invalid_local_time")
	ErrValidationFailed      = errors.New("validation_failed")
	ErrCombinationNotAllowed = errors.New("combination_not_allowed")
	ErrStaleRevision         = errors.New("stale_revision")
	ErrAlreadyInSeries       = errors.New("already_in_series")
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

func (e *Engine) PublishPolicy(ctx context.Context, restaurantID string, userID string, p *contracts.Policy) (*contracts.Policy, error) {
	rest, err := e.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	isMgr, err := e.store.IsManager(ctx, restaurantID, userID)
	if err != nil || !isMgr {
		return nil, ErrForbidden
	}

	// Validate policy fields
	if p.SlotMinutes < 1 || p.SlotMinutes > 1440 {
		return nil, ErrValidationFailed
	}
	if p.ReservationDurationMinutes < 1 || p.ReservationDurationMinutes > 1440 {
		return nil, ErrValidationFailed
	}
	if p.CancellationCutoffMinutes < 0 || p.CancellationCutoffMinutes > 10080 {
		return nil, ErrValidationFailed
	}
	if len(p.EffectiveFrom) != 10 {
		return nil, ErrValidationFailed
	}
	if _, err := time.Parse("2006-01-02", p.EffectiveFrom); err != nil {
		return nil, ErrValidationFailed
	}

	// Validate opening hours (no duplicate weekdays)
	seenWeekdays := make(map[string]bool)
	for _, h := range p.OpeningHours {
		wLower := strings.ToLower(h.Weekday)
		if seenWeekdays[wLower] {
			return nil, ErrValidationFailed
		}
		seenWeekdays[wLower] = true
		if h.Opens >= h.Closes {
			return nil, ErrValidationFailed
		}
	}

	// Capacities must name exactly the restaurant's table ids
	if len(p.Capacities) != len(rest.Tables) {
		return nil, ErrValidationFailed
	}
	for _, t := range rest.Tables {
		capVal, ok := p.Capacities[t.ID]
		if !ok || capVal < 1 || capVal > 100 {
			return nil, ErrValidationFailed
		}
	}

	return e.store.PublishPolicy(ctx, restaurantID, p)
}

func (e *Engine) ListPolicies(ctx context.Context, restaurantID string) ([]contracts.Policy, error) {
	_, err := e.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, ErrNotFound
	}
	return e.store.ListPolicies(ctx, restaurantID)
}

func (e *Engine) CreateReservation(ctx context.Context, req contracts.CreateReservationParams) (*contracts.Reservation, error) {
	if req.PartySize < 1 {
		return nil, ErrValidationFailed
	}

	// Normalize table inputs
	if req.TableID != "" && len(req.TableIDs) > 0 {
		return nil, ErrValidationFailed
	}
	if req.TableID != "" {
		req.TableIDs = []string{req.TableID}
	}
	if len(req.TableIDs) == 0 {
		return nil, ErrValidationFailed
	}

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

	if len(req.TableIDs) == 1 {
		if _, ok := tableMap[req.TableIDs[0]]; !ok {
			return nil, ErrNotFound
		}
	} else {
		canonicalPair, ok := findCombinablePair(rest, req.TableIDs[0], req.TableIDs[1])
		if !ok {
			return nil, ErrCombinationNotAllowed
		}
		req.TableIDs = canonicalPair
	}

	startsAt, err := e.calendar.ParseLocalTime(rest.Timezone, req.StartsAtLocal)
	if err != nil {
		if errors.Is(err, calendar.ErrInvalidLocalTime) {
			return nil, ErrInvalidLocalTime
		}
		return nil, ErrValidationFailed
	}

	dateStr := req.StartsAtLocal[:10]
	pol, err := e.store.GetEffectivePolicy(ctx, req.RestaurantID, dateStr)
	if err != nil {
		return nil, err
	}

	if !e.calendar.IsWithinOpeningHoursForPolicy(pol.OpeningHours, pol.ReservationDurationMinutes, startsAt) {
		return nil, ErrOutsideOpeningHours
	}
	if !e.calendar.IsOnSlotGridForPolicy(rest.Timezone, pol.SlotMinutes, pol.ReservationDurationMinutes, pol.OpeningHours, startsAt) {
		return nil, ErrNotOnSlotGrid
	}

	// Capacity check against selected policy
	var combinedCapacity int
	if len(req.TableIDs) == 1 {
		combinedCapacity = pol.Capacities[req.TableIDs[0]]
	} else {
		combinedCapacity = pol.Capacities[req.TableIDs[0]] + pol.Capacities[req.TableIDs[1]]
	}
	if req.PartySize > combinedCapacity {
		return nil, ErrPartyExceedsCapacity
	}

	endsAt := startsAt.Add(time.Duration(pol.ReservationDurationMinutes) * time.Minute)

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Collision check
	queryPlaceholders := strings.Repeat("?,", len(req.TableIDs)-1) + "?"
	args := make([]interface{}, 0, 1+len(req.TableIDs)+2)
	args = append(args, rest.ID)
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

	resID, err := generateID("res")
	if err != nil {
		return nil, err
	}
	ref, err := generateReference()
	if err != nil {
		return nil, err
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var singleTID *string
	if len(req.TableIDs) == 1 {
		singleTID = &req.TableIDs[0]
	}
	rawTableIDsJSON, _ := json.Marshal(req.TableIDs)

	terms := contracts.AcceptedTerms{
		PolicyVersion:              pol.PolicyVersion,
		SlotMinutes:                pol.SlotMinutes,
		ReservationDurationMinutes: pol.ReservationDurationMinutes,
		CancellationCutoffMinutes:  pol.CancellationCutoffMinutes,
		OpeningHours:               pol.OpeningHours,
		Capacities:                 pol.Capacities,
	}
	termsJSON, _ := json.Marshal(terms)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reservations(id, reference, restaurant_id, table_id, table_ids_json, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		resID, ref, rest.ID, singleTID, string(rawTableIDsJSON), req.UserID, req.PartySize, "confirmed", req.StartsAtLocal, startsAt.Unix(), endsAt.Unix(), now.Unix(), 1, string(termsJSON))
	if err != nil {
		return nil, err
	}

	for idx, tid := range req.TableIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, resID, tid, idx); err != nil {
			return nil, err
		}
	}

	// Initial history event
	var changes []contracts.HistoryChange
	if len(req.TableIDs) == 1 {
		changes = []contracts.HistoryChange{
			{Field: "table_id", From: nil, To: req.TableIDs[0]},
			{Field: "starts_at_local", From: nil, To: req.StartsAtLocal},
			{Field: "party_size", From: nil, To: req.PartySize},
		}
	} else {
		changes = []contracts.HistoryChange{
			{Field: "table_ids", From: nil, To: req.TableIDs},
			{Field: "starts_at_local", From: nil, To: req.StartsAtLocal},
			{Field: "party_size", From: nil, To: req.PartySize},
		}
	}
	changesJSON, _ := json.Marshal(changes)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		resID, 1, now.Unix(), "created", string(changesJSON), 1, string(termsJSON))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	res := &contracts.Reservation{
		ID:            resID,
		Reference:     ref,
		RestaurantID:  rest.ID,
		TableIDs:      req.TableIDs,
		TableID:       singleTID,
		UserID:        req.UserID,
		PartySize:     req.PartySize,
		Status:        "confirmed",
		StartsAtLocal: req.StartsAtLocal,
		Revision:      1,
		AcceptedTerms: &terms,
		StartsAtUTC:   startsAt.Unix(),
		EndsAtUTC:     endsAt.Unix(),
		CreatedAtUTC:  now.Unix(),
		StartsAt:      startsAt,
		EndsAt:        endsAt,
		CreatedAt:     now,
	}

	return res, nil
}

func (e *Engine) GetReservation(ctx context.Context, reference string) (*contracts.Reservation, error) {
	row := e.store.DB().QueryRowContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.table_ids_json, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, r.revision, r.accepted_terms_json, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.reference = ?`, reference)

	var res contracts.Reservation
	var sU, eU, cU int64
	var tidNull sql.NullString
	var tidsJSON string
	var termsJSON sql.NullString
	var tz string

	err := row.Scan(&res.ID, &res.Reference, &res.RestaurantID, &tidNull, &tidsJSON, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU, &res.Revision, &termsJSON, &tz)
	if err != nil {
		return nil, ErrNotFound
	}

	if tidNull.Valid {
		res.TableID = &tidNull.String
	}
	_ = json.Unmarshal([]byte(tidsJSON), &res.TableIDs)
	if len(res.TableIDs) == 1 {
		res.TableID = &res.TableIDs[0]
	} else {
		res.TableID = nil
	}

	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}
	res.StartsAt = time.Unix(sU, 0).In(loc)
	res.EndsAt = time.Unix(eU, 0).In(loc)
	res.CreatedAt = time.Unix(cU, 0).UTC()
	res.StartsAtUTC = sU
	res.EndsAtUTC = eU
	res.CreatedAtUTC = cU

	if res.Revision == 0 {
		res.Revision = 1
	}

	if termsJSON.Valid && termsJSON.String != "" {
		var terms contracts.AcceptedTerms
		if err := json.Unmarshal([]byte(termsJSON.String), &terms); err == nil {
			res.AcceptedTerms = &terms
		}
	}
	if res.AcceptedTerms == nil {
		rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
		if err == nil {
			t := store.BuildPolicyZero(rest)
			res.AcceptedTerms = &t
		}
	}

	return &res, nil
}

func (e *Engine) GetDecision(ctx context.Context, reference string, userID string) (*contracts.DecisionResponse, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, ErrNotFound
	}

	if userID == "" || res.UserID != userID {
		return nil, ErrNotFound
	}

	return &contracts.DecisionResponse{
		Reference:     res.Reference,
		Revision:      res.Revision,
		AcceptedTerms: *res.AcceptedTerms,
	}, nil
}

func (e *Engine) GetHistory(ctx context.Context, reference string, userID string) (*contracts.HistoryResponse, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, ErrNotFound
	}

	if userID == "" || res.UserID != userID {
		return nil, ErrNotFound
	}

	entries, err := e.store.GetReservationHistory(ctx, res.ID)
	if err != nil {
		return nil, err
	}

	return &contracts.HistoryResponse{
		Reference: res.Reference,
		Entries:   entries,
	}, nil
}

func (e *Engine) ListReservations(ctx context.Context, userID string) ([]*contracts.Reservation, error) {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.table_ids_json, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, r.revision, r.accepted_terms_json, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.user_id = ?
		ORDER BY r.starts_at_utc DESC`, userID)
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
		var termsJSON sql.NullString
		var tz string

		if err := rows.Scan(&res.ID, &res.Reference, &res.RestaurantID, &tidNull, &tidsJSON, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU, &res.Revision, &termsJSON, &tz); err != nil {
			return nil, err
		}

		if tidNull.Valid {
			res.TableID = &tidNull.String
		}
		_ = json.Unmarshal([]byte(tidsJSON), &res.TableIDs)
		if len(res.TableIDs) == 1 {
			res.TableID = &res.TableIDs[0]
		} else {
			res.TableID = nil
		}

		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		res.StartsAt = time.Unix(sU, 0).In(loc)
		res.EndsAt = time.Unix(eU, 0).In(loc)
		res.CreatedAt = time.Unix(cU, 0).UTC()
		res.StartsAtUTC = sU
		res.EndsAtUTC = eU
		res.CreatedAtUTC = cU

		if res.Revision == 0 {
			res.Revision = 1
		}
		if termsJSON.Valid && termsJSON.String != "" {
			var terms contracts.AcceptedTerms
			if err := json.Unmarshal([]byte(termsJSON.String), &terms); err == nil {
				res.AcceptedTerms = &terms
			}
		}

		list = append(list, &res)
	}

	return list, nil
}

func (e *Engine) CancelReservation(ctx context.Context, reference string, now time.Time) (*contracts.Reservation, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, ErrNotFound
	}

	// Repeated cancel is idempotent 200 OK without revision increment or new history
	if res.Status == "cancelled" {
		return res, nil
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Check cutoff against OLD accepted cutoff
	cutoffMinutes := res.AcceptedTerms.CancellationCutoffMinutes
	if e.calendar.IsCutoffPassed(res.StartsAt, cutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	newRevision := res.Revision + 1

	if _, err := tx.ExecContext(ctx, "UPDATE reservations SET status = 'cancelled', revision = ? WHERE id = ?", newRevision, res.ID); err != nil {
		return nil, err
	}

	// Remove from reservation_tables so slot is immediately available
	if _, err := tx.ExecContext(ctx, "DELETE FROM reservation_tables WHERE reservation_id = ?", res.ID); err != nil {
		return nil, err
	}

	// History entry for cancellation
	if err := e.store.AppendHistory(ctx, tx, res.ID, now.Unix(), "cancelled", []contracts.HistoryChange{}, newRevision, *res.AcceptedTerms); err != nil {
		return nil, err
	}

	// If part of series, increment series revision
	var seriesID string
	if err := tx.QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", res.ID).Scan(&seriesID); err == nil {
		if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", seriesID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	res.Status = "cancelled"
	res.Revision = newRevision
	return res, nil
}

func (e *Engine) PatchReservation(ctx context.Context, reference string, patch contracts.PatchReservationParams, now time.Time) (*contracts.Reservation, error) {
	res, err := e.GetReservation(ctx, reference)
	if err != nil {
		return nil, ErrNotFound
	}

	if res.Status == "cancelled" {
		return nil, ErrReservationCancelled
	}

	// Validate expected_revision
	if patch.ExpectedRevision != nil {
		if *patch.ExpectedRevision <= 0 {
			return nil, ErrValidationFailed
		}
		if *patch.ExpectedRevision != res.Revision {
			return nil, ErrStaleRevision
		}
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Check old accepted cutoff first
	cutoffMinutes := res.AcceptedTerms.CancellationCutoffMinutes
	if e.calendar.IsCutoffPassed(res.StartsAt, cutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	// Target tables
	targetTableIDs := res.TableIDs
	if patch.TableID != nil && len(patch.TableIDs) > 0 {
		return nil, ErrValidationFailed
	}
	if patch.TableID != nil {
		if *patch.TableID == "" {
			return nil, ErrValidationFailed
		}
		targetTableIDs = []string{*patch.TableID}
	} else if len(patch.TableIDs) > 0 {
		targetTableIDs = patch.TableIDs
	}

	// Normalize target tables
	if len(targetTableIDs) == 0 || len(targetTableIDs) > 2 {
		return nil, ErrCombinationNotAllowed
	}
	seen := make(map[string]bool)
	for _, tid := range targetTableIDs {
		if seen[tid] {
			return nil, ErrValidationFailed
		}
		seen[tid] = true
	}

	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	if len(targetTableIDs) == 1 {
		if _, ok := tableMap[targetTableIDs[0]]; !ok {
			return nil, ErrNotFound
		}
	} else {
		canonicalPair, ok := findCombinablePair(rest, targetTableIDs[0], targetTableIDs[1])
		if !ok {
			return nil, ErrCombinationNotAllowed
		}
		targetTableIDs = canonicalPair
	}

	// Target party size
	targetParty := res.PartySize
	if patch.PartySize != nil {
		if *patch.PartySize < 1 {
			return nil, ErrValidationFailed
		}
		targetParty = *patch.PartySize
	}

	// Target starts at
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
		targetStartsAt = parsed
		targetStartsAtLocal = *patch.StartsAtLocal
	}

	// Check if this is a NO-OP
	isTableSame := sameTableSets(res.TableIDs, targetTableIDs)
	isTimeSame := res.StartsAtLocal == targetStartsAtLocal
	isPartySame := res.PartySize == targetParty

	if isTableSame && isTimeSame && isPartySame {
		// No-op amendment: retains terms, end time, revision; records NO history
		return res, nil
	}

	// Real change: validate against policy applicable to resulting start date
	targetDate := targetStartsAtLocal[:10]
	targetPol, err := e.store.GetEffectivePolicy(ctx, res.RestaurantID, targetDate)
	if err != nil {
		return nil, err
	}

	if !e.calendar.IsWithinOpeningHoursForPolicy(targetPol.OpeningHours, targetPol.ReservationDurationMinutes, targetStartsAt) {
		return nil, ErrOutsideOpeningHours
	}
	if !e.calendar.IsOnSlotGridForPolicy(rest.Timezone, targetPol.SlotMinutes, targetPol.ReservationDurationMinutes, targetPol.OpeningHours, targetStartsAt) {
		return nil, ErrNotOnSlotGrid
	}

	var combinedCap int
	if len(targetTableIDs) == 1 {
		combinedCap = targetPol.Capacities[targetTableIDs[0]]
	} else {
		combinedCap = targetPol.Capacities[targetTableIDs[0]] + targetPol.Capacities[targetTableIDs[1]]
	}
	if targetParty > combinedCap {
		return nil, ErrPartyExceedsCapacity
	}

	targetEndsAt := targetStartsAt.Add(time.Duration(targetPol.ReservationDurationMinutes) * time.Minute)

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify revision again inside transaction if expected_revision was provided
	if patch.ExpectedRevision != nil {
		var curRev int
		if err := tx.QueryRowContext(ctx, "SELECT revision FROM reservations WHERE id = ?", res.ID).Scan(&curRev); err != nil {
			return nil, err
		}
		if curRev != *patch.ExpectedRevision {
			return nil, ErrStaleRevision
		}
	}

	// Collision check
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

	newRevision := res.Revision + 1
	newTerms := contracts.AcceptedTerms{
		PolicyVersion:              targetPol.PolicyVersion,
		SlotMinutes:                targetPol.SlotMinutes,
		ReservationDurationMinutes: targetPol.ReservationDurationMinutes,
		CancellationCutoffMinutes:  targetPol.CancellationCutoffMinutes,
		OpeningHours:               targetPol.OpeningHours,
		Capacities:                 targetPol.Capacities,
	}
	termsJSON, _ := json.Marshal(newTerms)

	var singleTID *string
	if len(targetTableIDs) == 1 {
		singleTID = &targetTableIDs[0]
	}
	rawTableIDsJSON, _ := json.Marshal(targetTableIDs)

	_, err = tx.ExecContext(ctx, `
		UPDATE reservations
		SET table_id = ?, table_ids_json = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?, revision = ?, accepted_terms_json = ?
		WHERE id = ?`,
		singleTID, string(rawTableIDsJSON), targetParty, targetStartsAtLocal, targetStartsAt.Unix(), targetEndsAt.Unix(), newRevision, string(termsJSON), res.ID)
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

	// History changes in order: table_id / table_ids, starts_at_local, party_size
	var changes []contracts.HistoryChange
	if !isTableSame {
		if len(res.TableIDs) == 1 && len(targetTableIDs) == 1 {
			changes = append(changes, contracts.HistoryChange{
				Field: "table_id",
				From:  res.TableIDs[0],
				To:    targetTableIDs[0],
			})
		} else {
			changes = append(changes, contracts.HistoryChange{
				Field: "table_ids",
				From:  res.TableIDs,
				To:    targetTableIDs,
			})
		}
	}
	if !isTimeSame {
		changes = append(changes, contracts.HistoryChange{
			Field: "starts_at_local",
			From:  res.StartsAtLocal,
			To:    targetStartsAtLocal,
		})
	}
	if !isPartySame {
		changes = append(changes, contracts.HistoryChange{
			Field: "party_size",
			From:  res.PartySize,
			To:    targetParty,
		})
	}

	if err := e.store.AppendHistory(ctx, tx, res.ID, now.Unix(), "changed", changes, newRevision, newTerms); err != nil {
		return nil, err
	}

	// If in series, mark exception and increment series revision
	var seriesID string
	if err := tx.QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", res.ID).Scan(&seriesID); err == nil {
		if _, err := tx.ExecContext(ctx, "UPDATE series_occurrences SET exception = 1 WHERE reservation_id = ?", res.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", seriesID); err != nil {
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
	res.Revision = newRevision
	res.AcceptedTerms = &newTerms

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

		if m.ExpectedRevision != nil {
			if *m.ExpectedRevision <= 0 {
				return nil, ErrValidationFailed
			}
			if *m.ExpectedRevision != cur.Revision {
				return nil, ErrStaleRevision
			}
		}

		// Cutoff check
		if e.calendar.IsCutoffPassed(cur.StartsAt, cur.AcceptedTerms.CancellationCutoffMinutes, now) {
			return nil, ErrCutoffPassed
		}

		existing = append(existing, cur)
	}

	rest, err := e.store.GetRestaurant(ctx, restID)
	if err != nil {
		return nil, ErrNotFound
	}

	tableMap := make(map[string]*contracts.Table)
	for i := range rest.Tables {
		tableMap[rest.Tables[i].ID] = &rest.Tables[i]
	}

	type plannedMove struct {
		res            *contracts.Reservation
		isNoOp         bool
		targetTableIDs []string
		targetParty    int
		targetStartsAt time.Time
		targetStartsLocal string
		targetEndsAt   time.Time
		targetPol      *contracts.Policy
	}

	var planned []plannedMove

	for i, m := range moves {
		cur := existing[i]

		targetTableIDs := cur.TableIDs
		if m.TableID != nil && len(m.TableIDs) > 0 {
			return nil, ErrValidationFailed
		}
		if m.TableID != nil {
			if *m.TableID == "" {
				return nil, ErrValidationFailed
			}
			targetTableIDs = []string{*m.TableID}
		} else if len(m.TableIDs) > 0 {
			targetTableIDs = m.TableIDs
		}

		if len(targetTableIDs) == 0 || len(targetTableIDs) > 2 {
			return nil, ErrCombinationNotAllowed
		}
		seenT := make(map[string]bool)
		for _, tid := range targetTableIDs {
			if seenT[tid] {
				return nil, ErrValidationFailed
			}
			seenT[tid] = true
		}

		if len(targetTableIDs) == 1 {
			if _, ok := tableMap[targetTableIDs[0]]; !ok {
				return nil, ErrNotFound
			}
		} else {
			canonicalPair, ok := findCombinablePair(rest, targetTableIDs[0], targetTableIDs[1])
			if !ok {
				return nil, ErrCombinationNotAllowed
			}
			targetTableIDs = canonicalPair
		}

		targetParty := cur.PartySize
		if m.PartySize != nil {
			if *m.PartySize < 1 {
				return nil, ErrValidationFailed
			}
			targetParty = *m.PartySize
		}

		targetStartsAt := cur.StartsAt
		targetStartsLocal := cur.StartsAtLocal
		if m.StartsAtLocal != nil {
			parsed, err := e.calendar.ParseLocalTime(rest.Timezone, *m.StartsAtLocal)
			if err != nil {
				if errors.Is(err, calendar.ErrInvalidLocalTime) {
					return nil, ErrInvalidLocalTime
				}
				return nil, ErrValidationFailed
			}
			targetStartsAt = parsed
			targetStartsLocal = *m.StartsAtLocal
		}

		isTableSame := sameTableSets(cur.TableIDs, targetTableIDs)
		isTimeSame := cur.StartsAtLocal == targetStartsLocal
		isPartySame := cur.PartySize == targetParty

		if isTableSame && isTimeSame && isPartySame {
			planned = append(planned, plannedMove{
				res:    cur,
				isNoOp: true,
			})
			continue
		}

		targetDate := targetStartsLocal[:10]
		targetPol, err := e.store.GetEffectivePolicy(ctx, restID, targetDate)
		if err != nil {
			return nil, err
		}

		if !e.calendar.IsWithinOpeningHoursForPolicy(targetPol.OpeningHours, targetPol.ReservationDurationMinutes, targetStartsAt) {
			return nil, ErrOutsideOpeningHours
		}
		if !e.calendar.IsOnSlotGridForPolicy(rest.Timezone, targetPol.SlotMinutes, targetPol.ReservationDurationMinutes, targetPol.OpeningHours, targetStartsAt) {
			return nil, ErrNotOnSlotGrid
		}

		var combinedCap int
		if len(targetTableIDs) == 1 {
			combinedCap = targetPol.Capacities[targetTableIDs[0]]
		} else {
			combinedCap = targetPol.Capacities[targetTableIDs[0]] + targetPol.Capacities[targetTableIDs[1]]
		}
		if targetParty > combinedCap {
			return nil, ErrPartyExceedsCapacity
		}

		targetEndsAt := targetStartsAt.Add(time.Duration(targetPol.ReservationDurationMinutes) * time.Minute)

		planned = append(planned, plannedMove{
			res:               cur,
			isNoOp:            false,
			targetTableIDs:    targetTableIDs,
			targetParty:       targetParty,
			targetStartsAt:    targetStartsAt,
			targetStartsLocal: targetStartsLocal,
			targetEndsAt:      targetEndsAt,
			targetPol:         targetPol,
		})
	}

	// Internal overlap check
	for i := 0; i < len(planned); i++ {
		pA := planned[i]
		tAIDs := pA.res.TableIDs
		sA := pA.res.StartsAt.Unix()
		eA := pA.res.EndsAt.Unix()
		if !pA.isNoOp {
			tAIDs = pA.targetTableIDs
			sA = pA.targetStartsAt.Unix()
			eA = pA.targetEndsAt.Unix()
		}

		for j := i + 1; j < len(planned); j++ {
			pB := planned[j]
			tBIDs := pB.res.TableIDs
			sB := pB.res.StartsAt.Unix()
			eB := pB.res.EndsAt.Unix()
			if !pB.isNoOp {
				tBIDs = pB.targetTableIDs
				sB = pB.targetStartsAt.Unix()
				eB = pB.targetEndsAt.Unix()
			}

			if sA < eB && sB < eA {
				for _, tidA := range tAIDs {
					for _, tidB := range tBIDs {
						if tidA == tidB {
							return nil, ErrTableUnavailable
						}
					}
				}
			}
		}
	}

	// DB transaction
	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var movedIDs []interface{}
	for _, p := range planned {
		movedIDs = append(movedIDs, p.res.ID)
	}
	idPlaceholders := strings.Repeat("?,", len(movedIDs)-1) + "?"

	// Check DB collisions for all modified moves
	for _, p := range planned {
		if p.isNoOp {
			continue
		}
		queryPlaceholders := strings.Repeat("?,", len(p.targetTableIDs)-1) + "?"
		args := make([]interface{}, 0, 1+len(p.targetTableIDs)+len(movedIDs)+2)
		args = append(args, restID)
		for _, tid := range p.targetTableIDs {
			args = append(args, tid)
		}
		args = append(args, movedIDs...)
		args = append(args, p.targetEndsAt.Unix(), p.targetStartsAt.Unix())

		var overlapCount int
		err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM reservations r
			JOIN reservation_tables rt ON r.id = rt.reservation_id
			WHERE r.restaurant_id = ? AND rt.table_id IN (`+queryPlaceholders+`)
			  AND r.id NOT IN (`+idPlaceholders+`) AND r.status = 'confirmed'
			  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
			args...).Scan(&overlapCount)
		if err != nil {
			return nil, err
		}
		if overlapCount > 0 {
			return nil, ErrTableUnavailable
		}
	}

	// Apply updates
	var results []*contracts.Reservation
	affectedSeries := make(map[string]bool)

	for _, p := range planned {
		if p.isNoOp {
			results = append(results, p.res)
			continue
		}

		newRev := p.res.Revision + 1
		newTerms := contracts.AcceptedTerms{
			PolicyVersion:              p.targetPol.PolicyVersion,
			SlotMinutes:                p.targetPol.SlotMinutes,
			ReservationDurationMinutes: p.targetPol.ReservationDurationMinutes,
			CancellationCutoffMinutes:  p.targetPol.CancellationCutoffMinutes,
			OpeningHours:               p.targetPol.OpeningHours,
			Capacities:                 p.targetPol.Capacities,
		}
		termsJSON, _ := json.Marshal(newTerms)

		var singleTID *string
		if len(p.targetTableIDs) == 1 {
			singleTID = &p.targetTableIDs[0]
		}
		rawTableIDsJSON, _ := json.Marshal(p.targetTableIDs)

		_, err = tx.ExecContext(ctx, `
			UPDATE reservations
			SET table_id = ?, table_ids_json = ?, party_size = ?, starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?, revision = ?, accepted_terms_json = ?
			WHERE id = ?`,
			singleTID, string(rawTableIDsJSON), p.targetParty, p.targetStartsLocal, p.targetStartsAt.Unix(), p.targetEndsAt.Unix(), newRev, string(termsJSON), p.res.ID)
		if err != nil {
			return nil, err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM reservation_tables WHERE reservation_id = ?`, p.res.ID); err != nil {
			return nil, err
		}
		for idx, tid := range p.targetTableIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, p.res.ID, tid, idx); err != nil {
				return nil, err
			}
		}

		// Changes
		var changes []contracts.HistoryChange
		if !sameTableSets(p.res.TableIDs, p.targetTableIDs) {
			if len(p.res.TableIDs) == 1 && len(p.targetTableIDs) == 1 {
				changes = append(changes, contracts.HistoryChange{Field: "table_id", From: p.res.TableIDs[0], To: p.targetTableIDs[0]})
			} else {
				changes = append(changes, contracts.HistoryChange{Field: "table_ids", From: p.res.TableIDs, To: p.targetTableIDs})
			}
		}
		if p.res.StartsAtLocal != p.targetStartsLocal {
			changes = append(changes, contracts.HistoryChange{Field: "starts_at_local", From: p.res.StartsAtLocal, To: p.targetStartsLocal})
		}
		if p.res.PartySize != p.targetParty {
			changes = append(changes, contracts.HistoryChange{Field: "party_size", From: p.res.PartySize, To: p.targetParty})
		}

		if err := e.store.AppendHistory(ctx, tx, p.res.ID, now.Unix(), "changed", changes, newRev, newTerms); err != nil {
			return nil, err
		}

		// Check series
		var sID string
		if err := tx.QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", p.res.ID).Scan(&sID); err == nil {
			affectedSeries[sID] = true
			if _, err := tx.ExecContext(ctx, "UPDATE series_occurrences SET exception = 1 WHERE reservation_id = ?", p.res.ID); err != nil {
				return nil, err
			}
		}

		updated := *p.res
		updated.TableIDs = p.targetTableIDs
		updated.TableID = singleTID
		updated.PartySize = p.targetParty
		updated.StartsAtLocal = p.targetStartsLocal
		updated.StartsAt = p.targetStartsAt
		updated.EndsAt = p.targetEndsAt
		updated.StartsAtUTC = p.targetStartsAt.Unix()
		updated.EndsAtUTC = p.targetEndsAt.Unix()
		updated.Revision = newRev
		updated.AcceptedTerms = &newTerms

		results = append(results, &updated)
	}

	// Each affected series revision increases once
	for sID := range affectedSeries {
		if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", sID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return results, nil
}

func (e *Engine) CreateSeries(ctx context.Context, userID string, req contracts.CreateSeriesRequest, now time.Time) (*contracts.SeriesResponse, error) {
	if req.Count < 2 || req.Count > 12 {
		return nil, ErrValidationFailed
	}
	if req.IntervalWeeks < 1 || req.IntervalWeeks > 4 {
		return nil, ErrValidationFailed
	}

	anchor, err := e.GetReservation(ctx, req.AnchorReference)
	if err != nil {
		return nil, ErrNotFound
	}
	if anchor.UserID != userID {
		return nil, ErrNotFound
	}
	if anchor.Status == "cancelled" {
		return nil, ErrReservationCancelled
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Cutoff check for anchor
	if e.calendar.IsCutoffPassed(anchor.StartsAt, anchor.AcceptedTerms.CancellationCutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	// Already adopted check
	var existingSeriesID string
	err = e.store.DB().QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", anchor.ID).Scan(&existingSeriesID)
	if err == nil {
		return nil, ErrAlreadyInSeries
	}

	rest, err := e.store.GetRestaurant(ctx, anchor.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	anchorDate, err := time.Parse("2006-01-02", anchor.StartsAtLocal[:10])
	if err != nil {
		return nil, ErrValidationFailed
	}
	clockTimeStr := anchor.StartsAtLocal[11:16]

	type occSpec struct {
		startsAtLocal string
		startsAt      time.Time
		endsAt        time.Time
		pol           *contracts.Policy
	}
	var futureSpecs []occSpec

	for i := 1; i < req.Count; i++ {
		daysToAdd := i * req.IntervalWeeks * 7
		targetDate := anchorDate.AddDate(0, 0, daysToAdd).Format("2006-01-02")
		targetStartsAtLocal := fmt.Sprintf("%sT%s", targetDate, clockTimeStr)

		parsedTime, err := e.calendar.ParseLocalTime(rest.Timezone, targetStartsAtLocal)
		if err != nil {
			if errors.Is(err, calendar.ErrInvalidLocalTime) {
				return nil, ErrInvalidLocalTime
			}
			return nil, ErrValidationFailed
		}

		pol, err := e.store.GetEffectivePolicy(ctx, rest.ID, targetDate)
		if err != nil {
			return nil, err
		}

		if !e.calendar.IsWithinOpeningHoursForPolicy(pol.OpeningHours, pol.ReservationDurationMinutes, parsedTime) {
			return nil, ErrOutsideOpeningHours
		}
		if !e.calendar.IsOnSlotGridForPolicy(rest.Timezone, pol.SlotMinutes, pol.ReservationDurationMinutes, pol.OpeningHours, parsedTime) {
			return nil, ErrNotOnSlotGrid
		}

		// Capacity check
		var combinedCap int
		if len(anchor.TableIDs) == 1 {
			combinedCap = pol.Capacities[anchor.TableIDs[0]]
		} else {
			combinedCap = pol.Capacities[anchor.TableIDs[0]] + pol.Capacities[anchor.TableIDs[1]]
		}
		if anchor.PartySize > combinedCap {
			return nil, ErrPartyExceedsCapacity
		}

		endsAt := parsedTime.Add(time.Duration(pol.ReservationDurationMinutes) * time.Minute)

		futureSpecs = append(futureSpecs, occSpec{
			startsAtLocal: targetStartsAtLocal,
			startsAt:      parsedTime,
			endsAt:        endsAt,
			pol:           pol,
		})
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Recheck if anchor was adopted concurrently
	var checkID string
	if err := tx.QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", anchor.ID).Scan(&checkID); err == nil {
		return nil, ErrAlreadyInSeries
	}

	seriesID, err := generateID("ser")
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO series(id, restaurant_id, owner_user_id, revision, interval_weeks, created_at_utc)
		VALUES(?, ?, ?, 1, ?, ?)`,
		seriesID, rest.ID, userID, req.IntervalWeeks, now.Unix()); err != nil {
		return nil, err
	}

	// Occurrence 0 is the anchor
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO series_occurrences(series_id, idx, reservation_id, reference, exception)
		VALUES(?, 0, ?, ?, 0)`,
		seriesID, anchor.ID, anchor.Reference); err != nil {
		return nil, err
	}

	occurrences := []contracts.SeriesOccurrence{
		{
			Index:       0,
			Reference:   anchor.Reference,
			Exception:   false,
			Reservation: *anchor,
		},
	}

	// Generate occurrences 1..count-1
	for i, spec := range futureSpecs {
		idx := i + 1

		// Collision check on table(s)
		queryPlaceholders := strings.Repeat("?,", len(anchor.TableIDs)-1) + "?"
		args := make([]interface{}, 0, 1+len(anchor.TableIDs)+2)
		args = append(args, rest.ID)
		for _, tid := range anchor.TableIDs {
			args = append(args, tid)
		}
		args = append(args, spec.endsAt.Unix(), spec.startsAt.Unix())

		var overlapCount int
		err := tx.QueryRowContext(ctx, `
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

		resID, err := generateID("res")
		if err != nil {
			return nil, err
		}
		ref, err := generateReference()
		if err != nil {
			return nil, err
		}

		terms := contracts.AcceptedTerms{
			PolicyVersion:              spec.pol.PolicyVersion,
			SlotMinutes:                spec.pol.SlotMinutes,
			ReservationDurationMinutes: spec.pol.ReservationDurationMinutes,
			CancellationCutoffMinutes:  spec.pol.CancellationCutoffMinutes,
			OpeningHours:               spec.pol.OpeningHours,
			Capacities:                 spec.pol.Capacities,
		}
		termsJSON, _ := json.Marshal(terms)

		var singleTID *string
		if len(anchor.TableIDs) == 1 {
			singleTID = &anchor.TableIDs[0]
		}
		rawTableIDsJSON, _ := json.Marshal(anchor.TableIDs)

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reservations(id, reference, restaurant_id, table_id, table_ids_json, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc, revision, accepted_terms_json)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			resID, ref, rest.ID, singleTID, string(rawTableIDsJSON), userID, anchor.PartySize, "confirmed", spec.startsAtLocal, spec.startsAt.Unix(), spec.endsAt.Unix(), now.Unix(), string(termsJSON)); err != nil {
			return nil, err
		}

		for sIdx, tid := range anchor.TableIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, resID, tid, sIdx); err != nil {
				return nil, err
			}
		}

		var changes []contracts.HistoryChange
		if len(anchor.TableIDs) == 1 {
			changes = []contracts.HistoryChange{
				{Field: "table_id", From: nil, To: anchor.TableIDs[0]},
				{Field: "starts_at_local", From: nil, To: spec.startsAtLocal},
				{Field: "party_size", From: nil, To: anchor.PartySize},
			}
		} else {
			changes = []contracts.HistoryChange{
				{Field: "table_ids", From: nil, To: anchor.TableIDs},
				{Field: "starts_at_local", From: nil, To: spec.startsAtLocal},
				{Field: "party_size", From: nil, To: anchor.PartySize},
			}
		}
		changesJSON, _ := json.Marshal(changes)

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json)
			VALUES(?, 1, ?, 'created', ?, 1, ?)`,
			resID, now.Unix(), string(changesJSON), string(termsJSON)); err != nil {
			return nil, err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO series_occurrences(series_id, idx, reservation_id, reference, exception)
			VALUES(?, ?, ?, ?, 0)`,
			seriesID, idx, resID, ref); err != nil {
			return nil, err
		}

		occRes := contracts.Reservation{
			ID:            resID,
			Reference:     ref,
			RestaurantID:  rest.ID,
			TableIDs:      anchor.TableIDs,
			TableID:       singleTID,
			UserID:        userID,
			PartySize:     anchor.PartySize,
			Status:        "confirmed",
			StartsAtLocal: spec.startsAtLocal,
			Revision:      1,
			AcceptedTerms: &terms,
			StartsAtUTC:   spec.startsAt.Unix(),
			EndsAtUTC:     spec.endsAt.Unix(),
			CreatedAtUTC:  now.Unix(),
			StartsAt:      spec.startsAt,
			EndsAt:        spec.endsAt,
			CreatedAt:     now,
		}

		occurrences = append(occurrences, contracts.SeriesOccurrence{
			Index:       idx,
			Reference:   ref,
			Exception:   false,
			Reservation: occRes,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &contracts.SeriesResponse{
		SeriesID:      seriesID,
		Revision:      1,
		IntervalWeeks: req.IntervalWeeks,
		Occurrences:   occurrences,
	}, nil
}

func (e *Engine) GetSeries(ctx context.Context, seriesID string, userID string) (*contracts.SeriesResponse, error) {
	row := e.store.DB().QueryRowContext(ctx, `SELECT id, restaurant_id, owner_user_id, revision, interval_weeks FROM series WHERE id = ?`, seriesID)
	var resp contracts.SeriesResponse
	var restID, ownerUID string
	if err := row.Scan(&resp.SeriesID, &restID, &ownerUID, &resp.Revision, &resp.IntervalWeeks); err != nil {
		return nil, ErrNotFound
	}

	if userID == "" || ownerUID != userID {
		return nil, ErrNotFound
	}

	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT idx, reference, exception
		FROM series_occurrences
		WHERE series_id = ?
		ORDER BY idx ASC`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var occ contracts.SeriesOccurrence
		var excInt int
		if err := rows.Scan(&occ.Index, &occ.Reference, &excInt); err != nil {
			return nil, err
		}
		occ.Exception = (excInt == 1)

		curRes, err := e.GetReservation(ctx, occ.Reference)
		if err == nil {
			occ.Reservation = *curRes
		}
		resp.Occurrences = append(resp.Occurrences, occ)
	}

	return &resp, nil
}

func (e *Engine) GetAvailability(ctx context.Context, restaurantID string, dateStr string, partySize int, explain bool) (*contracts.AvailabilityResult, error) {
	if partySize < 1 {
		return nil, ErrValidationFailed
	}

	rest, err := e.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	pol, err := e.store.GetEffectivePolicy(ctx, restaurantID, dateStr)
	if err != nil {
		return nil, err
	}

	slots, err := e.calendar.GenerateSlotsForPolicy(rest.Timezone, pol.SlotMinutes, pol.ReservationDurationMinutes, pol.OpeningHours, dateStr)
	if err != nil {
		if errors.Is(err, calendar.ErrValidationFailed) {
			return nil, ErrValidationFailed
		}
		return nil, err
	}

	if len(slots) == 0 {
		return &contracts.AvailabilityResult{
			RestaurantID: restaurantID,
			Date:         dateStr,
			Timezone:     rest.Timezone,
			Slots:        []contracts.AvailabilitySlot{},
		}, nil
	}

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

	var availabilitySlots []contracts.AvailabilitySlot
	for _, s := range slots {
		slotStartU := s.StartsAt.Unix()
		slotEndU := s.EndsAt.Unix()

		availableTableIDs := []string{}
		availableOptions := []contracts.AvailabilityOption{}
		explains := []contracts.TableExplain{}

		// Fixture order for tables
		for _, t := range rest.Tables {
			tCap := pol.Capacities[t.ID]
			capHolds := partySize <= tCap

			hasOverlap := false
			for _, r := range resList {
				if r.tableID == t.ID && r.start < slotEndU && slotStartU < r.end {
					hasOverlap = true
					break
				}
			}
			overlapHolds := !hasOverlap

			tableAvailable := capHolds && overlapHolds
			if tableAvailable {
				availableTableIDs = append(availableTableIDs, t.ID)
				availableOptions = append(availableOptions, contracts.AvailabilityOption{
					TableIDs: []string{t.ID},
					Capacity: tCap,
				})
			}

			if explain {
				explains = append(explains, contracts.TableExplain{
					TableID:       t.ID,
					PolicyVersion: pol.PolicyVersion,
					Available:     tableAvailable,
					Rules: []contracts.ExplainRule{
						{Rule: "capacity", Holds: capHolds},
						{Rule: "no_overlap", Holds: overlapHolds},
					},
				})
			}
		}

		// Combinable pairs in declared combinable order
		for _, pair := range rest.Combinable {
			if len(pair) != 2 {
				continue
			}
			capA := pol.Capacities[pair[0]]
			capB := pol.Capacities[pair[1]]
			combinedCap := capA + capB

			if combinedCap < partySize {
				continue
			}

			hasOverlapA := false
			hasOverlapB := false
			for _, r := range resList {
				if r.start < slotEndU && slotStartU < r.end {
					if r.tableID == pair[0] {
						hasOverlapA = true
					}
					if r.tableID == pair[1] {
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

		slotItem := contracts.AvailabilitySlot{
			StartsAtLocal:     s.StartsAtLocal,
			StartsAt:          e.calendar.FormatRFC3339(s.StartsAt),
			AvailableTableIDs: availableTableIDs,
			AvailableOptions:  availableOptions,
		}
		if explain {
			slotItem.Explain = explains
		}

		availabilitySlots = append(availabilitySlots, slotItem)
	}

	return &contracts.AvailabilityResult{
		RestaurantID: restaurantID,
		Date:         dateStr,
		Timezone:     rest.Timezone,
		Slots:        availabilitySlots,
	}, nil
}

func sameTableSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int)
	for _, id := range a {
		counts[id]++
	}
	for _, id := range b {
		counts[id]--
		if counts[id] < 0 {
			return false
		}
	}
	return true
}
