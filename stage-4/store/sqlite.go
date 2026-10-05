package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"tablekeeper/contracts"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("pragma %s failed: %w", p, err)
		}
	}

	if _, err := db.Exec(SchemaSQL); err != nil {
		return nil, fmt.Errorf("failed to init schema: %w", err)
	}

	st := &Store{db: db}

	var count int
	_ = db.QueryRow("SELECT count(*) FROM restaurants").Scan(&count)
	if count == 0 {
		_ = st.SeedDefaultRestaurants(context.Background())
	}

	return st, nil
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Lock() {
	s.mu.Lock()
}

func (s *Store) Unlock() {
	s.mu.Unlock()
}

func (s *Store) RLock() {
	s.mu.RLock()
}

func (s *Store) RUnlock() {
	s.mu.RUnlock()
}

func HashBody(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func BuildPolicyZero(r *contracts.Restaurant) contracts.AcceptedTerms {
	caps := make(map[string]int)
	for _, t := range r.Tables {
		caps[t.ID] = t.Capacity
	}
	return contracts.AcceptedTerms{
		PolicyVersion:              0,
		SlotMinutes:                r.SlotMinutes,
		ReservationDurationMinutes: r.ReservationDurationMinutes,
		CancellationCutoffMinutes:  r.CancellationCutoffMinutes,
		OpeningHours:               r.OpeningHours,
		Capacities:                 caps,
	}
}

func (s *Store) Reset(ctx context.Context, fixture *contracts.FixtureData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tables := []string{
		"audit_log", "idempotency", "table_replans", "table_closures",
		"series_occurrences", "series", "reservation_history", "reservation_tables",
		"reservations", "policies", "combinable_tables", "tables", "opening_hours",
		"restaurant_managers", "restaurants", "tokens", "users",
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s", t)); err != nil {
			return err
		}
	}

	stmtUser, err := tx.PrepareContext(ctx, "INSERT INTO users(id, email, password_hash, display_name) VALUES(?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmtUser.Close()

	for _, u := range fixture.Users {
		if _, err := stmtUser.ExecContext(ctx, u.ID, u.Email, u.Password, u.DisplayName); err != nil {
			return err
		}
	}

	stmtRest, err := tx.PrepareContext(ctx, `INSERT INTO restaurants(id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, revision) VALUES(?, ?, ?, ?, ?, ?, 0)`)
	if err != nil {
		return err
	}
	defer stmtRest.Close()

	stmtMgr, err := tx.PrepareContext(ctx, `INSERT INTO restaurant_managers(restaurant_id, user_id) VALUES(?, ?)`)
	if err != nil {
		return err
	}
	defer stmtMgr.Close()

	stmtHour, err := tx.PrepareContext(ctx, `INSERT INTO opening_hours(restaurant_id, weekday, opens, closes) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtHour.Close()

	stmtTable, err := tx.PrepareContext(ctx, `INSERT INTO tables(id, restaurant_id, label, capacity, sort_order) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtTable.Close()

	stmtComb, err := tx.PrepareContext(ctx, `INSERT INTO combinable_tables(restaurant_id, table_a, table_b, sort_order) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtComb.Close()

	restMap := make(map[string]*contracts.Restaurant)
	for i := range fixture.Restaurants {
		r := &fixture.Restaurants[i]
		restMap[r.ID] = r
		if _, err := stmtRest.ExecContext(ctx, r.ID, r.Name, r.Timezone, r.SlotMinutes, r.ReservationDurationMinutes, r.CancellationCutoffMinutes); err != nil {
			return err
		}
		for _, uid := range r.ManagerUserIDs {
			if _, err := stmtMgr.ExecContext(ctx, r.ID, uid); err != nil {
				return err
			}
		}
		for _, h := range r.OpeningHours {
			if _, err := stmtHour.ExecContext(ctx, r.ID, h.Weekday, h.Opens, h.Closes); err != nil {
				return err
			}
		}
		for idx, t := range r.Tables {
			if _, err := stmtTable.ExecContext(ctx, t.ID, r.ID, t.Label, t.Capacity, idx); err != nil {
				return err
			}
		}
		for idx, pair := range r.Combinable {
			if len(pair) == 2 {
				if _, err := stmtComb.ExecContext(ctx, r.ID, pair[0], pair[1], idx); err != nil {
					return err
				}
			}
		}
	}

	stmtRes, err := tx.PrepareContext(ctx, `
		INSERT INTO reservations(id, reference, restaurant_id, table_id, table_ids_json, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtRes.Close()

	stmtResTab, err := tx.PrepareContext(ctx, `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtResTab.Close()

	stmtHist, err := tx.PrepareContext(ctx, `
		INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmtHist.Close()

	for _, res := range fixture.Reservations {
		tIDs := res.TableIDs
		if len(tIDs) == 0 && res.TableID != nil && *res.TableID != "" {
			tIDs = []string{*res.TableID}
		}

		var singleTID *string
		if len(tIDs) == 1 {
			singleTID = &tIDs[0]
		}
		rawJSON, _ := json.Marshal(tIDs)

		stUTC := res.StartsAtUTC
		if stUTC == 0 && !res.StartsAt.IsZero() {
			stUTC = res.StartsAt.Unix()
		}
		endUTC := res.EndsAtUTC
		if endUTC == 0 && !res.EndsAt.IsZero() {
			endUTC = res.EndsAt.Unix()
		}
		createUTC := res.CreatedAtUTC
		if createUTC == 0 && !res.CreatedAt.IsZero() {
			createUTC = res.CreatedAt.Unix()
		}
		if createUTC == 0 {
			createUTC = time.Now().Unix()
		}

		status := res.Status
		if status == "" {
			status = "confirmed"
		}

		revision := res.Revision
		if revision == 0 {
			revision = 1
		}

		var terms contracts.AcceptedTerms
		if res.AcceptedTerms != nil {
			terms = *res.AcceptedTerms
		} else if r, ok := restMap[res.RestaurantID]; ok {
			terms = BuildPolicyZero(r)
		}
		termsJSON, _ := json.Marshal(terms)

		if _, err := stmtRes.ExecContext(ctx, res.ID, res.Reference, res.RestaurantID, singleTID, string(rawJSON), res.UserID, res.PartySize, status, res.StartsAtLocal, stUTC, endUTC, createUTC, revision, string(termsJSON)); err != nil {
			return err
		}

		for idx, tid := range tIDs {
			if _, err := stmtResTab.ExecContext(ctx, res.ID, tid, idx); err != nil {
				return err
			}
		}

		var changes []contracts.HistoryChange
		if len(tIDs) == 1 {
			changes = []contracts.HistoryChange{
				{Field: "table_id", From: nil, To: tIDs[0]},
				{Field: "starts_at_local", From: nil, To: res.StartsAtLocal},
				{Field: "party_size", From: nil, To: res.PartySize},
			}
		} else {
			changes = []contracts.HistoryChange{
				{Field: "table_ids", From: nil, To: tIDs},
				{Field: "starts_at_local", From: nil, To: res.StartsAtLocal},
				{Field: "party_size", From: nil, To: res.PartySize},
			}
		}
		changesJSON, _ := json.Marshal(changes)

		if _, err := stmtHist.ExecContext(ctx, res.ID, 1, createUTC, "created", string(changesJSON), revision, string(termsJSON)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*contracts.User, error) {
	row := s.db.QueryRowContext(ctx, "SELECT id, email, password_hash, display_name FROM users WHERE email = ?", email)
	var u contracts.User
	if err := row.Scan(&u.ID, &u.Email, &u.Password, &u.DisplayName); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, u *contracts.User) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO users(id, email, password_hash, display_name) VALUES(?, ?, ?, ?)", u.ID, u.Email, u.Password, u.DisplayName)
	return err
}

func (s *Store) CreateToken(ctx context.Context, token, userID string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO tokens(token, user_id) VALUES(?, ?)", token, userID)
	return err
}

func (s *Store) GetUserByToken(ctx context.Context, token string) (*contracts.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name FROM users u JOIN tokens t ON u.id = t.user_id WHERE t.token = ?`, token)
	var u contracts.User
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) IsManager(ctx context.Context, restaurantID, userID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restaurant_managers WHERE restaurant_id = ? AND user_id = ?`, restaurantID, userID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) GetRestaurant(ctx context.Context, id string) (*contracts.Restaurant, error) {
	row := s.db.QueryRowContext(ctx, "SELECT id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, revision FROM restaurants WHERE id = ?", id)
	var r contracts.Restaurant
	if err := row.Scan(&r.ID, &r.Name, &r.Timezone, &r.SlotMinutes, &r.ReservationDurationMinutes, &r.CancellationCutoffMinutes, &r.Revision); err != nil {
		return nil, err
	}

	mRows, err := s.db.QueryContext(ctx, "SELECT user_id FROM restaurant_managers WHERE restaurant_id = ? ORDER BY user_id", id)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var uid string
			if err := mRows.Scan(&uid); err == nil {
				r.ManagerUserIDs = append(r.ManagerUserIDs, uid)
			}
		}
	}

	hRows, err := s.db.QueryContext(ctx, "SELECT weekday, opens, closes FROM opening_hours WHERE restaurant_id = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer hRows.Close()
	for hRows.Next() {
		var h contracts.OpeningHour
		if err := hRows.Scan(&h.Weekday, &h.Opens, &h.Closes); err != nil {
			return nil, err
		}
		r.OpeningHours = append(r.OpeningHours, h)
	}

	tRows, err := s.db.QueryContext(ctx, "SELECT id, label, capacity FROM tables WHERE restaurant_id = ? ORDER BY sort_order", id)
	if err != nil {
		return nil, err
	}
	defer tRows.Close()
	for tRows.Next() {
		var t contracts.Table
		if err := tRows.Scan(&t.ID, &t.Label, &t.Capacity); err != nil {
			return nil, err
		}
		r.Tables = append(r.Tables, t)
	}

	cRows, err := s.db.QueryContext(ctx, "SELECT table_a, table_b FROM combinable_tables WHERE restaurant_id = ? ORDER BY sort_order", id)
	if err != nil {
		return nil, err
	}
	defer cRows.Close()
	r.Combinable = [][]string{}
	for cRows.Next() {
		var ta, tb string
		if err := cRows.Scan(&ta, &tb); err != nil {
			return nil, err
		}
		r.Combinable = append(r.Combinable, []string{ta, tb})
	}

	return &r, nil
}

func (s *Store) ListRestaurants(ctx context.Context) ([]*contracts.Restaurant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, timezone, revision FROM restaurants ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*contracts.Restaurant
	for rows.Next() {
		var r contracts.Restaurant
		if err := rows.Scan(&r.ID, &r.Name, &r.Timezone, &r.Revision); err != nil {
			return nil, err
		}
		list = append(list, &r)
	}
	return list, nil
}

func (s *Store) IncrementRestaurantRevision(ctx context.Context, tx *sql.Tx, restaurantID string) (int, error) {
	var newRev int
	if tx != nil {
		_, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", restaurantID)
		if err != nil {
			return 0, err
		}
		err = tx.QueryRowContext(ctx, "SELECT revision FROM restaurants WHERE id = ?", restaurantID).Scan(&newRev)
		return newRev, err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", restaurantID)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRowContext(ctx, "SELECT revision FROM restaurants WHERE id = ?", restaurantID).Scan(&newRev)
	return newRev, err
}

func (s *Store) GetRestaurantRevision(ctx context.Context, restaurantID string) (int, error) {
	var rev int
	err := s.db.QueryRowContext(ctx, "SELECT revision FROM restaurants WHERE id = ?", restaurantID).Scan(&rev)
	return rev, err
}

func (s *Store) PublishPolicy(ctx context.Context, restaurantID string, p *contracts.Policy) (*contracts.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var nextVersion int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(policy_version), 0) + 1 FROM policies WHERE restaurant_id = ?`, restaurantID).Scan(&nextVersion)
	if err != nil {
		return nil, err
	}

	hoursJSON, _ := json.Marshal(p.OpeningHours)
	capsJSON, _ := json.Marshal(p.Capacities)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO policies(restaurant_id, policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		restaurantID, nextVersion, p.EffectiveFrom, p.SlotMinutes, p.ReservationDurationMinutes, p.CancellationCutoffMinutes, string(hoursJSON), string(capsJSON), time.Now().Unix())
	if err != nil {
		return nil, err
	}

	// Increment restaurant revision
	if _, err := tx.ExecContext(ctx, "UPDATE restaurants SET revision = revision + 1 WHERE id = ?", restaurantID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	p.PolicyVersion = nextVersion
	return p, nil
}

func (s *Store) ListPolicies(ctx context.Context, restaurantID string) ([]contracts.Policy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json
		FROM policies
		WHERE restaurant_id = ?
		ORDER BY policy_version ASC`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []contracts.Policy
	for rows.Next() {
		var p contracts.Policy
		var hoursJSON, capsJSON string
		if err := rows.Scan(&p.PolicyVersion, &p.EffectiveFrom, &p.SlotMinutes, &p.ReservationDurationMinutes, &p.CancellationCutoffMinutes, &hoursJSON, &capsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(hoursJSON), &p.OpeningHours)
		_ = json.Unmarshal([]byte(capsJSON), &p.Capacities)
		policies = append(policies, p)
	}
	if policies == nil {
		policies = []contracts.Policy{}
	}
	return policies, nil
}

func (s *Store) GetEffectivePolicy(ctx context.Context, restaurantID, dateStr string) (*contracts.Policy, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json
		FROM policies
		WHERE restaurant_id = ? AND effective_from <= ?
		ORDER BY effective_from DESC, policy_version DESC
		LIMIT 1`, restaurantID, dateStr)

	var p contracts.Policy
	var hoursJSON, capsJSON string
	err := row.Scan(&p.PolicyVersion, &p.EffectiveFrom, &p.SlotMinutes, &p.ReservationDurationMinutes, &p.CancellationCutoffMinutes, &hoursJSON, &capsJSON)
	if err == nil {
		_ = json.Unmarshal([]byte(hoursJSON), &p.OpeningHours)
		_ = json.Unmarshal([]byte(capsJSON), &p.Capacities)
		return &p, nil
	}

	rest, err := s.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, err
	}

	caps := make(map[string]int)
	for _, t := range rest.Tables {
		caps[t.ID] = t.Capacity
	}

	return &contracts.Policy{
		PolicyVersion:              0,
		EffectiveFrom:              "",
		SlotMinutes:                rest.SlotMinutes,
		ReservationDurationMinutes: rest.ReservationDurationMinutes,
		CancellationCutoffMinutes:  rest.CancellationCutoffMinutes,
		OpeningHours:               rest.OpeningHours,
		Capacities:                 caps,
	}, nil
}

func (s *Store) GetReservationHistory(ctx context.Context, reservationID string) ([]contracts.HistoryEntry, error) {
	rowRest := s.db.QueryRowContext(ctx, `
		SELECT rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id
		WHERE r.id = ?`, reservationID)
	var tz string
	if err := rowRest.Scan(&tz); err != nil {
		return nil, err
	}
	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, at_utc, event, changes_json, revision, accepted_terms_json, plan_id
		FROM reservation_history
		WHERE reservation_id = ?
		ORDER BY seq ASC`, reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []contracts.HistoryEntry
	for rows.Next() {
		var e contracts.HistoryEntry
		var atU int64
		var changesJSON, termsJSON string
		var planNull sql.NullString
		if err := rows.Scan(&e.Seq, &atU, &e.Event, &changesJSON, &e.Revision, &termsJSON, &planNull); err != nil {
			return nil, err
		}
		e.At = time.Unix(atU, 0).In(loc).Format(time.RFC3339)
		_ = json.Unmarshal([]byte(changesJSON), &e.Changes)
		if e.Changes == nil {
			e.Changes = []contracts.HistoryChange{}
		}
		_ = json.Unmarshal([]byte(termsJSON), &e.AcceptedTerms)
		if planNull.Valid {
			e.PlanID = &planNull.String
		}
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []contracts.HistoryEntry{}
	}
	return entries, nil
}

func (s *Store) AppendHistory(ctx context.Context, tx *sql.Tx, reservationID string, atUTC int64, event string, changes []contracts.HistoryChange, revision int, terms contracts.AcceptedTerms) error {
	return s.AppendHistoryWithPlan(ctx, tx, reservationID, atUTC, event, changes, revision, terms, nil)
}

func (s *Store) AppendHistoryWithPlan(ctx context.Context, tx *sql.Tx, reservationID string, atUTC int64, event string, changes []contracts.HistoryChange, revision int, terms contracts.AcceptedTerms, planID *string) error {
	var nextSeq int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM reservation_history WHERE reservation_id = ?`, reservationID).Scan(&nextSeq)
	if err != nil {
		return err
	}

	if changes == nil {
		changes = []contracts.HistoryChange{}
	}
	changesJSON, _ := json.Marshal(changes)
	termsJSON, _ := json.Marshal(terms)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json, plan_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		reservationID, nextSeq, atUTC, event, string(changesJSON), revision, string(termsJSON), planID)
	return err
}

func (s *Store) AddTableClosure(ctx context.Context, tx *sql.Tx, restaurantID, tableID string, fromUTC, toUTC int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO table_closures(restaurant_id, table_id, from_utc, to_utc)
		VALUES(?, ?, ?, ?)`,
		restaurantID, tableID, fromUTC, toUTC)
	return err
}

func (s *Store) HasClosureOverlap(ctx context.Context, restaurantID, tableID string, startUTC, endUTC int64) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM table_closures
		WHERE restaurant_id = ? AND table_id = ? AND from_utc < ? AND to_utc > ?`,
		restaurantID, tableID, endUTC, startUTC).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) SaveReplan(ctx context.Context, plan *contracts.ReplanResponse, restaurantID string, fromUTC, toUTC int64, fromStr, toStr string) error {
	assignJSON, _ := json.Marshal(plan.Assignments)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO table_replans(plan_id, restaurant_id, restaurant_revision, closure_table_id, closure_from_utc, closure_to_utc, closure_from_str, closure_to_str, assignments_json, moved_count, unused_seats, created_at_utc)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.PlanID, restaurantID, plan.RestaurantRevision, plan.Closure.TableID, fromUTC, toUTC, fromStr, toStr, string(assignJSON), plan.MovedCount, plan.UnusedSeats, time.Now().Unix())
	return err
}

func (s *Store) GetReplan(ctx context.Context, restaurantID, planID string) (plan *contracts.ReplanResponse, restRev int, applied bool, appliedKey string, fromUTC int64, toUTC int64, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT plan_id, restaurant_revision, closure_table_id, closure_from_utc, closure_to_utc, closure_from_str, closure_to_str, assignments_json, moved_count, unused_seats, applied, applied_key
		FROM table_replans
		WHERE restaurant_id = ? AND plan_id = ?`, restaurantID, planID)

	var p contracts.ReplanResponse
	var aJSON string
	var appInt int
	var aKeyNull sql.NullString
	err = row.Scan(&p.PlanID, &p.RestaurantRevision, &p.Closure.TableID, &fromUTC, &toUTC, &p.Closure.From, &p.Closure.To, &aJSON, &p.MovedCount, &p.UnusedSeats, &appInt, &aKeyNull)
	if err != nil {
		return nil, 0, false, "", 0, 0, err
	}
	_ = json.Unmarshal([]byte(aJSON), &p.Assignments)
	if p.Assignments == nil {
		p.Assignments = []contracts.Assignment{}
	}
	if aKeyNull.Valid {
		appliedKey = aKeyNull.String
	}
	return &p, p.RestaurantRevision, appInt == 1, appliedKey, fromUTC, toUTC, nil
}

func (s *Store) MarkReplanApplied(ctx context.Context, tx *sql.Tx, planID, idempotencyKey string) error {
	_, err := tx.ExecContext(ctx, `UPDATE table_replans SET applied = 1, applied_key = ? WHERE plan_id = ?`, idempotencyKey, planID)
	return err
}

func (s *Store) GetIdempotency(ctx context.Context, userID, key, method, path string) (*contracts.IdempotencyItem, error) {
	row := s.db.QueryRowContext(ctx, "SELECT user_id, key, method, path, body_hash, status_code, response_body FROM idempotency WHERE user_id = ? AND key = ? AND method = ? AND path = ?", userID, key, method, path)
	var item contracts.IdempotencyItem
	if err := row.Scan(&item.UserID, &item.Key, &item.Method, &item.Path, &item.BodyHash, &item.StatusCode, &item.ResponseBody); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Store) SaveIdempotency(ctx context.Context, item *contracts.IdempotencyItem) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency(user_id, key, method, path, body_hash, status_code, response_body, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, key, method, path) DO UPDATE SET
		body_hash = excluded.body_hash,
		status_code = excluded.status_code,
		response_body = excluded.response_body`,
		item.UserID, item.Key, item.Method, item.Path, item.BodyHash, item.StatusCode, item.ResponseBody, time.Now().Unix())
	return err
}

func (s *Store) SeedDefaultRestaurants(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Seed default user Ada if not exists
	_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO users(id, email, password_hash, display_name) VALUES('u_ada', 'ada@example.com', 'correct horse', 'Ada Lovelace')`)

	type seedTable struct {
		id       string
		label    string
		capacity int
	}
	type seedRest struct {
		id       string
		name     string
		timezone string
		slotMin  int
		durMin   int
		cutMin   int
		opens    string
		closes   string
		tables   []seedTable
		comb     [][]string
	}

	rests := []seedRest{
		// =========================================================================
		// 1. JOEY Bellevue (Flagship Pacific NW Multi-Zone Social Venue - MAX 25 TABLES, 8,500 sq ft)
		// =========================================================================
		{
			id: "r_anker", name: "JOEY Bellevue", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "11:00", closes: "23:30",
			tables: []seedTable{
				{"t_1", "Heated Patio Booth 1", 2},
				{"t_2", "Heated Patio Booth 2", 2},
				{"t_3", "Main Dining Salon 3", 4},
				{"t_4", "Main Dining Salon 4", 4},
				{"t_5", "Main Dining Salon 5", 4},
				{"t_6", "Main Dining Salon 6", 4},
				{"t_7", "Panoramic Window 7", 2},
				{"t_8", "Panoramic Window 8", 2},
				{"t_9", "Fire Pit Booth 9", 4},
				{"t_10", "Fire Pit Booth 10", 4},
				{"t_11", "VIP Diamond Table 11", 8},
				{"t_12", "Executive 2-Top 12", 2},
				{"t_13", "Executive 2-Top 13", 2},
				{"t_14", "Cobalt Curved Booth 14", 4},
				{"t_15", "Grand Amber Banquet 15", 8},
				{"t_16", "Center Circle Table 16", 8},
				{"t_17", "Cyan Octagon Table 17", 6},
				{"t_18", "Sommelier Reserve 18", 6},
				{"t_19", "Lounge Velvet Booth 19", 4},
				{"t_20", "Lounge Velvet Booth 20", 4},
				{"t_21", "Chef Exhibition Rail 21", 2},
				{"t_22", "High-Top Bar Stool 22", 2},
				{"t_23", "High-Top Bar Stool 23", 2},
				{"t_24", "High-Top Bar Stool 24", 2},
				{"t_25", "High-Top Bar Stool 25", 2},
			},
			comb: [][]string{{"t_1", "t_2"}, {"t_3", "t_4"}, {"t_9", "t_10"}, {"t_19", "t_20"}},
		},

		// =========================================================================
		// 2. Sukiyabashi Jiro (Tokyo Ginza - STRICTLY 10 COUNTER SEATS ONLY, 450 sq ft)
		// =========================================================================
		{
			id: "r_jiro", name: "Sukiyabashi Jiro", timezone: "Asia/Tokyo", slotMin: 30, durMin: 60, cutMin: 2880, opens: "11:30", closes: "20:30",
			tables: []seedTable{
				{"jr_1", "Hinoki Master Counter 1", 1},
				{"jr_2", "Hinoki Master Counter 2", 1},
				{"jr_3", "Hinoki Master Counter 3", 1},
				{"jr_4", "Hinoki Master Counter 4", 1},
				{"jr_5", "Center Omakase Seat 5", 1},
				{"jr_6", "Center Omakase Seat 6", 1},
				{"jr_7", "Edomae Craft Seat 7", 1},
				{"jr_8", "Edomae Craft Seat 8", 1},
				{"jr_9", "Sake Pairing Seat 9", 1},
				{"jr_10", "Apprentice Station 10", 1},
			},
			comb: [][]string{{"jr_1", "jr_2"}, {"jr_3", "jr_4"}, {"jr_5", "jr_6"}, {"jr_7", "jr_8"}},
		},

		// =========================================================================
		// 3. The French Laundry (Yountville, Napa Valley - STRICTLY 12 TABLES, 2,400 sq ft)
		// =========================================================================
		{
			id: "r_frenchlaundry", name: "The French Laundry", timezone: "America/Los_Angeles", slotMin: 30, durMin: 180, cutMin: 1440, opens: "16:30", closes: "23:00",
			tables: []seedTable{
				{"fl_1", "Keller Garden Courtyard 1", 2},
				{"fl_2", "Keller Garden Courtyard 2", 2},
				{"fl_3", "Stone Wall Arbor 3", 4},
				{"fl_4", "Stone Wall Arbor 4", 4},
				{"fl_5", "Historic Main Salon 5", 2},
				{"fl_6", "Historic Main Salon 6", 2},
				{"fl_7", "Salon Velvet Banquette 7", 4},
				{"fl_8", "Salon Velvet Banquette 8", 4},
				{"fl_9", "Clothespin Signature Table 9", 4},
				{"fl_10", "Sommelier Reserve 10", 6},
				{"fl_11", "Historic Wine Vault 11", 6},
				{"fl_12", "Keller Private Dining Salon 12", 8},
			},
			comb: [][]string{{"fl_1", "fl_2"}, {"fl_7", "fl_8"}},
		},

		// =========================================================================
		// 4. Spinasse (Seattle Capitol Hill - STRICTLY 11 TABLES, 1,600 sq ft)
		// =========================================================================
		{
			id: "r_spinasse", name: "Spinasse", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 120, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"sp_1", "Hand-Cut Pasta Bar 1", 2},
				{"sp_2", "Hand-Cut Pasta Bar 2", 2},
				{"sp_3", "Hand-Cut Pasta Bar 3", 2},
				{"sp_4", "Rustic Trattoria 4", 2},
				{"sp_5", "Rustic Trattoria 5", 2},
				{"sp_6", "Piedmont Wood Table 6", 4},
				{"sp_7", "Piedmont Wood Table 7", 4},
				{"sp_8", "Fireplace Nook 8", 4},
				{"sp_9", "Cantina Wine Alcove 9", 4},
				{"sp_10", "Barolo Tasting Room 10", 6},
				{"sp_11", "Chef Family Table 11", 6},
			},
			comb: [][]string{{"sp_4", "sp_5"}, {"sp_6", "sp_7"}},
		},

		// =========================================================================
		// 5. Den Tokyo (Shibuya Kaiseki - STRICTLY 10 TABLES, 950 sq ft)
		// =========================================================================
		{
			id: "r_den", name: "Den Tokyo", timezone: "Asia/Tokyo", slotMin: 30, durMin: 120, cutMin: 1440, opens: "18:00", closes: "22:30",
			tables: []seedTable{
				{"dn_1", "Kaiseki Counter 1", 2},
				{"dn_2", "Kaiseki Counter 2", 2},
				{"dn_3", "Chef Hasegawa Seat 3", 2},
				{"dn_4", "Chef Hasegawa Seat 4", 2},
				{"dn_5", "Shibuya Garden 5", 4},
				{"dn_6", "Shibuya Garden 6", 4},
				{"dn_7", "Omotenashi Salon 7", 4},
				{"dn_8", "Omotenashi Salon 8", 4},
				{"dn_9", "Zashiki Tatami Room 9", 4},
				{"dn_10", "Private Dining Room 10", 6},
			},
			comb: [][]string{{"dn_1", "dn_2"}, {"dn_5", "dn_6"}},
		},

		// =========================================================================
		// 6. Le Gabriel - La Réserve (Paris - STRICTLY 12 TABLES, 1,800 sq ft)
		// =========================================================================
		{
			id: "r_legabriel", name: "Le Gabriel - La Réserve", timezone: "Europe/Paris", slotMin: 30, durMin: 150, cutMin: 1440, opens: "19:00", closes: "23:00",
			tables: []seedTable{
				{"lg_1", "Napoléon III Salon 1", 2},
				{"lg_2", "Napoléon III Salon 2", 2},
				{"lg_3", "Palais-Royal Booth 3", 2},
				{"lg_4", "Palais-Royal Booth 4", 2},
				{"lg_5", "Champs-Élysées Salon 5", 4},
				{"lg_6", "Champs-Élysées Salon 6", 4},
				{"lg_7", "Gold Leaf Table 7", 4},
				{"lg_8", "Gold Leaf Table 8", 4},
				{"lg_9", "Gabriel Banquette 9", 4},
				{"lg_10", "Haussmann Hall 10", 6},
				{"lg_11", "Grand Cru Salon 11", 6},
				{"lg_12", "Salon Impérial 12", 8},
			},
			comb: [][]string{{"lg_1", "lg_2"}, {"lg_5", "lg_6"}},
		},

		// =========================================================================
		// 7. Sushi Kashiba (Seattle Pike Place - STRICTLY 12 TABLES, 1,900 sq ft)
		// =========================================================================
		{
			id: "r_kashiba", name: "Sushi Kashiba", timezone: "America/Los_Angeles", slotMin: 30, durMin: 120, cutMin: 240, opens: "17:00", closes: "22:00",
			tables: []seedTable{
				{"sk_1", "Shiro Master Counter 1", 2},
				{"sk_2", "Shiro Master Counter 2", 2},
				{"sk_3", "Shiro Master Counter 3", 2},
				{"sk_4", "Shiro Master Counter 4", 2},
				{"sk_5", "Elliott Bay Window 5", 4},
				{"sk_6", "Elliott Bay Window 6", 4},
				{"sk_7", "Elliott Bay Window 7", 4},
				{"sk_8", "Courtyard View 8", 4},
				{"sk_9", "Pike Place Dining 9", 4},
				{"sk_10", "Sake Vault 10", 6},
				{"sk_11", "Master Shiro Salon 11", 6},
				{"sk_12", "Honorary Guest Room 12", 8},
			},
			comb: [][]string{{"sk_1", "sk_2"}, {"sk_5", "sk_6"}},
		},

		// =========================================================================
		// 8. The Walrus and the Carpenter (Seattle Ballard - STRICTLY 12 TABLES, 1,500 sq ft)
		// =========================================================================
		{
			id: "r_walrus", name: "The Walrus and the Carpenter", timezone: "America/Los_Angeles", slotMin: 15, durMin: 75, cutMin: 60, opens: "16:00", closes: "22:00",
			tables: []seedTable{
				{"wc_1", "Zinc Oyster Bar 1", 2},
				{"wc_2", "Zinc Oyster Bar 2", 2},
				{"wc_3", "Zinc Oyster Bar 3", 2},
				{"wc_4", "Zinc Oyster Bar 4", 2},
				{"wc_5", "Ballard Courtyard 5", 4},
				{"wc_6", "Ballard Courtyard 6", 4},
				{"wc_7", "Maritime Table 7", 4},
				{"wc_8", "Maritime Table 8", 4},
				{"wc_9", "Oysterman Banquette 9", 4},
				{"wc_10", "Fisherman Feast 10", 6},
				{"wc_11", "Harbor Vault 11", 6},
				{"wc_12", "Captain Quarters 12", 8},
			},
			comb: [][]string{{"wc_1", "wc_2"}, {"wc_5", "wc_6"}},
		},

		// =========================================================================
		// 9. Alinea (Chicago Lincoln Park - STRICTLY 14 TABLES, 3,200 sq ft)
		// =========================================================================
		{
			id: "r_alinea", name: "Alinea", timezone: "America/Chicago", slotMin: 30, durMin: 180, cutMin: 1440, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"al_1", "The Gallery Front 1", 2},
				{"al_2", "The Gallery Front 2", 2},
				{"al_3", "The Gallery Center 3", 4},
				{"al_4", "The Gallery Center 4", 4},
				{"al_5", "Salon Mirrored Table 5", 2},
				{"al_6", "Salon Mirrored Table 6", 2},
				{"al_7", "Salon Banquette 7", 4},
				{"al_8", "Salon Banquette 8", 4},
				{"al_9", "Sensory Lab Table 9", 4},
				{"al_10", "Floating Balloon Table 10", 4},
				{"al_11", "Achatz Kitchen Experience 11", 6},
				{"al_12", "Achatz Kitchen Experience 12", 6},
				{"al_13", "Molecular Vault 13", 6},
				{"al_14", "Culinary Grand Table 14", 8},
			},
			comb: [][]string{{"al_1", "al_2"}, {"al_3", "al_4"}, {"al_7", "al_8"}},
		},

		// =========================================================================
		// 10. Gary Danko (San Francisco Wharf - STRICTLY 14 TABLES, 2,600 sq ft)
		// =========================================================================
		{
			id: "r_garydanko", name: "Gary Danko", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"gd_1", "Wharf Window 1", 2},
				{"gd_2", "Wharf Window 2", 2},
				{"gd_3", "Sommelier Booth 3", 2},
				{"gd_4", "Sommelier Booth 4", 2},
				{"gd_5", "Main Salon 5", 4},
				{"gd_6", "Main Salon 6", 4},
				{"gd_7", "Main Salon 7", 4},
				{"gd_8", "Cheese Cart Alcove 8", 4},
				{"gd_9", "Flambé Banquette 9", 4},
				{"gd_10", "Cellar Alcove 10", 4},
				{"gd_11", "Reserve Wine Table 11", 6},
				{"gd_12", "Reserve Wine Table 12", 6},
				{"gd_13", "Chef Danko Table 13", 6},
				{"gd_14", "Grand Feast Salon 14", 8},
			},
			comb: [][]string{{"gd_1", "gd_2"}, {"gd_5", "gd_6"}},
		},

		// =========================================================================
		// 11. Restaurant Tim Raue (Berlin Kreuzberg - STRICTLY 14 TABLES, 2,500 sq ft)
		// =========================================================================
		{
			id: "r_timraue", name: "Restaurant Tim Raue", timezone: "Europe/Berlin", slotMin: 30, durMin: 120, cutMin: 1440, opens: "18:00", closes: "23:00",
			tables: []seedTable{
				{"tr_1", "Checkpoint Charlie Salon 1", 2},
				{"tr_2", "Checkpoint Charlie Salon 2", 2},
				{"tr_3", "Asian Fusion Booth 3", 2},
				{"tr_4", "Asian Fusion Booth 4", 2},
				{"tr_5", "Kreuzberg Gallery 5", 4},
				{"tr_6", "Kreuzberg Gallery 6", 4},
				{"tr_7", "Wasabi Table 7", 4},
				{"tr_8", "Wasabi Table 8", 4},
				{"tr_9", "Peking Duck Alcove 9", 4},
				{"tr_10", "Dim Sum Station 10", 4},
				{"tr_11", "Chef Tim Raue Table 11", 6},
				{"tr_12", "Chef Tim Raue Table 12", 6},
				{"tr_13", "Berlin Wall Reserve 13", 6},
				{"tr_14", "Imperial Jade Room 14", 8},
			},
			comb: [][]string{{"tr_1", "tr_2"}, {"tr_5", "tr_6"}},
		},

		// =========================================================================
		// 12. The Ledbury (London Notting Hill - STRICTLY 14 TABLES, 2,700 sq ft)
		// =========================================================================
		{
			id: "r_ledbury", name: "The Ledbury", timezone: "Europe/London", slotMin: 30, durMin: 150, cutMin: 1440, opens: "18:00", closes: "22:30",
			tables: []seedTable{
				{"ld_1", "Notting Hill Salon 1", 2},
				{"ld_2", "Notting Hill Salon 2", 2},
				{"ld_3", "Wine Library Booth 3", 2},
				{"ld_4", "Wine Library Booth 4", 2},
				{"ld_5", "Tasting Room 5", 4},
				{"ld_6", "Tasting Room 6", 4},
				{"ld_7", "Garden Terrace Table 7", 4},
				{"ld_8", "Garden Terrace Table 8", 4},
				{"ld_9", "English Truffle Table 9", 4},
				{"ld_10", "Conservatory Alcove 10", 4},
				{"ld_11", "Brett Graham Table 11", 6},
				{"ld_12", "Venison Salon 12", 6},
				{"ld_13", "Cellar Master Suite 13", 6},
				{"ld_14", "Conservatory Grand 14", 8},
			},
			comb: [][]string{{"ld_1", "ld_2"}, {"ld_5", "ld_6"}},
		},

		// =========================================================================
		// 13. COMMUNION Restaurant & Bar (Seattle Central District - STRICTLY 14 TABLES, 2,800 sq ft)
		// =========================================================================
		{
			id: "r_communion", name: "COMMUNION Restaurant & Bar", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "16:30", closes: "22:00",
			tables: []seedTable{
				{"cm_1", "Soul Booth 1", 2},
				{"cm_2", "Soul Booth 2", 2},
				{"cm_3", "Soul Booth 3", 2},
				{"cm_4", "Soul Booth 4", 2},
				{"cm_5", "Central District Table 5", 4},
				{"cm_6", "Central District Table 6", 4},
				{"cm_7", "Central District Table 7", 4},
				{"cm_8", "Kristi Family Table 8", 4},
				{"cm_9", "Catfish Corner 9", 4},
				{"cm_10", "Po' Boy Banquette 10", 4},
				{"cm_11", "Sweet Potato Salon 11", 6},
				{"cm_12", "Reverence Vault 12", 6},
				{"cm_13", "Community Table 13", 6},
				{"cm_14", "Central Grand Hall 14", 8},
			},
			comb: [][]string{{"cm_1", "cm_2"}, {"cm_5", "cm_6"}},
		},

		// =========================================================================
		// 14. Palace Kitchen (Seattle Belltown - STRICTLY 15 TABLES, 3,000 sq ft)
		// =========================================================================
		{
			id: "r_palace", name: "Palace Kitchen", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "16:00", closes: "23:59",
			tables: []seedTable{
				{"pk_1", "Horseshoe Bar 1", 2},
				{"pk_2", "Horseshoe Bar 2", 2},
				{"pk_3", "Horseshoe Bar 3", 2},
				{"pk_4", "Hearth Table 4", 2},
				{"pk_5", "Hearth Table 5", 2},
				{"pk_6", "Rotisserie Table 6", 4},
				{"pk_7", "Rotisserie Table 7", 4},
				{"pk_8", "Rotisserie Table 8", 4},
				{"pk_9", "Belltown Booth 9", 4},
				{"pk_10", "Belltown Booth 10", 4},
				{"pk_11", "Applewood Grill 11", 4},
				{"pk_12", "Tom Douglas Salon 12", 6},
				{"pk_13", "Goat Cheese Fondue Table 13", 6},
				{"pk_14", "Captain Table 14", 6},
				{"pk_15", "Palace Feast Table 15", 8},
			},
			comb: [][]string{{"pk_1", "pk_2"}, {"pk_6", "pk_7"}, {"pk_9", "pk_10"}},
		},

		// =========================================================================
		// 15. Carbone NYC (Greenwich Village - STRICTLY 16 TABLES, 2,800 sq ft)
		// =========================================================================
		{
			id: "r_carbone", name: "Carbone NYC", timezone: "America/New_York", slotMin: 15, durMin: 90, cutMin: 120, opens: "17:00", closes: "23:59",
			tables: []seedTable{
				{"cb_1", "Thompson St Window 1", 2},
				{"cb_2", "Thompson St Window 2", 2},
				{"cb_3", "Tuxedo Velvet Banquette 3", 4},
				{"cb_4", "Tuxedo Velvet Banquette 4", 4},
				{"cb_5", "Tuxedo Velvet Banquette 5", 4},
				{"cb_6", "Tuxedo Velvet Banquette 6", 4},
				{"cb_7", "Greenwich Village Salon 7", 2},
				{"cb_8", "Greenwich Village Salon 8", 2},
				{"cb_9", "Spicy Rigatoni Table 9", 4},
				{"cb_10", "Spicy Rigatoni Table 10", 4},
				{"cb_11", "Veal Parmigiana Nook 11", 4},
				{"cb_12", "Mario Carbone Table 12", 4},
				{"cb_13", "Capo Corner Booth 13", 6},
				{"cb_14", "Capo Corner Booth 14", 6},
				{"cb_15", "Godfather Banquet 15", 6},
				{"cb_16", "Grand Mafioso Table 16", 8},
			},
			comb: [][]string{{"cb_1", "cb_2"}, {"cb_3", "cb_4"}, {"cb_9", "cb_10"}},
		},

		// =========================================================================
		// 16. Le Bernardin (New York Midtown - STRICTLY 16 TABLES, 3,400 sq ft)
		// =========================================================================
		{
			id: "r_lebernardin", name: "Le Bernardin", timezone: "America/New_York", slotMin: 30, durMin: 150, cutMin: 1440, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"lb_1", "Midtown Window 1", 2},
				{"lb_2", "Midtown Window 2", 2},
				{"lb_3", "Sommelier Banquette 3", 2},
				{"lb_4", "Sommelier Banquette 4", 2},
				{"lb_5", "Seafood Salon 5", 4},
				{"lb_6", "Seafood Salon 6", 4},
				{"lb_7", "Seafood Salon 7", 4},
				{"lb_8", "Caviar Tasting Table 8", 4},
				{"lb_9", "Ripert Signature Table 9", 4},
				{"lb_10", "Ripert Signature Table 10", 4},
				{"lb_11", "Midtown Grand 11", 4},
				{"lb_12", "Midtown Grand 12", 4},
				{"lb_13", "Bluefin Tuna Salon 13", 6},
				{"lb_14", "Le Bernardin Vault 14", 6},
				{"lb_15", "Presidential Salon 15", 6},
				{"lb_16", "Sommelier Grand Cru 16", 8},
			},
			comb: [][]string{{"lb_1", "lb_2"}, {"lb_5", "lb_6"}, {"lb_9", "lb_10"}},
		},

		// =========================================================================
		// 17. The Pink Door (Seattle Post Alley - STRICTLY 16 TABLES, 3,200 sq ft)
		// =========================================================================
		{
			id: "r_pinkdoor", name: "The Pink Door", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "11:30", closes: "23:00",
			tables: []seedTable{
				{"pd_1", "Post Alley Entrance 1", 2},
				{"pd_2", "Post Alley Entrance 2", 2},
				{"pd_3", "Cabaret Front 3", 2},
				{"pd_4", "Cabaret Front 4", 2},
				{"pd_5", "Trapeze View Booth 5", 4},
				{"pd_6", "Trapeze View Booth 6", 4},
				{"pd_7", "Post Alley Deck 7", 4},
				{"pd_8", "Post Alley Deck 8", 4},
				{"pd_9", "Elliott Bay Sunset 9", 4},
				{"pd_10", "Elliott Bay Sunset 10", 4},
				{"pd_11", "Wine Cellar 11", 4},
				{"pd_12", "Tarot Reader Nook 12", 4},
				{"pd_13", "Piazza Table 13", 6},
				{"pd_14", "Jacopo Family Table 14", 6},
				{"pd_15", "Burlesque Banquette 15", 6},
				{"pd_16", "Grand Cabaret Table 16", 8},
			},
			comb: [][]string{{"pd_1", "pd_2"}, {"pd_5", "pd_6"}, {"pd_7", "pd_8"}},
		},

		// =========================================================================
		// 18. Canlis (Seattle Queen Anne - STRICTLY 18 TABLES, 4,800 sq ft)
		// =========================================================================
		{
			id: "r_canlis", name: "Canlis", timezone: "America/Los_Angeles", slotMin: 30, durMin: 150, cutMin: 1440, opens: "17:00", closes: "23:00",
			tables: []seedTable{
				{"cn_1", "Lake Union Window Rail 1", 2},
				{"cn_2", "Lake Union Window Rail 2", 2},
				{"cn_3", "Lake Union Window Rail 3", 2},
				{"cn_4", "Cascade View 4", 2},
				{"cn_5", "Cascade View 5", 2},
				{"cn_6", "Copper Hearth Center 6", 4},
				{"cn_7", "Copper Hearth Center 7", 4},
				{"cn_8", "Copper Hearth Center 8", 4},
				{"cn_9", "Mid-Century Beam 9", 4},
				{"cn_10", "Mid-Century Beam 10", 4},
				{"cn_11", "Steinway Piano Lounge 11", 4},
				{"cn_12", "Steinway Piano Lounge 12", 4},
				{"cn_13", "Sommelier Cellar 13", 4},
				{"cn_14", "Mark Canlis Salon 14", 6},
				{"cn_15", "Peter Canlis Penthouse 15", 6},
				{"cn_16", "Wine Vault 16", 6},
				{"cn_17", "Terrace Sunset 17", 6},
				{"cn_18", "Grand Cascade Hall 18", 8},
			},
			comb: [][]string{{"cn_1", "cn_2"}, {"cn_6", "cn_7"}, {"cn_11", "cn_12"}},
		},

		// =========================================================================
		// 19. El Gaucho Seattle (Belltown - STRICTLY 18 TABLES, 4,500 sq ft)
		// =========================================================================
		{
			id: "r_elgaucho", name: "El Gaucho Seattle", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"eg_1", "Charcoal Grill Rail 1", 2},
				{"eg_2", "Charcoal Grill Rail 2", 2},
				{"eg_3", "Steinway Piano Booth 3", 2},
				{"eg_4", "Steinway Piano Booth 4", 2},
				{"eg_5", "Captain Booth 5", 4},
				{"eg_6", "Captain Booth 6", 4},
				{"eg_7", "Vintage Leather Booth 7", 4},
				{"eg_8", "Vintage Leather Booth 8", 4},
				{"eg_9", "Flaming Sword Station 9", 4},
				{"eg_10", "Tableside Caesar 10", 4},
				{"eg_11", "Cigar Lounge Nook 11", 4},
				{"eg_12", "Wine Cellar 12", 4},
				{"eg_13", "Pampas Salon 13", 6},
				{"eg_14", "Sommelier Vault 14", 6},
				{"eg_15", "Gaucho Executive 15", 6},
				{"eg_16", "Belltown Boardroom 16", 6},
				{"eg_17", "Gaucho Master Suite 17", 8},
				{"eg_18", "Presidential Steakhouse 18", 8},
			},
			comb: [][]string{{"eg_1", "eg_2"}, {"eg_5", "eg_6"}, {"eg_7", "eg_8"}},
		},

		// =========================================================================
		// 20. Bestia DTLA (Los Angeles Arts District - STRICTLY 18 TABLES, 4,400 sq ft)
		// =========================================================================
		{
			id: "r_bestia", name: "Bestia DTLA", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "17:00", closes: "23:00",
			tables: []seedTable{
				{"bs_1", "Charcuterie Counter 1", 2},
				{"bs_2", "Charcuterie Counter 2", 2},
				{"bs_3", "Charcuterie Counter 3", 2},
				{"bs_4", "Wood Fire Oven 4", 2},
				{"bs_5", "Industrial Booth 5", 4},
				{"bs_6", "Industrial Booth 6", 4},
				{"bs_7", "Arts District Hall 7", 4},
				{"bs_8", "Arts District Hall 8", 4},
				{"bs_9", "Arts District Hall 9", 4},
				{"bs_10", "Bone Marrow Table 10", 4},
				{"bs_11", "Cavatelli Nook 11", 4},
				{"bs_12", "Raw Bar Rail 12", 4},
				{"bs_13", "Pork Feast Table 13", 6},
				{"bs_14", "Pork Feast Table 14", 6},
				{"bs_15", "Butcher Salon 15", 6},
				{"bs_16", "Warehouse Vault 16", 6},
				{"bs_17", "Ori Menashe Table 17", 8},
				{"bs_18", "Industrial Grand 18", 8},
			},
			comb: [][]string{{"bs_1", "bs_2"}, {"bs_5", "bs_6"}, {"bs_7", "bs_8"}},
		},

		// =========================================================================
		// 21. Gramercy Tavern (New York Flatiron - STRICTLY 18 TABLES, 4,600 sq ft)
		// =========================================================================
		{
			id: "r_gramercy", name: "Gramercy Tavern", timezone: "America/New_York", slotMin: 15, durMin: 90, cutMin: 60, opens: "12:00", closes: "23:00",
			tables: []seedTable{
				{"gt_1", "Tavern Front 1", 2},
				{"gt_2", "Tavern Front 2", 2},
				{"gt_3", "Wood Hearth 3", 2},
				{"gt_4", "Wood Hearth 4", 2},
				{"gt_5", "Dining Room Hall 5", 4},
				{"gt_6", "Dining Room Hall 6", 4},
				{"gt_7", "Dining Room Hall 7", 4},
				{"gt_8", "Dining Room Hall 8", 4},
				{"gt_9", "Floral Salon 9", 4},
				{"gt_10", "Floral Salon 10", 4},
				{"gt_11", "Mural Wall 11", 4},
				{"gt_12", "Flatiron Nook 12", 4},
				{"gt_13", "Danny Meyer Table 13", 6},
				{"gt_14", "Danny Meyer Table 14", 6},
				{"gt_15", "Sommelier Reserve 15", 6},
				{"gt_16", "Farm to Table 16", 6},
				{"gt_17", "Gramercy Grand 17", 8},
				{"gt_18", "Founders Banquet 18", 8},
			},
			comb: [][]string{{"gt_1", "gt_2"}, {"gt_5", "gt_6"}, {"gt_9", "gt_10"}},
		},

		// =========================================================================
		// 22. Ascend Prime Steak & Sushi (Bellevue 31st Floor - STRICTLY 20 TABLES, 5,600 sq ft)
		// =========================================================================
		{
			id: "r_ascend", name: "Ascend Prime Steak & Sushi", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "16:30", closes: "23:00",
			tables: []seedTable{
				{"as_1", "31st Skyline Window 1", 2},
				{"as_2", "31st Skyline Window 2", 2},
				{"as_3", "Mt. Rainier View 3", 2},
				{"as_4", "Mt. Rainier View 4", 2},
				{"as_5", "Robata Counter 5", 4},
				{"as_6", "Robata Counter 6", 4},
				{"as_7", "Penthouse Booth 7", 4},
				{"as_8", "Penthouse Booth 8", 4},
				{"as_9", "Bellevue Towers 9", 4},
				{"as_10", "Bellevue Towers 10", 4},
				{"as_11", "Sushi Pavilion 11", 4},
				{"as_12", "Sushi Pavilion 12", 4},
				{"as_13", "Sky Lounge 13", 6},
				{"as_14", "Sky Lounge 14", 6},
				{"as_15", "Woodfire Hearth 15", 6},
				{"as_16", "Woodfire Hearth 16", 6},
				{"as_17", "Ascend VIP Booth 17", 6},
				{"as_18", "Rainier Penthouse 18", 6},
				{"as_19", "Presidential Suite 19", 8},
				{"as_20", "Cloud 31 Grand 20", 8},
			},
			comb: [][]string{{"as_1", "as_2"}, {"as_7", "as_8"}, {"as_13", "as_14"}},
		},

		// =========================================================================
		// 23. Girl & the Goat (Chicago West Loop - STRICTLY 20 TABLES, 5,200 sq ft)
		// =========================================================================
		{
			id: "r_girlgoat", name: "Girl & the Goat", timezone: "America/Chicago", slotMin: 15, durMin: 90, cutMin: 60, opens: "16:30", closes: "23:00",
			tables: []seedTable{
				{"gg_1", "West Loop Bar 1", 2},
				{"gg_2", "West Loop Bar 2", 2},
				{"gg_3", "Kitchen Counter 3", 2},
				{"gg_4", "Kitchen Counter 4", 2},
				{"gg_5", "Rustic Booth 5", 4},
				{"gg_6", "Rustic Booth 6", 4},
				{"gg_7", "Rustic Booth 7", 4},
				{"gg_8", "Rustic Booth 8", 4},
				{"gg_9", "Wood Beam Dining 9", 4},
				{"gg_10", "Wood Beam Dining 10", 4},
				{"gg_11", "Goat Empanada Table 11", 4},
				{"gg_12", "Stephanie Izard Nook 12", 4},
				{"gg_13", "Family Table 13", 6},
				{"gg_14", "Family Table 14", 6},
				{"gg_15", "Goat Lounge 15", 6},
				{"gg_16", "Goat Lounge 16", 6},
				{"gg_17", "Randolph Street 17", 6},
				{"gg_18", "West Loop Feast 18", 6},
				{"gg_19", "Grand Goat Salon 19", 8},
				{"gg_20", "Izard Banquet 20", 8},
			},
			comb: [][]string{{"gg_1", "gg_2"}, {"gg_5", "gg_6"}, {"gg_13", "gg_14"}},
		},

		// =========================================================================
		// 24. Dishoom Covent Garden (London - STRICTLY 20 TABLES, 5,400 sq ft)
		// =========================================================================
		{
			id: "r_dishoom", name: "Dishoom Covent Garden", timezone: "Europe/London", slotMin: 15, durMin: 90, cutMin: 60, opens: "08:00", closes: "23:00",
			tables: []seedTable{
				{"ds_1", "Irani Cafe Verandah 1", 2},
				{"ds_2", "Irani Cafe Verandah 2", 2},
				{"ds_3", "Irani Cafe Verandah 3", 2},
				{"ds_4", "Permit Room Bar 4", 2},
				{"ds_5", "Bombay Dining Room 5", 4},
				{"ds_6", "Bombay Dining Room 6", 4},
				{"ds_7", "Bombay Dining Room 7", 4},
				{"ds_8", "Bombay Dining Room 8", 4},
				{"ds_9", "House Black Daal 9", 4},
				{"ds_10", "Chai Wallah Nook 10", 4},
				{"ds_11", "Old Bombay Alcove 11", 4},
				{"ds_12", "Bespoke Booth 12", 4},
				{"ds_13", "Family Thali Table 13", 6},
				{"ds_14", "Family Thali Table 14", 6},
				{"ds_15", "Victoria Terminus 15", 6},
				{"ds_16", "Marine Drive 16", 6},
				{"ds_17", "Governor Banquet 17", 6},
				{"ds_18", "Governor Banquet 18", 6},
				{"ds_19", "Colaba Grand Hall 19", 8},
				{"ds_20", "Willingdon Suite 20", 8},
			},
			comb: [][]string{{"ds_1", "ds_2"}, {"ds_5", "ds_6"}, {"ds_13", "ds_14"}},
		},

		// =========================================================================
		// 25. Zum Anker Historic (Berlin Spree - STRICTLY 20 TABLES, 5,000 sq ft)
		// =========================================================================
		{
			id: "r_berlin_anker", name: "Zum Anker Historic", timezone: "Europe/Berlin", slotMin: 30, durMin: 90, cutMin: 120, opens: "18:00", closes: "23:00",
			tables: []seedTable{
				{"ba_1", "Spree River View 1", 2},
				{"ba_2", "Spree River View 2", 2},
				{"ba_3", "Spree River View 3", 2},
				{"ba_4", "Spree River View 4", 2},
				{"ba_5", "Mitte Gaststube 5", 4},
				{"ba_6", "Mitte Gaststube 6", 4},
				{"ba_7", "Mitte Gaststube 7", 4},
				{"ba_8", "Mitte Gaststube 8", 4},
				{"ba_9", "Eichenholz Nook 9", 4},
				{"ba_10", "Eichenholz Nook 10", 4},
				{"ba_11", "Kupfer Brauhaus 11", 4},
				{"ba_12", "Kupfer Brauhaus 12", 4},
				{"ba_13", "Brauhaus Table 13", 6},
				{"ba_14", "Brauhaus Table 14", 6},
				{"ba_15", "Fasskeller Suite 15", 6},
				{"ba_16", "Fasskeller Suite 16", 6},
				{"ba_17", "Historischer Tisch 17", 6},
				{"ba_18", "Spreeufer Lounge 18", 6},
				{"ba_19", "Alt-Berlin Hall 19", 8},
				{"ba_20", "Kaiserliche Loge 20", 8},
			},
			comb: [][]string{{"ba_1", "ba_2"}, {"ba_5", "ba_6"}, {"ba_13", "ba_14"}},
		},

		// =========================================================================
		// 26. House of Prime Rib (San Francisco Van Ness - STRICTLY 22 TABLES, 5,800 sq ft)
		// =========================================================================
		{
			id: "r_hopr", name: "House of Prime Rib", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "16:30", closes: "22:30",
			tables: []seedTable{
				{"hp_1", "Carving Cart Rail 1", 2},
				{"hp_2", "Carving Cart Rail 2", 2},
				{"hp_3", "English Booth 3", 2},
				{"hp_4", "English Booth 4", 2},
				{"hp_5", "Fireplace Banquette 5", 4},
				{"hp_6", "Fireplace Banquette 6", 4},
				{"hp_7", "Fireplace Banquette 7", 4},
				{"hp_8", "Fireplace Banquette 8", 4},
				{"hp_9", "City Cut Nook 9", 4},
				{"hp_10", "King Henry VIII 10", 4},
				{"hp_11", "Yorkshire Pudding Table 11", 4},
				{"hp_12", "Creamed Spinach Booth 12", 4},
				{"hp_13", "Lords Dining Table 13", 6},
				{"hp_14", "Lords Dining Table 14", 6},
				{"hp_15", "Zeppelin Cart Suite 15", 6},
				{"hp_16", "Sommelier Cellar 16", 6},
				{"hp_17", "Van Ness Alcove 17", 6},
				{"hp_18", "Master Carver Suite 18", 6},
				{"hp_19", "Grand Dining Hall 19", 8},
				{"hp_20", "Grand Dining Hall 20", 8},
				{"hp_21", "English Bar Stool 21", 2},
				{"hp_22", "English Bar Stool 22", 2},
			},
			comb: [][]string{{"hp_1", "hp_2"}, {"hp_5", "hp_6"}, {"hp_13", "hp_14"}},
		},

		// =========================================================================
		// 27. Nobu Malibu (Pacific Coast Highway - STRICTLY 22 TABLES, 6,500 sq ft)
		// =========================================================================
		{
			id: "r_nobumalibu", name: "Nobu Malibu", timezone: "America/Los_Angeles", slotMin: 15, durMin: 105, cutMin: 120, opens: "12:00", closes: "22:30",
			tables: []seedTable{
				{"nb_1", "Pacific Deck Surf 1", 2},
				{"nb_2", "Pacific Deck Surf 2", 2},
				{"nb_3", "Pacific Deck Surf 3", 2},
				{"nb_4", "Pacific Deck Surf 4", 2},
				{"nb_5", "Oceanfront Booth 5", 4},
				{"nb_6", "Oceanfront Booth 6", 4},
				{"nb_7", "Oceanfront Booth 7", 4},
				{"nb_8", "Oceanfront Booth 8", 4},
				{"nb_9", "Robata Grill Rail 9", 2},
				{"nb_10", "Robata Grill Rail 10", 2},
				{"nb_11", "Sushi Pavilion 11", 4},
				{"nb_12", "Sushi Pavilion 12", 4},
				{"nb_13", "Black Cod Table 13", 4},
				{"nb_14", "Yellowtail Jalapeno Nook 14", 4},
				{"nb_15", "Malibu Sunset Salon 15", 6},
				{"nb_16", "Malibu Sunset Salon 16", 6},
				{"nb_17", "Teak Pavilion 17", 6},
				{"nb_18", "Teak Pavilion 18", 6},
				{"nb_19", "Matsuhisa VIP Terrace 19", 6},
				{"nb_20", "Matsuhisa VIP Terrace 20", 6},
				{"nb_21", "Celebrity Ocean Grand 21", 8},
				{"nb_22", "Pacific Penthouse 22", 8},
			},
			comb: [][]string{{"nb_1", "nb_2"}, {"nb_5", "nb_6"}, {"nb_15", "nb_16"}},
		},

		// =========================================================================
		// 28. Zuma Dubai (DIFC Gate Village - FLAGSHIP ULTRA-LUXURY MAX 25 TABLES, 9,200 sq ft)
		// =========================================================================
		{
			id: "r_zuma_dubai", name: "Zuma Dubai DIFC", timezone: "Asia/Dubai", slotMin: 15, durMin: 120, cutMin: 120, opens: "12:00", closes: "23:59",
			tables: []seedTable{
				{"zm_1", "DIFC Gate Lounge 1", 2},
				{"zm_2", "DIFC Gate Lounge 2", 2},
				{"zm_3", "DIFC Gate Lounge 3", 2},
				{"zm_4", "Robata Grill Counter 4", 2},
				{"zm_5", "Robata Grill Counter 5", 2},
				{"zm_6", "Sushi Exhibition Rail 6", 2},
				{"zm_7", "Skyline Glass Booth 7", 4},
				{"zm_8", "Skyline Glass Booth 8", 4},
				{"zm_9", "Main Hall Pavilion 9", 4},
				{"zm_10", "Main Hall Pavilion 10", 4},
				{"zm_11", "Main Hall Pavilion 11", 4},
				{"zm_12", "Main Hall Pavilion 12", 4},
				{"zm_13", "Sake Sommelier Lounge 13", 4},
				{"zm_14", "Sake Sommelier Lounge 14", 4},
				{"zm_15", "Burj Khalifa Vista 15", 6},
				{"zm_16", "Burj Khalifa Vista 16", 6},
				{"zm_17", "Granite Spiral Nook 17", 6},
				{"zm_18", "Granite Spiral Nook 18", 6},
				{"zm_19", "Mezzanine VIP Pod 19", 6},
				{"zm_20", "Mezzanine VIP Pod 20", 6},
				{"zm_21", "Royal Palm Suite 21", 8},
				{"zm_22", "Royal Palm Suite 22", 8},
				{"zm_23", "Emirates High-Top Stool 23", 2},
				{"zm_24", "Emirates High-Top Stool 24", 2},
				{"zm_25", "Emirates High-Top Stool 25", 2},
			},
			comb: [][]string{{"zm_1", "zm_2"}, {"zm_7", "zm_8"}, {"zm_15", "zm_16"}},
		},
	}

	weekdays := []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

	for _, r := range rests {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO restaurants(id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, revision) VALUES(?, ?, ?, ?, ?, ?, 0)`,
			r.id, r.name, r.timezone, r.slotMin, r.durMin, r.cutMin)
		if err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO restaurant_managers(restaurant_id, user_id) VALUES(?, 'u_ada')`, r.id)

		_, _ = tx.ExecContext(ctx, `DELETE FROM opening_hours WHERE restaurant_id = ?`, r.id)
		_, _ = tx.ExecContext(ctx, `DELETE FROM tables WHERE restaurant_id = ?`, r.id)
		_, _ = tx.ExecContext(ctx, `DELETE FROM combinable_tables WHERE restaurant_id = ?`, r.id)

		var openHours []contracts.OpeningHour
		for _, w := range weekdays {
			_, err := tx.ExecContext(ctx, `INSERT INTO opening_hours(restaurant_id, weekday, opens, closes) VALUES(?, ?, ?, ?)`, r.id, w, r.opens, r.closes)
			if err != nil {
				return err
			}
			openHours = append(openHours, contracts.OpeningHour{Weekday: w, Opens: r.opens, Closes: r.closes})
		}

		caps := make(map[string]int)
		for idx, t := range r.tables {
			_, err := tx.ExecContext(ctx, `INSERT INTO tables(id, restaurant_id, label, capacity, sort_order) VALUES(?, ?, ?, ?, ?)`, t.id, r.id, t.label, t.capacity, idx)
			if err != nil {
				return err
			}
			caps[t.id] = t.capacity
		}

		for idx, pair := range r.comb {
			if len(pair) == 2 {
				_, _ = tx.ExecContext(ctx, `INSERT INTO combinable_tables(restaurant_id, table_a, table_b, sort_order) VALUES(?, ?, ?, ?)`, r.id, pair[0], pair[1], idx)
			}
		}

		// Policy 0
		hoursJSON, _ := json.Marshal(openHours)
		capsJSON, _ := json.Marshal(caps)
		_, _ = tx.ExecContext(ctx, `INSERT OR REPLACE INTO policies(restaurant_id, policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json, created_at)
			VALUES(?, 0, '1970-01-01', ?, ?, ?, ?, ?, ?)`,
			r.id, r.slotMin, r.durMin, r.cutMin, string(hoursJSON), string(capsJSON), time.Now().Unix())
	}

	return tx.Commit()
}

const SchemaSQL = `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
    token TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS restaurants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    timezone TEXT NOT NULL,
    slot_minutes INTEGER NOT NULL,
    reservation_duration_minutes INTEGER NOT NULL,
    cancellation_cutoff_minutes INTEGER NOT NULL,
    revision INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS restaurant_managers (
    restaurant_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    PRIMARY KEY(restaurant_id, user_id),
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS opening_hours (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    restaurant_id TEXT NOT NULL,
    weekday TEXT NOT NULL,
    opens TEXT NOT NULL,
    closes TEXT NOT NULL,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tables (
    id TEXT NOT NULL,
    restaurant_id TEXT NOT NULL,
    label TEXT NOT NULL,
    capacity INTEGER NOT NULL,
    sort_order INTEGER NOT NULL,
    PRIMARY KEY(restaurant_id, id),
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS combinable_tables (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    restaurant_id TEXT NOT NULL,
    table_a TEXT NOT NULL,
    table_b TEXT NOT NULL,
    sort_order INTEGER NOT NULL,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS policies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    restaurant_id TEXT NOT NULL,
    policy_version INTEGER NOT NULL,
    effective_from TEXT NOT NULL,
    slot_minutes INTEGER NOT NULL,
    reservation_duration_minutes INTEGER NOT NULL,
    cancellation_cutoff_minutes INTEGER NOT NULL,
    opening_hours_json TEXT NOT NULL,
    capacities_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(restaurant_id, policy_version),
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_policies_lookup ON policies(restaurant_id, effective_from DESC, policy_version DESC);

CREATE TABLE IF NOT EXISTS reservations (
    id TEXT PRIMARY KEY,
    reference TEXT UNIQUE NOT NULL,
    restaurant_id TEXT NOT NULL,
    table_id TEXT,
    table_ids_json TEXT NOT NULL,
    user_id TEXT NOT NULL,
    party_size INTEGER NOT NULL,
    status TEXT NOT NULL,
    starts_at_local TEXT NOT NULL,
    starts_at_utc INTEGER NOT NULL,
    ends_at_utc INTEGER NOT NULL,
    created_at_utc INTEGER NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    accepted_terms_json TEXT,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id)
);

CREATE TABLE IF NOT EXISTS reservation_tables (
    reservation_id TEXT NOT NULL,
    table_id TEXT NOT NULL,
    sort_order INTEGER NOT NULL,
    PRIMARY KEY(reservation_id, table_id),
    FOREIGN KEY(reservation_id) REFERENCES reservations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_res_tab_table ON reservation_tables(table_id);
CREATE INDEX IF NOT EXISTS idx_res_table_time ON reservations(restaurant_id, status, starts_at_utc, ends_at_utc);
CREATE INDEX IF NOT EXISTS idx_res_user ON reservations(user_id, starts_at_utc DESC);

CREATE TABLE IF NOT EXISTS reservation_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    reservation_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    at_utc INTEGER NOT NULL,
    event TEXT NOT NULL,
    changes_json TEXT NOT NULL,
    revision INTEGER NOT NULL,
    accepted_terms_json TEXT NOT NULL,
    plan_id TEXT,
    FOREIGN KEY(reservation_id) REFERENCES reservations(id) ON DELETE CASCADE,
    UNIQUE(reservation_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_history_res ON reservation_history(reservation_id, seq ASC);

CREATE TABLE IF NOT EXISTS series (
    id TEXT PRIMARY KEY,
    restaurant_id TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    interval_weeks INTEGER NOT NULL,
    created_at_utc INTEGER NOT NULL,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id),
    FOREIGN KEY(owner_user_id) REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS series_occurrences (
    series_id TEXT NOT NULL,
    idx INTEGER NOT NULL,
    reservation_id TEXT NOT NULL UNIQUE,
    reference TEXT NOT NULL,
    exception INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(series_id, idx),
    FOREIGN KEY(series_id) REFERENCES series(id) ON DELETE CASCADE,
    FOREIGN KEY(reservation_id) REFERENCES reservations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_series_occ_res ON series_occurrences(reservation_id);

CREATE TABLE IF NOT EXISTS table_closures (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    restaurant_id TEXT NOT NULL,
    table_id TEXT NOT NULL,
    from_utc INTEGER NOT NULL,
    to_utc INTEGER NOT NULL,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_closures_lookup ON table_closures(restaurant_id, table_id, from_utc, to_utc);

CREATE TABLE IF NOT EXISTS table_replans (
    plan_id TEXT PRIMARY KEY,
    restaurant_id TEXT NOT NULL,
    restaurant_revision INTEGER NOT NULL,
    closure_table_id TEXT NOT NULL,
    closure_from_utc INTEGER NOT NULL,
    closure_to_utc INTEGER NOT NULL,
    closure_from_str TEXT NOT NULL,
    closure_to_str TEXT NOT NULL,
    assignments_json TEXT NOT NULL,
    moved_count INTEGER NOT NULL,
    unused_seats INTEGER NOT NULL,
    applied INTEGER NOT NULL DEFAULT 0,
    applied_key TEXT,
    created_at_utc INTEGER NOT NULL,
    FOREIGN KEY(restaurant_id) REFERENCES restaurants(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS idempotency (
    user_id TEXT NOT NULL,
    key TEXT NOT NULL,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    body_hash TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    response_body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, key, method, path)
);

CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    action TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    currency TEXT NOT NULL,
    balance INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS payment_intents (
    intent_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    amount INTEGER NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(account_id) REFERENCES accounts(id)
);
`
