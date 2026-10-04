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
		{
			id: "r_anker", name: "JOEY Bellevue", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "11:00", closes: "23:30",
			tables: []seedTable{
				{"t_1", "Patio Booth 1", 2},
				{"t_2", "Patio Booth 2", 2},
				{"t_3", "Main Dining 3", 4},
				{"t_4", "Main Dining 4", 4},
				{"t_5", "Lounge Table 5", 6},
				{"t_6", "Sommelier Table 6", 8},
				{"t_7", "Window Table 7", 2},
				{"t_8", "Chef Counter 8", 4},
			},
			comb: [][]string{{"t_1", "t_2"}, {"t_3", "t_4"}},
		},
		{
			id: "r_spinasse", name: "Spinasse", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 120, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"sp_1", "Pasta Bar 1", 2},
				{"sp_2", "Rustic Table 2", 2},
				{"sp_3", "Cantina 3", 4},
				{"sp_4", "Cantina 4", 4},
				{"sp_5", "Wine Table 5", 6},
				{"sp_6", "Piedmontese Room 6", 8},
			},
			comb: [][]string{{"sp_3", "sp_4"}},
		},
		{
			id: "r_kashiba", name: "Sushi Kashiba", timezone: "America/Los_Angeles", slotMin: 30, durMin: 120, cutMin: 240, opens: "17:00", closes: "22:00",
			tables: []seedTable{
				{"sk_1", "Omakase Bar 1", 2},
				{"sk_2", "Omakase Bar 2", 2},
				{"sk_3", "Elliott Bay 3", 4},
				{"sk_4", "Elliott Bay 4", 4},
				{"sk_5", "Master Shiro Salon 5", 6},
				{"sk_6", "Courtyard Table 6", 8},
			},
			comb: [][]string{{"sk_3", "sk_4"}},
		},
		{
			id: "r_communion", name: "COMMUNION Restaurant & Bar", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "16:30", closes: "22:00",
			tables: []seedTable{
				{"cm_1", "Soul Booth 1", 2},
				{"cm_2", "Soul Booth 2", 2},
				{"cm_3", "Central Table 3", 4},
				{"cm_4", "Central Table 4", 4},
				{"cm_5", "Kristi Family Table 5", 6},
				{"cm_6", "Community Table 6", 8},
			},
			comb: [][]string{{"cm_3", "cm_4"}},
		},
		{
			id: "r_pinkdoor", name: "The Pink Door", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "11:30", closes: "23:00",
			tables: []seedTable{
				{"pd_1", "Cabaret Front 1", 2},
				{"pd_2", "Trapeze View 2", 2},
				{"pd_3", "Post Alley Deck 3", 4},
				{"pd_4", "Post Alley Deck 4", 4},
				{"pd_5", "Wine Cellar 5", 6},
				{"pd_6", "Piazza Table 6", 8},
			},
			comb: [][]string{{"pd_3", "pd_4"}},
		},
		{
			id: "r_palace", name: "Palace Kitchen", timezone: "America/Los_Angeles", slotMin: 15, durMin: 90, cutMin: 60, opens: "16:00", closes: "23:59",
			tables: []seedTable{
				{"pk_1", "Horseshoe Bar 1", 2},
				{"pk_2", "Hearth Table 2", 2},
				{"pk_3", "Rotisserie Table 3", 4},
				{"pk_4", "Rotisserie Table 4", 4},
				{"pk_5", "Belltown Salon 5", 6},
				{"pk_6", "Captain Table 6", 8},
			},
			comb: [][]string{{"pk_3", "pk_4"}},
		},
		{
			id: "r_canlis", name: "Canlis", timezone: "America/Los_Angeles", slotMin: 30, durMin: 150, cutMin: 1440, opens: "17:00", closes: "23:00",
			tables: []seedTable{
				{"cn_1", "Lake Union View 1", 2},
				{"cn_2", "Cascade View 2", 2},
				{"cn_3", "Mid-Century Hearth 3", 4},
				{"cn_4", "Piano Salon 4", 4},
				{"cn_5", "Peter Canlis Room 5", 6},
				{"cn_6", "Wine Vault 6", 8},
			},
			comb: [][]string{{"cn_3", "cn_4"}},
		},
		{
			id: "r_ascend", name: "Ascend Prime Steak & Sushi", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "16:30", closes: "23:00",
			tables: []seedTable{
				{"as_1", "31st Skyline 1", 2},
				{"as_2", "Mt. Rainier View 2", 2},
				{"as_3", "Robata Counter 3", 4},
				{"as_4", "Penthouse Booth 4", 4},
				{"as_5", "Sky Lounge 5", 6},
				{"as_6", "Presidential Suite 6", 8},
			},
			comb: [][]string{{"as_3", "as_4"}},
		},
		{
			id: "r_elgaucho", name: "El Gaucho Seattle", timezone: "America/Los_Angeles", slotMin: 15, durMin: 120, cutMin: 120, opens: "17:00", closes: "22:30",
			tables: []seedTable{
				{"eg_1", "Charcoal Grill 1", 2},
				{"eg_2", "Steinway Piano 2", 2},
				{"eg_3", "Captain Table 3", 4},
				{"eg_4", "Vintage Booth 4", 4},
				{"eg_5", "Sommelier Vault 5", 6},
				{"eg_6", "Pampas Salon 6", 8},
			},
			comb: [][]string{{"eg_3", "eg_4"}},
		},
		{
			id: "r_walrus", name: "The Walrus and the Carpenter", timezone: "America/Los_Angeles", slotMin: 15, durMin: 75, cutMin: 60, opens: "16:00", closes: "22:00",
			tables: []seedTable{
				{"wc_1", "Zinc Oyster Bar 1", 2},
				{"wc_2", "Zinc Oyster Bar 2", 2},
				{"wc_3", "Maritime Table 3", 4},
				{"wc_4", "Ballard Courtyard 4", 4},
				{"wc_5", "Fisherman Table 5", 6},
				{"wc_6", "Harbor Banquette 6", 8},
			},
			comb: [][]string{{"wc_3", "wc_4"}},
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
