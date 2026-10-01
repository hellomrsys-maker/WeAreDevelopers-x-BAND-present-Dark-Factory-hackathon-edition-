package api

import (
	"database/sql"
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
		if len(res.TableIDs) == 0 && res.TableID != nil && *res.TableID != "" {
			res.TableIDs = []string{*res.TableID}
		}
		if len(res.ID) > 64 || len(res.RestaurantID) > 64 || len(res.UserID) > 64 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "reservation id exceeds 64 characters")
			return
		}
		for _, tid := range res.TableIDs {
			if len(tid) > 64 {
				WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table id exceeds 64 characters")
				return
			}
		}
		if !validReferenceRegex.MatchString(res.Reference) {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid reservation reference format")
			return
		}

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
		if res.Revision == 0 {
			res.Revision = 1
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

	// Export policies
	var policyExports []contracts.PolicyExport
	pRows, err := h.store.DB().QueryContext(r.Context(), `
		SELECT restaurant_id, policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json
		FROM policies ORDER BY restaurant_id, policy_version ASC`)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var pe contracts.PolicyExport
			var hJSON, cJSON string
			_ = pRows.Scan(&pe.RestaurantID, &pe.Policy.PolicyVersion, &pe.Policy.EffectiveFrom, &pe.Policy.SlotMinutes, &pe.Policy.ReservationDurationMinutes, &pe.Policy.CancellationCutoffMinutes, &hJSON, &cJSON)
			_ = json.Unmarshal([]byte(hJSON), &pe.Policy.OpeningHours)
			_ = json.Unmarshal([]byte(cJSON), &pe.Policy.Capacities)
			policyExports = append(policyExports, pe)
		}
	}

	// Export reservations
	var reservations []contracts.Reservation
	rRows, err := h.store.DB().QueryContext(r.Context(), `
		SELECT r.id, r.reference, r.restaurant_id, r.table_id, r.table_ids_json, r.user_id, r.party_size, r.status, r.starts_at_local, r.starts_at_utc, r.ends_at_utc, r.created_at_utc, r.revision, r.accepted_terms_json, rest.timezone
		FROM reservations r
		JOIN restaurants rest ON r.restaurant_id = rest.id`)
	if err == nil {
		defer rRows.Close()
		for rRows.Next() {
			var res contracts.Reservation
			var sU, eU, cU int64
			var tidNull sql.NullString
			var tidsJSON string
			var termsJSON sql.NullString
			var tz string
			_ = rRows.Scan(&res.ID, &res.Reference, &res.RestaurantID, &tidNull, &tidsJSON, &res.UserID, &res.PartySize, &res.Status, &res.StartsAtLocal, &sU, &eU, &cU, &res.Revision, &termsJSON, &tz)
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

			if termsJSON.Valid && termsJSON.String != "" {
				var t contracts.AcceptedTerms
				if err := json.Unmarshal([]byte(termsJSON.String), &t); err == nil {
					res.AcceptedTerms = &t
				}
			}

			reservations = append(reservations, res)
		}
	}

	// Export series
	var seriesExports []contracts.SeriesExport
	sRows, err := h.store.DB().QueryContext(r.Context(), `SELECT id, restaurant_id, owner_user_id, revision, interval_weeks FROM series`)
	if err == nil {
		defer sRows.Close()
		for sRows.Next() {
			var se contracts.SeriesExport
			_ = sRows.Scan(&se.SeriesID, &se.RestaurantID, &se.OwnerUserID, &se.Revision, &se.IntervalWeeks)

			// Occurrences
			occRows, err := h.store.DB().QueryContext(r.Context(), `SELECT idx, reference, exception FROM series_occurrences WHERE series_id = ? ORDER BY idx ASC`, se.SeriesID)
			if err == nil {
				for occRows.Next() {
					var occ contracts.SeriesOccurrence
					var excInt int
					_ = occRows.Scan(&occ.Index, &occ.Reference, &excInt)
					occ.Exception = (excInt == 1)
					se.Occurrences = append(se.Occurrences, occ)
				}
				occRows.Close()
			}
			seriesExports = append(seriesExports, se)
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
			Policies:     policyExports,
			Series:       seriesExports,
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
	tables := []string{
		"audit_log", "idempotency", "series_occurrences", "series",
		"reservation_history", "reservation_tables", "reservations",
		"policies", "combinable_tables", "tables", "opening_hours",
		"restaurant_managers", "restaurants", "tokens", "users",
	}
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

	// Restore restaurants, managers, hours, tables, and combinable pairs
	stmtComb, err := tx.PrepareContext(r.Context(), "INSERT INTO combinable_tables(restaurant_id, table_a, table_b, sort_order) VALUES(?, ?, ?, ?)")
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer stmtComb.Close()

	stmtMgr, err := tx.PrepareContext(r.Context(), "INSERT INTO restaurant_managers(restaurant_id, user_id) VALUES(?, ?)")
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer stmtMgr.Close()

	restMap := make(map[string]*contracts.Restaurant)
	for i := range payload.State.Restaurants {
		rest := &payload.State.Restaurants[i]
		restMap[rest.ID] = rest
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO restaurants(id, name, timezone, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes) VALUES(?, ?, ?, ?, ?, ?)", rest.ID, rest.Name, rest.Timezone, rest.SlotMinutes, rest.ReservationDurationMinutes, rest.CancellationCutoffMinutes); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		for _, uid := range rest.ManagerUserIDs {
			if _, err := stmtMgr.ExecContext(r.Context(), rest.ID, uid); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		for _, h := range rest.OpeningHours {
			if _, err := tx.ExecContext(r.Context(), "INSERT INTO opening_hours(restaurant_id, weekday, opens, closes) VALUES(?, ?, ?, ?)", rest.ID, h.Weekday, h.Opens, h.Closes); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		for idx, t := range rest.Tables {
			if _, err := tx.ExecContext(r.Context(), "INSERT INTO tables(id, restaurant_id, label, capacity, sort_order) VALUES(?, ?, ?, ?, ?)", t.ID, rest.ID, t.Label, t.Capacity, idx); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		for idx, pair := range rest.Combinable {
			if len(pair) == 2 {
				if _, err := stmtComb.ExecContext(r.Context(), rest.ID, pair[0], pair[1], idx); err != nil {
					WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
					return
				}
			}
		}
	}

	// Restore policies if present
	for _, pe := range payload.State.Policies {
		hJSON, _ := json.Marshal(pe.Policy.OpeningHours)
		cJSON, _ := json.Marshal(pe.Policy.Capacities)
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO policies(restaurant_id, policy_version, effective_from, slot_minutes, reservation_duration_minutes, cancellation_cutoff_minutes, opening_hours_json, capacities_json, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			pe.RestaurantID, pe.Policy.PolicyVersion, pe.Policy.EffectiveFrom, pe.Policy.SlotMinutes, pe.Policy.ReservationDurationMinutes, pe.Policy.CancellationCutoffMinutes, string(hJSON), string(cJSON), time.Now().Unix()); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore reservations
	stmtRes, err := tx.PrepareContext(r.Context(), `
		INSERT INTO reservations(id, reference, restaurant_id, table_id, table_ids_json, user_id, party_size, status, starts_at_local, starts_at_utc, ends_at_utc, created_at_utc, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer stmtRes.Close()

	stmtResTab, err := tx.PrepareContext(r.Context(), `INSERT INTO reservation_tables(reservation_id, table_id, sort_order) VALUES(?, ?, ?)`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer stmtResTab.Close()

	stmtHist, err := tx.PrepareContext(r.Context(), `
		INSERT INTO reservation_history(reservation_id, seq, at_utc, event, changes_json, revision, accepted_terms_json)
		VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer stmtHist.Close()

	for _, res := range payload.State.Reservations {
		tIDs := res.TableIDs
		if len(tIDs) == 0 && res.TableID != nil && *res.TableID != "" {
			tIDs = []string{*res.TableID}
		}

		var singleTID *string
		if len(tIDs) == 1 {
			singleTID = &tIDs[0]
		}
		rawJSON, _ := json.Marshal(tIDs)

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
			terms = store.BuildPolicyZero(r)
		}
		termsJSON, _ := json.Marshal(terms)

		if _, err := stmtRes.ExecContext(r.Context(),
			res.ID, res.Reference, res.RestaurantID, singleTID, string(rawJSON), res.UserID, res.PartySize, status, res.StartsAtLocal, startU, endU, createdU, revision, string(termsJSON)); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		for idx, tid := range tIDs {
			if _, err := stmtResTab.ExecContext(r.Context(), res.ID, tid, idx); err != nil {
				WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}

		// Initial history entry if importing stage 1/2 data
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
		if _, err := stmtHist.ExecContext(r.Context(), res.ID, 1, createdU, "created", string(changesJSON), revision, string(termsJSON)); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	// Restore series if present
	for _, se := range payload.State.Series {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO series(id, restaurant_id, owner_user_id, revision, interval_weeks, created_at_utc)
			VALUES(?, ?, ?, ?, ?, ?)`,
			se.SeriesID, se.RestaurantID, se.OwnerUserID, se.Revision, se.IntervalWeeks, time.Now().Unix()); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		for _, occ := range se.Occurrences {
			excInt := 0
			if occ.Exception {
				excInt = 1
			}
			// Find reservation_id for reference
			var rID string
			_ = tx.QueryRowContext(r.Context(), `SELECT id FROM reservations WHERE reference = ?`, occ.Reference).Scan(&rID)
			if rID != "" {
				if _, err := tx.ExecContext(r.Context(), `
					INSERT INTO series_occurrences(series_id, idx, reservation_id, reference, exception)
					VALUES(?, ?, ?, ?, ?)`,
					se.SeriesID, occ.Index, rID, occ.Reference, excInt); err != nil {
					WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
					return
				}
			}
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
