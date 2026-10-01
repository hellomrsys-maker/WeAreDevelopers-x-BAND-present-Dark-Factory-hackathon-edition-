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

	// Recommended SQLite pragmas for high concurrency
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

	// Execute schema
	if _, err := db.Exec(SchemaSQL); err != nil {
		return nil, fmt.Errorf("failed to init schema: %w", err)
	}

	return &Store{db: db}, nil
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

// BuildPolicyZero constructs the default Policy 0 from a restaurant fixture
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

// Reset wipes all tables and loads the fixture inside an atomic transaction
func (s *Store) Reset(ctx context.Context, fixture *contracts.FixtureData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clear all tables in dependency order
	tables := []string{
		"audit_log", "idempotency", "series_occurrences", "series",
		"reservation_history", "reservation_tables", "reservations",
		"policies", "combinable_tables", "tables", "opening_hours",
		"restaurant_managers", "restaurants", "tokens", "users",
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s", t)); err != nil {
			return err
		}
	}

	// Insert users
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

	// Insert restaurants, managers, hours, tables, and combinable pairs
	stmtRest, err := tx.PrepareContext(ctx, `INSERT INTO restaurants(id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes) VALUES(?, ?, ?, ?, ?, ?)`)
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

	// Insert seeded reservations
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

		// Initial "created" history entry for seeded reservation
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
	row := s.db.QueryRowContext(ctx, "SELECT id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes FROM restaurants WHERE id = ?", id)
	var r contracts.Restaurant
	if err := row.Scan(&r.ID, &r.Name, &r.Timezone, &r.SlotMinutes, &r.ReservationDurationMinutes, &r.CancellationCutoffMinutes); err != nil {
		return nil, err
	}

	// Managers
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

	// Hours
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

	// Tables in fixture order
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

	// Combinable pairs in declared order
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
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, timezone FROM restaurants ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*contracts.Restaurant
	for rows.Next() {
		var r contracts.Restaurant
		if err := rows.Scan(&r.ID, &r.Name, &r.Timezone); err != nil {
			return nil, err
		}
		list = append(list, &r)
	}
	return list, nil
}

// PublishPolicy allocates the next policy_version and saves the policy
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

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	p.PolicyVersion = nextVersion
	return p, nil
}

// ListPolicies returns published policies in publication order (omitting policy 0)
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

// GetEffectivePolicy selects the greatest effective_from <= dateStr, ties choose greatest policy_version. Falls back to Policy 0.
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

	// Fallback to Policy 0 from restaurant fixture
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

// GetReservationHistory retrieves the entries for a reservation ordered by seq ASC
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
		SELECT seq, at_utc, event, changes_json, revision, accepted_terms_json
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
		if err := rows.Scan(&e.Seq, &atU, &e.Event, &changesJSON, &e.Revision, &termsJSON); err != nil {
			return nil, err
		}
		e.At = time.Unix(atU, 0).In(loc).Format(time.RFC3339)
		_ = json.Unmarshal([]byte(changesJSON), &e.Changes)
		if e.Changes == nil {
			e.Changes = []contracts.HistoryChange{}
		}
		_ = json.Unmarshal([]byte(termsJSON), &e.AcceptedTerms)
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []contracts.HistoryEntry{}
	}
	return entries, nil
}

// AppendHistory appends a new entry to reservation_history with next seq
func (s *Store) AppendHistory(ctx context.Context, tx *sql.Tx, reservationID string, atUTC int64, event string, changes []contracts.HistoryChange, revision int, terms contracts.AcceptedTerms) error {
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
		INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		reservationID, nextSeq, atUTC, event, string(changesJSON), revision, string(termsJSON))
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
    cancellation_cutoff_minutes INTEGER NOT NULL
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
`
