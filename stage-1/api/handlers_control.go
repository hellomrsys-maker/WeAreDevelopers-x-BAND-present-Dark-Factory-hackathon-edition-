package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/store"
)

var validReferenceRegex = regexp.MustCompile(`^[A-Z0-9]{6,12}$`)

type TestHandler struct {
	store    *store.Store
	calendar contracts.CalendarEngine
}

func NewTestHandler(s *store.Store, c contracts.CalendarEngine) *TestHandler {
	return &TestHandler{store: s, calendar: c}
}

func (h *TestHandler) Health(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (h *TestHandler) Reset(w http.ResponseWriter, r *http.Request) {
	var fixture contracts.FixtureData
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&fixture); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "invalid JSON fixture")
		return
	}

	// Validate IDs <= 64 chars
	for _, u := range fixture.Users {
		if len(u.ID) > 64 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "user id exceeds 64 characters")
			return
		}
	}
	for _, rest := range fixture.Restaurants {
		if len(rest.ID) > 64 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "restaurant id exceeds 64 characters")
			return
		}
		for _, t := range rest.Tables {
			if len(t.ID) > 64 {
				WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table id exceeds 64 characters")
				return
			}
		}
	}

	// Validate seeded reservations
	for i := range fixture.Reservations {
		res := &fixture.Reservations[i]
		if len(res.ID) > 64 || len(res.RestaurantID) > 64 || len(res.TableID) > 64 || len(res.UserID) > 64 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "reservation id exceeds 64 characters")
			return
		}
		if !validReferenceRegex.MatchString(res.Reference) {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid reservation reference format")
			return
		}

		// Find restaurant for timezone and duration
		var rest *contracts.Restaurant
		for j := range fixture.Restaurants {
			if fixture.Restaurants[j].ID == res.RestaurantID {
				rest = &fixture.Restaurants[j]
				break
			}
		}
		if rest != nil {
			st, err := h.calendar.ParseLocalTime(rest.Timezone, res.StartsAtLocal)
			if err == nil {
				res.StartsAt = st
				res.EndsAt = st.Add(time.Duration(rest.ReservationDurationMinutes) * time.Minute)
			}
		}
		if res.Status == "" {
			res.Status = "confirmed"
		}
		if res.CreatedAt.IsZero() {
			res.CreatedAt = time.Now().UTC()
		}
	}

	if err := h.store.Reset(r.Context(), &fixture); err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *TestHandler) Export(w http.ResponseWriter, r *http.Request) {
	h.store.RLock()
	defer h.store.RUnlock()

	// Export users
	var users []contracts.User
	uRows, err := h.store.DB().QueryContext(r.Context(), "SELECT id, email, password_hash, display_name FROM users")
	if err == nil {
		defer uRows.Close()
		for uRows.Next() {
			var u contracts.User
			_ = uRows.Scan(&u.ID, &u.Email, &u.Password, &u.DisplayName)
			users = append(users, u)
		}
	}

	// Export tokens
	tokens := make(map[string]string)
	tokRows, err := h.store.DB().QueryContext(r.Context(), "SELECT token, user_id FROM tokens")
	if err == nil {
		defer tokRows.Close()
		for tokRows.Next() {
			var t, uid string
			_ = tokRows.Scan(&t, &uid)
			tokens[t] = uid
		}
	}

	// Export restaurants
	restaurants, _ := h.store.ListRestaurants(r.Context())
	var fullRestaurants []contracts.Restaurant
	for _, rest := range restaurants {
		full, err := h.store.GetRestaurant(r.Context(), rest.ID)
		if err == nil && full != nil {
			fullRestaurants = append(fullRestaurants, *full)
		}
	}

	// Export reservations
	var reservations []contracts.Reservation
	rRows, err := h.store.DB().QueryContext(r.Context(), `
		SELECT id, reference, restaurant_id, table_id, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc
		FROM reservations`)
	if err == nil {
		defer rRows.Close()
		for rRows.Next() {
			var res contracts.Reservation
			var sU, eU, cU int64
			_ = rRows.Scan(&res.ID, &res.Reference, &res.RestaurantID, &res.TableID, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU)
			res.StartsAt = time.Unix(sU, 0)
			res.EndsAt = time.Unix(eU, 0)
			res.CreatedAt = time.Unix(cU, 0)
			res.StartsAtUTC = sU
			res.EndsAtUTC = eU
			res.CreatedAtUTC = cU
			reservations = append(reservations, res)
		}
	}

	// Export idempotency
	var idempotency []contracts.IdempotencyItem
	iRows, err := h.store.DB().QueryContext(r.Context(), `
		SELECT user_id, key, method, path, body_hash, status_code, response_body
		FROM idempotency`)
	if err == nil {
		defer iRows.Close()
		for iRows.Next() {
			var item contracts.IdempotencyItem
			_ = iRows.Scan(&item.UserID, &item.Key, &item.Method, &item.Path, &item.BodyHash, &item.StatusCode, &item.ResponseBody)
			idempotency = append(idempotency, item)
		}
	}

	payload := contracts.ExportPayload{
		Track:         "tablekeeper",
		FormatVersion: 1,
		State: contracts.ExportState{
			Users:        users,
			Tokens:       tokens,
			Restaurants:  fullRestaurants,
			Reservations: reservations,
			Idempotency:  idempotency,
		},
	}

	WriteJSON(w, http.StatusOK, payload)
}

func (h *TestHandler) Import(w http.ResponseWriter, r *http.Request) {
	var payload contracts.ExportPayload
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&payload); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "invalid import JSON")
		return
	}

	if payload.Track != "tablekeeper" || payload.FormatVersion != 1 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "incompatible export format or track")
		return
	}

	h.store.Lock()
	defer h.store.Unlock()

	tx, err := h.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer tx.Rollback()

	// Clear all tables
	tables := []string{"audit_log", "idempotency", "reservations", "tables", "opening_hours", "restaurants", "tokens", "users"}
	for _, t := range tables {
		if _, err := tx.ExecContext(r.Context(), "DELETE FROM "+t); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore users
	for _, u := range payload.State.Users {
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO users(id, email, password_hash, display_name) VALUES(?, ?, ?, ?)", u.ID, u.Email, u.Password, u.DisplayName); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore tokens
	for tok, uid := range payload.State.Tokens {
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO tokens(token, user_id) VALUES(?, ?)", tok, uid); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore restaurants, hours, tables
	for _, rest := range payload.State.Restaurants {
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO restaurants(id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes) VALUES(?, ?, ?, ?, ?, ?)", rest.ID, rest.Name, rest.Timezone, rest.SlotMinutes, rest.ReservationDurationMinutes, rest.CancellationCutoffMinutes); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		for _, h := range rest.OpeningHours {
			if _, err := tx.ExecContext(r.Context(), "INSERT INTO opening_hours(restaurant_id, weekday, opens, closes) VALUES(?, ?, ?, ?)", rest.ID, h.Weekday, h.Opens, h.Closes); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		for i, t := range rest.Tables {
			if _, err := tx.ExecContext(r.Context(), "INSERT INTO tables(id, restaurant_id, label, capacity, sort_order) VALUES(?, ?, ?, ?, ?)", t.ID, rest.ID, t.Label, t.Capacity, i); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
	}

	// Restore reservations
	for _, res := range payload.State.Reservations {
		startU := res.StartsAtUTC
		if startU == 0 && !res.StartsAt.IsZero() {
			startU = res.StartsAt.Unix()
		}
		endU := res.EndsAtUTC
		if endU == 0 && !res.EndsAt.IsZero() {
			endU = res.EndsAt.Unix()
		}
		createdU := res.CreatedAtUTC
		if createdU == 0 && !res.CreatedAt.IsZero() {
			createdU = res.CreatedAt.Unix()
		}
		if createdU == 0 {
			createdU = time.Now().Unix()
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO reservations(id, reference, restaurant_id, table_id, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			res.ID, res.Reference, res.RestaurantID, res.TableID, res.UserID, res.PartySize, res.Status, res.StartsAtLocal, startU, endU, createdU); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore idempotency
	for _, item := range payload.State.Idempotency {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO idempotency(user_id, key, method, path, body_hash, status_code, response_body, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			item.UserID, item.Key, item.Method, item.Path, item.BodyHash, item.StatusCode, item.ResponseBody, time.Now().Unix()); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
