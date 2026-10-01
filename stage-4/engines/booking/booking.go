package booking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
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
	ErrStalePlan             = errors.New("stale_plan")
	ErrPlanAlreadyApplied    = errors.New("plan_already_applied")
	ErrNoFeasiblePlan        = errors.New("no_feasible_plan")
	ErrPlanningLimit         = errors.New("planning_limit")
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

func (e *Engine) checkClosureOverlap(ctx context.Context, tx *sql.Tx, restaurantID string, tableIDs []string, startUTC, endUTC int64) (bool, error) {
	for _, tid := range tableIDs {
		var count int
		var err error
		if tx != nil {
			err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM table_closures WHERE restaurant_id = ? AND table_id = ? AND from_utc < ? AND to_utc > ?", restaurantID, tid, endUTC, startUTC).Scan(&count)
		} else {
			err = e.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM table_closures WHERE restaurant_id = ? AND table_id = ? AND from_utc < ? AND to_utc > ?", restaurantID, tid, endUTC, startUTC).Scan(&count)
		}
		if err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
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

	// Check table closures
	closed, err := e.checkClosureOverlap(ctx, tx, rest.ID, req.TableIDs, startsAt.Unix(), endsAt.Unix())
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrTableUnavailable
	}

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

	// Increment restaurant revision
	if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", rest.ID); err != nil {
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

	if res.Status == "cancelled" {
		return res, nil
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

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

	if _, err := tx.ExecContext(ctx, "DELETE FROM reservation_tables WHERE reservation_id = ?", res.ID); err != nil {
		return nil, err
	}

	if err := e.store.AppendHistory(ctx, tx, res.ID, now.Unix(), "cancelled", []contracts.HistoryChange{}, newRevision, *res.AcceptedTerms); err != nil {
		return nil, err
	}

	// Increment restaurant revision
	if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", res.RestaurantID); err != nil {
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

	cutoffMinutes := res.AcceptedTerms.CancellationCutoffMinutes
	if e.calendar.IsCutoffPassed(res.StartsAt, cutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

	rest, err := e.store.GetRestaurant(ctx, res.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

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

	targetParty := res.PartySize
	if patch.PartySize != nil {
		if *patch.PartySize < 1 {
			return nil, ErrValidationFailed
		}
		targetParty = *patch.PartySize
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
		targetStartsAt = parsed
		targetStartsAtLocal = *patch.StartsAtLocal
	}

	isTableSame := sameTableSets(res.TableIDs, targetTableIDs)
	isTimeSame := res.StartsAtLocal == targetStartsAtLocal
	isPartySame := res.PartySize == targetParty

	if isTableSame && isTimeSame && isPartySame {
		return res, nil
	}

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

	if patch.ExpectedRevision != nil {
		var curRev int
		if err := tx.QueryRowContext(ctx, "SELECT revision FROM reservations WHERE id = ?", res.ID).Scan(&curRev); err != nil {
			return nil, err
		}
		if curRev != *patch.ExpectedRevision {
			return nil, ErrStaleRevision
		}
	}

	// Check table closures
	closed, err := e.checkClosureOverlap(ctx, tx, rest.ID, targetTableIDs, targetStartsAt.Unix(), targetEndsAt.Unix())
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrTableUnavailable
	}

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

	if _, err := tx.ExecContext(ctx, `DELETE FROM reservation_tables WHERE reservation_id = ?`, res.ID); err != nil {
		return nil, err
	}
	for idx, tid := range targetTableIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`, res.ID, tid, idx); err != nil {
			return nil, err
		}
	}

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

	// Increment restaurant revision
	if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", rest.ID); err != nil {
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
		res               *contracts.Reservation
		isNoOp            bool
		targetTableIDs    []string
		targetParty       int
		targetStartsAt    time.Time
		targetStartsLocal string
		targetEndsAt      time.Time
		targetPol         *contracts.Policy
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

	// Check DB collisions and closures
	for _, p := range planned {
		if p.isNoOp {
			continue
		}
		closed, err := e.checkClosureOverlap(ctx, tx, restID, p.targetTableIDs, p.targetStartsAt.Unix(), p.targetEndsAt.Unix())
		if err != nil {
			return nil, err
		}
		if closed {
			return nil, ErrTableUnavailable
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
		err = tx.QueryRowContext(ctx, `
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

	var results []*contracts.Reservation
	affectedSeries := make(map[string]bool)
	hasRealChange := false

	for _, p := range planned {
		if p.isNoOp {
			results = append(results, p.res)
			continue
		}
		hasRealChange = true

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

	if hasRealChange {
		if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", restID); err != nil {
			return nil, err
		}
		for sID := range affectedSeries {
			if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", sID); err != nil {
				return nil, err
			}
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

	if e.calendar.IsCutoffPassed(anchor.StartsAt, anchor.AcceptedTerms.CancellationCutoffMinutes, now) {
		return nil, ErrCutoffPassed
	}

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

	for i, spec := range futureSpecs {
		idx := i + 1

		closed, err := e.checkClosureOverlap(ctx, tx, rest.ID, anchor.TableIDs, spec.startsAt.Unix(), spec.endsAt.Unix())
		if err != nil {
			return nil, err
		}
		if closed {
			return nil, ErrTableUnavailable
		}

		queryPlaceholders := strings.Repeat("?,", len(anchor.TableIDs)-1) + "?"
		args := make([]interface{}, 0, 1+len(anchor.TableIDs)+2)
		args = append(args, rest.ID)
		for _, tid := range anchor.TableIDs {
			args = append(args, tid)
		}
		args = append(args, spec.endsAt.Unix(), spec.startsAt.Unix())

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

	// Increment restaurant revision
	if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", rest.ID); err != nil {
		return nil, err
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

func (e *Engine) AmendSeries(ctx context.Context, seriesID string, userID string, req contracts.AmendSeriesRequest, now time.Time) (*contracts.SeriesResponse, error) {
	if req.ExpectedRevision <= 0 {
		return nil, ErrValidationFailed
	}
	if req.FromIndex < 0 {
		return nil, ErrValidationFailed
	}
	if len(req.LocalTime) != 5 || req.LocalTime[2] != ':' {
		return nil, ErrValidationFailed
	}

	series, err := e.GetSeries(ctx, seriesID, userID)
	if err != nil {
		return nil, ErrNotFound
	}

	if series.Revision != req.ExpectedRevision {
		return nil, ErrStaleRevision
	}
	if req.FromIndex >= len(series.Occurrences) {
		return nil, ErrValidationFailed
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	rest, err := e.store.GetRestaurant(ctx, series.Occurrences[0].Reservation.RestaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	type amendOcc struct {
		occIdx        int
		res           contracts.Reservation
		isNoOp        bool
		targetLocal   string
		targetTime    time.Time
		targetEndsAt  time.Time
		targetPolicy  *contracts.Policy
	}

	var toAmend []amendOcc
	for _, occ := range series.Occurrences {
		if occ.Index < req.FromIndex || occ.Reservation.Status == "cancelled" || occ.Exception {
			continue
		}

		targetDate := occ.Reservation.StartsAtLocal[:10]
		targetLocal := fmt.Sprintf("%sT%s", targetDate, req.LocalTime)

		if occ.Reservation.StartsAtLocal == targetLocal {
			toAmend = append(toAmend, amendOcc{
				occIdx: occ.Index,
				res:    occ.Reservation,
				isNoOp: true,
			})
			continue
		}

		// Cutoff check
		if e.calendar.IsCutoffPassed(occ.Reservation.StartsAt, occ.Reservation.AcceptedTerms.CancellationCutoffMinutes, now) {
			return nil, ErrCutoffPassed
		}

		parsedTime, err := e.calendar.ParseLocalTime(rest.Timezone, targetLocal)
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

		var combinedCap int
		if len(occ.Reservation.TableIDs) == 1 {
			combinedCap = pol.Capacities[occ.Reservation.TableIDs[0]]
		} else {
			combinedCap = pol.Capacities[occ.Reservation.TableIDs[0]] + pol.Capacities[occ.Reservation.TableIDs[1]]
		}
		if occ.Reservation.PartySize > combinedCap {
			return nil, ErrPartyExceedsCapacity
		}

		endsAt := parsedTime.Add(time.Duration(pol.ReservationDurationMinutes) * time.Minute)

		toAmend = append(toAmend, amendOcc{
			occIdx:       occ.Index,
			res:          occ.Reservation,
			isNoOp:       false,
			targetLocal:  targetLocal,
			targetTime:   parsedTime,
			targetEndsAt: endsAt,
			targetPolicy: pol,
		})
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Recheck revision inside transaction
	var curRev int
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM series WHERE id = ?", seriesID).Scan(&curRev); err != nil {
		return nil, err
	}
	if curRev != req.ExpectedRevision {
		return nil, ErrStaleRevision
	}

	var amendingIDs []interface{}
	for _, a := range toAmend {
		if !a.isNoOp {
			amendingIDs = append(amendingIDs, a.res.ID)
		}
	}

	// Collision check
	for _, a := range toAmend {
		if a.isNoOp {
			continue
		}
		closed, err := e.checkClosureOverlap(ctx, tx, rest.ID, a.res.TableIDs, a.targetTime.Unix(), a.targetEndsAt.Unix())
		if err != nil {
			return nil, err
		}
		if closed {
			return nil, ErrTableUnavailable
		}

		queryPlaceholders := strings.Repeat("?,", len(a.res.TableIDs)-1) + "?"
		args := make([]interface{}, 0, 1+len(a.res.TableIDs)+len(amendingIDs)+2)
		args = append(args, rest.ID)
		for _, tid := range a.res.TableIDs {
			args = append(args, tid)
		}
		if len(amendingIDs) > 0 {
			idPlaceholders := strings.Repeat("?,", len(amendingIDs)-1) + "?"
			args = append(args, amendingIDs...)
			args = append(args, a.targetEndsAt.Unix(), a.targetTime.Unix())
			var overlapCount int
			err = tx.QueryRowContext(ctx, `
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
		} else {
			args = append(args, a.res.ID, a.targetEndsAt.Unix(), a.targetTime.Unix())
			var overlapCount int
			err = tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM reservations r
				JOIN reservation_tables rt ON r.id = rt.reservation_id
				WHERE r.restaurant_id = ? AND rt.table_id IN (`+queryPlaceholders+`)
				  AND r.id != ? AND r.status = 'confirmed'
				  AND r.starts_at_utc < ? AND r.ends_at_utc > ?`,
				args...).Scan(&overlapCount)
			if err != nil {
				return nil, err
			}
			if overlapCount > 0 {
				return nil, ErrTableUnavailable
			}
		}
	}

	hasAnyChange := false
	for _, a := range toAmend {
		if a.isNoOp {
			continue
		}
		hasAnyChange = true
		newResRev := a.res.Revision + 1
		newTerms := contracts.AcceptedTerms{
			PolicyVersion:              a.targetPolicy.PolicyVersion,
			SlotMinutes:                a.targetPolicy.SlotMinutes,
			ReservationDurationMinutes: a.targetPolicy.ReservationDurationMinutes,
			CancellationCutoffMinutes:  a.targetPolicy.CancellationCutoffMinutes,
			OpeningHours:               a.targetPolicy.OpeningHours,
			Capacities:                 a.targetPolicy.Capacities,
		}
		termsJSON, _ := json.Marshal(newTerms)

		_, err = tx.ExecContext(ctx, `
			UPDATE reservations
			SET starts_at_local = ?, starts_at_utc = ?, ends_at_utc = ?, revision = ?, accepted_terms_json = ?
			WHERE id = ?`,
			a.targetLocal, a.targetTime.Unix(), a.targetEndsAt.Unix(), newResRev, string(termsJSON), a.res.ID)
		if err != nil {
			return nil, err
		}

		changes := []contracts.HistoryChange{
			{Field: "starts_at_local", From: a.res.StartsAtLocal, To: a.targetLocal},
		}
		if err := e.store.AppendHistory(ctx, tx, a.res.ID, now.Unix(), "changed", changes, newResRev, newTerms); err != nil {
			return nil, err
		}
	}

	if hasAnyChange {
		if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", seriesID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", rest.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return e.GetSeries(ctx, seriesID, userID)
}

func (e *Engine) CreateReplan(ctx context.Context, restaurantID string, userID string, req contracts.ReplanRequest) (*contracts.ReplanResponse, error) {
	rest, err := e.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, ErrNotFound
	}

	isMgr, err := e.store.IsManager(ctx, restaurantID, userID)
	if err != nil || !isMgr {
		return nil, ErrForbidden
	}

	// Verify table exists
	tableFound := false
	for _, t := range rest.Tables {
		if t.ID == req.TableID {
			tableFound = true
			break
		}
	}
	if !tableFound {
		return nil, ErrNotFound
	}

	fromTime, err1 := time.Parse(time.RFC3339, req.From)
	toTime, err2 := time.Parse(time.RFC3339, req.To)
	if err1 != nil || err2 != nil || !fromTime.Before(toTime) {
		return nil, ErrValidationFailed
	}

	fromUTC := fromTime.Unix()
	toUTC := toTime.Unix()

	// Query considered bookings: all confirmed bookings overlapping [fromUTC, toUTC)
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT id, reference, table_ids_json, user_id, party_size, starts_at_local, starts_at_utc, ends_at_utc, revision, accepted_terms_json
		FROM reservations
		WHERE restaurant_id = ? AND status = 'confirmed'
		  AND starts_at_utc < ? AND ends_at_utc > ?
		ORDER BY reference ASC`, restaurantID, toUTC, fromUTC)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type consideredBooking struct {
		res contracts.Reservation
	}
	var considered []consideredBooking

	for rows.Next() {
		var cb consideredBooking
		var tidsJSON, termsJSON string
		if err := rows.Scan(&cb.res.ID, &cb.res.Reference, &tidsJSON, &cb.res.UserID, &cb.res.PartySize, &cb.res.StartsAtLocal, &cb.res.StartsAtUTC, &cb.res.EndsAtUTC, &cb.res.Revision, &termsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tidsJSON), &cb.res.TableIDs)
		var terms contracts.AcceptedTerms
		_ = json.Unmarshal([]byte(termsJSON), &terms)
		cb.res.AcceptedTerms = &terms
		considered = append(considered, cb)
	}

	if len(considered) > 6 || len(rest.Tables) > 6 || len(rest.Combinable) > 4 {
		return nil, ErrPlanningLimit
	}

	// Build options: singles in fixture order, then pairs in declared order
	type tableOption struct {
		rank     int
		tableIDs []string
	}
	var options []tableOption
	rankIdx := 0
	for _, t := range rest.Tables {
		options = append(options, tableOption{
			rank:     rankIdx,
			tableIDs: []string{t.ID},
		})
		rankIdx++
	}
	for _, pair := range rest.Combinable {
		if len(pair) == 2 {
			options = append(options, tableOption{
				rank:     rankIdx,
				tableIDs: []string{pair[0], pair[1]},
			})
			rankIdx++
		}
	}

	// Query fixed bookings: confirmed bookings not in considered
	var consideredIDs []interface{}
	for _, cb := range considered {
		consideredIDs = append(consideredIDs, cb.res.ID)
	}

	var fixedRows *sql.Rows
	if len(consideredIDs) > 0 {
		ph := strings.Repeat("?,", len(consideredIDs)-1) + "?"
		args := append([]interface{}{restaurantID}, consideredIDs...)
		fixedRows, err = e.store.DB().QueryContext(ctx, `
			SELECT r.id, rt.table_id, r.starts_at_utc, r.ends_at_utc
			FROM reservations r
			JOIN reservation_tables rt ON r.id = rt.reservation_id
			WHERE r.restaurant_id = ? AND r.status = 'confirmed' AND r.id NOT IN (`+ph+`)`, args...)
	} else {
		fixedRows, err = e.store.DB().QueryContext(ctx, `
			SELECT r.id, rt.table_id, r.starts_at_utc, r.ends_at_utc
			FROM reservations r
			JOIN reservation_tables rt ON r.id = rt.reservation_id
			WHERE r.restaurant_id = ? AND r.status = 'confirmed'`, restaurantID)
	}
	if err != nil {
		return nil, err
	}
	defer fixedRows.Close()

	type fixedInterval struct {
		tableID string
		start   int64
		end     int64
	}
	var fixedList []fixedInterval
	for fixedRows.Next() {
		var fi fixedInterval
		var id string
		if err := fixedRows.Scan(&id, &fi.tableID, &fi.start, &fi.end); err == nil {
			fixedList = append(fixedList, fi)
		}
	}

	// Existing table closures
	cRows, err := e.store.DB().QueryContext(ctx, `SELECT table_id, from_utc, to_utc FROM table_closures WHERE restaurant_id = ?`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer cRows.Close()
	type closureInterval struct {
		tableID string
		from    int64
		to      int64
	}
	var closures []closureInterval
	for cRows.Next() {
		var ci closureInterval
		if err := cRows.Scan(&ci.tableID, &ci.from, &ci.to); err == nil {
			closures = append(closures, ci)
		}
	}

	// Zero considered bookings trivial solution
	if len(considered) == 0 {
		planID, _ := generateID("plan")
		resp := &contracts.ReplanResponse{
			PlanID:             planID,
			RestaurantRevision: rest.Revision,
			Closure: contracts.Closure{
				TableID: req.TableID,
				From:    req.From,
				To:      req.To,
			},
			Assignments: []contracts.Assignment{},
			MovedCount:  0,
			UnusedSeats: 0,
		}
		_ = e.store.SaveReplan(ctx, resp, restaurantID, fromUTC, toUTC, req.From, req.To)
		return resp, nil
	}

	// Combinatorial solver
	bestFound := false
	var bestMoved int
	var bestUnused int
	var bestRanks []int
	var bestAssignments []contracts.Assignment

	currentAssignment := make([]int, len(considered))

	var solve func(idx int)
	solve = func(idx int) {
		if idx == len(considered) {
			// Compute objective
			moved := 0
			unused := 0
			ranks := make([]int, len(considered))
			var assigns []contracts.Assignment

			for i := range considered {
				opt := options[currentAssignment[i]]
				ranks[i] = opt.rank
				changed := !sameTableSets(considered[i].res.TableIDs, opt.tableIDs)
				if changed {
					moved++
				}

				// Compute capacity under accepted terms
				var capVal int
				for _, tid := range opt.tableIDs {
					capVal += considered[i].res.AcceptedTerms.Capacities[tid]
				}
				unused += (capVal - considered[i].res.PartySize)

				assigns = append(assigns, contracts.Assignment{
					Reference: considered[i].res.Reference,
					TableIDs:  opt.tableIDs,
					Changed:   changed,
				})
			}

			// Compare with best
			isBetter := false
			if !bestFound {
				isBetter = true
			} else if moved < bestMoved {
				isBetter = true
			} else if moved == bestMoved {
				if unused < bestUnused {
					isBetter = true
				} else if unused == bestUnused {
					for k := 0; k < len(ranks); k++ {
						if ranks[k] < bestRanks[k] {
							isBetter = true
							break
						} else if ranks[k] > bestRanks[k] {
							break
						}
					}
				}
			}

			if isBetter {
				bestFound = true
				bestMoved = moved
				bestUnused = unused
				bestRanks = ranks
				bestAssignments = assigns
			}
			return
		}

		cb := considered[idx]

		for optIdx, opt := range options {
			// 1. Capacity check
			var capVal int
			for _, tid := range opt.tableIDs {
				capVal += cb.res.AcceptedTerms.Capacities[tid]
			}
			if capVal < cb.res.PartySize {
				continue
			}

			// 2. Proposed closure check
			conflictProposed := false
			for _, tid := range opt.tableIDs {
				if tid == req.TableID {
					if cb.res.StartsAtUTC < toUTC && fromUTC < cb.res.EndsAtUTC {
						conflictProposed = true
						break
					}
				}
			}
			if conflictProposed {
				continue
			}

			// 3. Existing closures check
			conflictClosure := false
			for _, tid := range opt.tableIDs {
				for _, cl := range closures {
					if cl.tableID == tid && cb.res.StartsAtUTC < cl.to && cl.from < cb.res.EndsAtUTC {
						conflictClosure = true
						break
					}
				}
				if conflictClosure {
					break
				}
			}
			if conflictClosure {
				continue
			}

			// 4. Fixed bookings check
			conflictFixed := false
			for _, tid := range opt.tableIDs {
				for _, fb := range fixedList {
					if fb.tableID == tid && cb.res.StartsAtUTC < fb.end && fb.start < cb.res.EndsAtUTC {
						conflictFixed = true
						break
					}
				}
				if conflictFixed {
					break
				}
			}
			if conflictFixed {
				continue
			}

			// 5. Check against already assigned considered bookings
			conflictEarlier := false
			for prev := 0; prev < idx; prev++ {
				prevOpt := options[currentAssignment[prev]]
				prevCb := considered[prev]

				if cb.res.StartsAtUTC < prevCb.res.EndsAtUTC && prevCb.res.StartsAtUTC < cb.res.EndsAtUTC {
					for _, tidA := range opt.tableIDs {
						for _, tidB := range prevOpt.tableIDs {
							if tidA == tidB {
								conflictEarlier = true
								break
							}
						}
						if conflictEarlier {
							break
						}
					}
				}
				if conflictEarlier {
					break
				}
			}
			if conflictEarlier {
				continue
			}

			currentAssignment[idx] = optIdx
			solve(idx + 1)
		}
	}

	solve(0)

	if !bestFound {
		return nil, ErrNoFeasiblePlan
	}

	planID, err := generateID("plan")
	if err != nil {
		return nil, err
	}

	resp := &contracts.ReplanResponse{
		PlanID:             planID,
		RestaurantRevision: rest.Revision,
		Closure: contracts.Closure{
			TableID: req.TableID,
			From:    req.From,
			To:      req.To,
		},
		Assignments: bestAssignments,
		MovedCount:  bestMoved,
		UnusedSeats: bestUnused,
	}

	if err := e.store.SaveReplan(ctx, resp, restaurantID, fromUTC, toUTC, req.From, req.To); err != nil {
		return nil, err
	}

	return resp, nil
}

func (e *Engine) ApplyReplan(ctx context.Context, restaurantID string, planID string, userID string, idempotencyKey string, now time.Time) (*contracts.ApplyPlanResponse, error) {
	isMgr, err := e.store.IsManager(ctx, restaurantID, userID)
	if err != nil || !isMgr {
		return nil, ErrForbidden
	}

	plan, planRestRev, applied, appliedKey, fromUTC, toUTC, err := e.store.GetReplan(ctx, restaurantID, planID)
	if err != nil {
		return nil, ErrNotFound
	}

	if applied {
		if appliedKey == idempotencyKey {
			// Idempotent replay: load current reservations
			resList := []contracts.Reservation{}
			for _, a := range plan.Assignments {
				res, err := e.GetReservation(ctx, a.Reference)
				if err == nil {
					resList = append(resList, *res)
				}
			}
			return &contracts.ApplyPlanResponse{
				PlanID:             plan.PlanID,
				RestaurantRevision: planRestRev + 1,
				Reservations:       resList,
			}, nil
		}
		return nil, ErrPlanAlreadyApplied
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check restaurant revision
	var curRestRev int
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM restaurants WHERE id = ?", restaurantID).Scan(&curRestRev); err != nil {
		return nil, err
	}
	if curRestRev != planRestRev {
		return nil, ErrStalePlan
	}

	// Add closure
	if err := e.store.AddTableClosure(ctx, tx, restaurantID, plan.Closure.TableID, fromUTC, toUTC); err != nil {
		return nil, err
	}

	affectedSeries := make(map[string]bool)
	finalReservations := []contracts.Reservation{}

	for _, a := range plan.Assignments {
		cur, err := e.GetReservation(ctx, a.Reference)
		if err != nil {
			return nil, err
		}

		if a.Changed {
			newRev := cur.Revision + 1
			rawJSON, _ := json.Marshal(a.TableIDs)
			var singleTID *string
			if len(a.TableIDs) == 1 {
				singleTID = &a.TableIDs[0]
			}

			_, err = tx.ExecContext(ctx, `
				UPDATE reservations
				SET table_id = ?, table_ids_json = ?, revision = ?
				WHERE id = ?`,
				singleTID, string(rawJSON), newRev, cur.ID)
			if err != nil {
				return nil, err
			}

			if _, err := tx.ExecContext(ctx, "DELETE FROM reservation_tables WHERE reservation_id = ?", cur.ID); err != nil {
				return nil, err
			}
			for idx, tid := range a.TableIDs {
				if _, err := tx.ExecContext(ctx, "INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)", cur.ID, tid, idx); err != nil {
					return nil, err
				}
			}

			changes := []contracts.HistoryChange{
				{Field: "table_ids", From: cur.TableIDs, To: a.TableIDs},
			}
			if err := e.store.AppendHistoryWithPlan(ctx, tx, cur.ID, now.Unix(), "reassigned", changes, newRev, *cur.AcceptedTerms, &plan.PlanID); err != nil {
				return nil, err
			}

			// Check series
			var sID string
			if err := tx.QueryRowContext(ctx, "SELECT series_id FROM series_occurrences WHERE reservation_id = ?", cur.ID).Scan(&sID); err == nil {
				affectedSeries[sID] = true
			}

			cur.TableIDs = a.TableIDs
			cur.TableID = singleTID
			cur.Revision = newRev
		}

		finalReservations = append(finalReservations, *cur)
	}

	// Increment affected series revision
	for sID := range affectedSeries {
		if _, err := tx.ExecContext(ctx, "UPDATE series SET revision = revision + 1 WHERE id = ?", sID); err != nil {
			return nil, err
		}
	}

	// Increment restaurant revision once for the whole plan
	var newRestRev int
	_, err = tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", restaurantID)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, "SELECT revision FROM restaurants WHERE id = ?", restaurantID).Scan(&newRestRev)
	if err != nil {
		return nil, err
	}

	if err := e.store.MarkReplanApplied(ctx, tx, planID, idempotencyKey); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Sort finalReservations by reference ascending
	sort.Slice(finalReservations, func(i, j int) bool {
		return finalReservations[i].Reference < finalReservations[j].Reference
	})

	return &contracts.ApplyPlanResponse{
		PlanID:             plan.PlanID,
		RestaurantRevision: newRestRev,
		Reservations:       finalReservations,
	}, nil
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

	// Closures
	cRows, err := e.store.DB().QueryContext(ctx, `
		SELECT table_id, from_utc, to_utc
		FROM table_closures
		WHERE restaurant_id = ? AND from_utc < ? AND to_utc > ?`,
		restaurantID, lastSlotEnd, firstSlotStart)
	if err != nil {
		return nil, err
	}
	defer cRows.Close()

	type closureInterval struct {
		tableID string
		from    int64
		to      int64
	}
	var closures []closureInterval
	for cRows.Next() {
		var ci closureInterval
		if err := cRows.Scan(&ci.tableID, &ci.from, &ci.to); err == nil {
			closures = append(closures, ci)
		}
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
			if !hasOverlap {
				for _, cl := range closures {
					if cl.tableID == t.ID && cl.from < slotEndU && slotStartU < cl.to {
						hasOverlap = true
						break
					}
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
			if !hasOverlapA {
				for _, cl := range closures {
					if cl.tableID == pair[0] && cl.from < slotEndU && slotStartU < cl.to {
						hasOverlapA = true
						break
					}
				}
			}
			if !hasOverlapB {
				for _, cl := range closures {
					if cl.tableID == pair[1] && cl.from < slotEndU && slotStartU < cl.to {
						hasOverlapB = true
						break
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
