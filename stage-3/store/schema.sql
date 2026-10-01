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
