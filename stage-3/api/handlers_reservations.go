package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/engines/booking"
	"tablekeeper/engines/calendar"
)

type ReservationsHandler struct {
	booking  contracts.BookingEngine
	calendar contracts.CalendarEngine
}

func NewReservationsHandler(b contracts.BookingEngine, c contracts.CalendarEngine) *ReservationsHandler {
	return &ReservationsHandler{booking: b, calendar: c}
}

func (h *ReservationsHandler) Create(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	var raw map[string]interface{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "unparseable json")
		return
	}

	ridRaw, ok1 := raw["restaurant_id"].(string)
	localRaw, ok2 := raw["starts_at_local"].(string)
	partyRaw, ok3 := raw["party_size"]

	if !ok1 || !ok2 || !ok3 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "missing required reservation fields")
		return
	}

	// Must have either table_id or table_ids, but NOT both
	_, hasSingle := raw["table_id"]
	_, hasMultiple := raw["table_ids"]
	if (hasSingle && hasMultiple) || (!hasSingle && !hasMultiple) {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "must specify either table_id or table_ids")
		return
	}

	var tableIDs []string
	if hasSingle {
		tidStr, ok := raw["table_id"].(string)
		if !ok || tidStr == "" {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table_id must be a non-empty string")
			return
		}
		tableIDs = []string{tidStr}
	} else {
		tidsRaw, ok := raw["table_ids"].([]interface{})
		if !ok || len(tidsRaw) == 0 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table_ids must be a non-empty array")
			return
		}
		for _, item := range tidsRaw {
			s, okStr := item.(string)
			if !okStr || s == "" {
				WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table_ids must contain non-empty strings")
				return
			}
			tableIDs = append(tableIDs, s)
		}
	}

	partyFloat, okNum := partyRaw.(float64)
	if !okNum || partyFloat != float64(int(partyFloat)) || partyFloat < 1 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "party_size must be a positive integer")
		return
	}
	partySize := int(partyFloat)

	res, err := h.booking.CreateReservation(r.Context(), contracts.CreateReservationParams{
		RestaurantID:  ridRaw,
		TableIDs:      tableIDs,
		StartsAtLocal: localRaw,
		PartySize:     partySize,
		UserID:        user.ID,
		Now:           time.Now().UTC(),
	})
	if err != nil {
		handleBookingError(w, err)
		return
	}

	WriteJSON(w, http.StatusCreated, formatReservationResponse(res, h.calendar))
}

func (h *ReservationsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	list, err := h.booking.ListReservations(r.Context(), user.ID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	var resList []map[string]interface{}
	for _, res := range list {
		resList = append(resList, formatReservationResponse(res, h.calendar))
	}
	if resList == nil {
		resList = []map[string]interface{}{}
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"reservations": resList,
	})
}

func (h *ReservationsHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	ref := strings.TrimPrefix(r.URL.Path, "/reservations/")
	if ref == "" || strings.Contains(ref, "/") {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	res, err := h.booking.GetReservation(r.Context(), ref)
	if err != nil || res == nil || res.UserID != user.ID {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	WriteJSON(w, http.StatusOK, formatReservationResponse(res, h.calendar))
}

func (h *ReservationsHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/reservations/")
	ref := strings.TrimSuffix(path, "/cancel")

	existing, err := h.booking.GetReservation(r.Context(), ref)
	if err != nil || existing == nil || existing.UserID != user.ID {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	cancelled, err := h.booking.CancelReservation(r.Context(), ref, time.Now().UTC())
	if err != nil {
		handleBookingError(w, err)
		return
	}

	WriteJSON(w, http.StatusOK, formatReservationResponse(cancelled, h.calendar))
}

func (h *ReservationsHandler) Patch(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	ref := strings.TrimPrefix(r.URL.Path, "/reservations/")

	existing, err := h.booking.GetReservation(r.Context(), ref)
	if err != nil || existing == nil || existing.UserID != user.ID {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	var raw map[string]interface{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "unparseable json")
		return
	}

	var patch contracts.PatchReservationParams
	_, hasSingle := raw["table_id"]
	_, hasMultiple := raw["table_ids"]
	if hasSingle && hasMultiple {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "cannot send both table_id and table_ids")
		return
	}

	if hasSingle {
		if s, okStr := raw["table_id"].(string); okStr {
			patch.TableID = &s
		} else {
			WriteError(w, http.StatusBadRequest, "malformed_request", "invalid table_id type")
			return
		}
	} else if hasMultiple {
		if arr, okArr := raw["table_ids"].([]interface{}); okArr {
			for _, item := range arr {
				if s, okStr := item.(string); okStr {
					patch.TableIDs = append(patch.TableIDs, s)
				} else {
					WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid table_ids item")
					return
				}
			}
		} else {
			WriteError(w, http.StatusBadRequest, "malformed_request", "invalid table_ids type")
			return
		}
	}

	if pVal, ok := raw["party_size"]; ok {
		if f, okF := pVal.(float64); okF && f == float64(int(f)) && f >= 1 {
			intParty := int(f)
			patch.PartySize = &intParty
		} else {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid party_size")
			return
		}
	}
	if lVal, ok := raw["starts_at_local"]; ok {
		if s, okStr := lVal.(string); okStr {
			patch.StartsAtLocal = &s
		} else {
			WriteError(w, http.StatusBadRequest, "malformed_request", "invalid starts_at_local type")
			return
		}
	}

	if expVal, ok := raw["expected_revision"]; ok {
		if f, okF := expVal.(float64); okF && f == float64(int(f)) && f > 0 {
			expInt := int(f)
			patch.ExpectedRevision = &expInt
		} else {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "expected_revision must be positive integer")
			return
		}
	}

	updated, err := h.booking.PatchReservation(r.Context(), ref, patch, time.Now().UTC())
	if err != nil {
		handleBookingError(w, err)
		return
	}

	WriteJSON(w, http.StatusOK, formatReservationResponse(updated, h.calendar))
}

func (h *ReservationsHandler) Decision(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/reservations/")
	ref := strings.TrimSuffix(path, "/decision")

	user := GetUserFromContext(r.Context())
	var uid string
	if user != nil {
		uid = user.ID
	}

	decision, err := h.booking.GetDecision(r.Context(), ref, uid)
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	WriteJSON(w, http.StatusOK, decision)
}

func (h *ReservationsHandler) History(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/reservations/")
	ref := strings.TrimSuffix(path, "/history")

	user := GetUserFromContext(r.Context())
	var uid string
	if user != nil {
		uid = user.ID
	}

	history, err := h.booking.GetHistory(r.Context(), ref, uid)
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	WriteJSON(w, http.StatusOK, history)
}

func (h *ReservationsHandler) CreateSeries(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	var raw map[string]interface{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "invalid JSON")
		return
	}

	refRaw, ok1 := raw["anchor_reference"].(string)
	cntRaw, ok2 := raw["count"].(float64)
	intRaw, ok3 := raw["interval_weeks"].(float64)

	if !ok1 || !ok2 || !ok3 || float64(int(cntRaw)) != cntRaw || float64(int(intRaw)) != intRaw {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid series fields")
		return
	}

	req := contracts.CreateSeriesRequest{
		AnchorReference: refRaw,
		Count:           int(cntRaw),
		IntervalWeeks:   int(intRaw),
	}

	seriesResp, err := h.booking.CreateSeries(r.Context(), user.ID, req, time.Now().UTC())
	if err != nil {
		handleBookingError(w, err)
		return
	}

	WriteJSON(w, http.StatusCreated, formatSeriesResponse(seriesResp, h.calendar))
}

func (h *ReservationsHandler) GetSeries(w http.ResponseWriter, r *http.Request) {
	seriesID := strings.TrimPrefix(r.URL.Path, "/series/")
	if seriesID == "" || strings.Contains(seriesID, "/") {
		WriteError(w, http.StatusNotFound, "not_found", "series not found")
		return
	}

	user := GetUserFromContext(r.Context())
	var uid string
	if user != nil {
		uid = user.ID
	}

	seriesResp, err := h.booking.GetSeries(r.Context(), seriesID, uid)
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", "series not found")
		return
	}

	WriteJSON(w, http.StatusOK, formatSeriesResponse(seriesResp, h.calendar))
}

type MovesRequestBody struct {
	Moves []struct {
		Reference        *string      `json:"reference"`
		TableID          *string      `json:"table_id"`
		TableIDs         *[]string    `json:"table_ids"`
		StartsAtLocal    *string      `json:"starts_at_local"`
		PartySize        *interface{} `json:"party_size"`
		ExpectedRevision *interface{} `json:"expected_revision"`
	} `json:"moves"`
}

func (h *ReservationsHandler) Moves(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}

	var body MovesRequestBody
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "unparseable json")
		return
	}

	if len(body.Moves) == 0 || len(body.Moves) > 8 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "moves must contain 1 to 8 items")
		return
	}

	var moves []contracts.ReservationMoveRequest
	for _, m := range body.Moves {
		if m.Reference == nil || *m.Reference == "" {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "reference is required")
			return
		}
		if m.TableID != nil && m.TableIDs != nil {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "cannot provide both table_id and table_ids")
			return
		}

		item := contracts.ReservationMoveRequest{
			Reference:     *m.Reference,
			TableID:       m.TableID,
			StartsAtLocal: m.StartsAtLocal,
		}
		if m.TableIDs != nil {
			item.TableIDs = *m.TableIDs
		}

		if m.PartySize != nil {
			if f, ok := (*m.PartySize).(float64); ok && f == float64(int(f)) && f >= 1 {
				p := int(f)
				item.PartySize = &p
			} else {
				WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid party_size in moves")
				return
			}
		}

		if m.ExpectedRevision != nil {
			if f, ok := (*m.ExpectedRevision).(float64); ok && f == float64(int(f)) && f > 0 {
				rev := int(f)
				item.ExpectedRevision = &rev
			} else {
				WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid expected_revision in moves")
				return
			}
		}

		moves = append(moves, item)
	}

	moved, err := h.booking.MoveReservations(r.Context(), user.ID, moves, time.Now().UTC())
	if err != nil {
		handleBookingError(w, err)
		return
	}

	var resList []map[string]interface{}
	for _, res := range moved {
		resList = append(resList, formatReservationResponse(res, h.calendar))
	}
	WriteJSON(w, http.StatusCreated, map[string]interface{}{
		"reservations": resList,
	})
}

func (h *ReservationsHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("reference")
	if ref == "" {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "reference is required")
		return
	}

	res, err := h.booking.GetReservation(r.Context(), ref)
	if err != nil || res == nil {
		WriteError(w, http.StatusNotFound, "not_found", "reservation not found")
		return
	}

	WriteJSON(w, http.StatusOK, formatReservationResponse(res, h.calendar))
}

func formatReservationResponse(res *contracts.Reservation, c contracts.CalendarEngine) map[string]interface{} {
	resp := map[string]interface{}{
		"reservation_id":  res.ID,
		"reference":       res.Reference,
		"restaurant_id":   res.RestaurantID,
		"table_ids":       res.TableIDs,
		"party_size":      res.PartySize,
		"status":          res.Status,
		"starts_at_local": res.StartsAtLocal,
		"starts_at":       c.FormatRFC3339(res.StartsAt),
		"ends_at":         c.FormatRFC3339(res.EndsAt),
		"created_at":      c.FormatRFC3339(res.CreatedAt),
		"revision":        res.Revision,
	}
	if len(res.TableIDs) == 1 {
		resp["table_id"] = res.TableIDs[0]
	}
	if res.AcceptedTerms != nil {
		resp["accepted_terms"] = res.AcceptedTerms
	}
	return resp
}

func formatSeriesResponse(s *contracts.SeriesResponse, c contracts.CalendarEngine) map[string]interface{} {
	var occurrences []map[string]interface{}
	for _, occ := range s.Occurrences {
		occurrences = append(occurrences, map[string]interface{}{
			"index":       occ.Index,
			"reference":   occ.Reference,
			"exception":   occ.Exception,
			"reservation": formatReservationResponse(&occ.Reservation, c),
		})
	}
	return map[string]interface{}{
		"series_id":      s.SeriesID,
		"revision":       s.Revision,
		"interval_weeks": s.IntervalWeeks,
		"occurrences":    occurrences,
	}
}

func handleBookingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, booking.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, booking.ErrForbidden):
		WriteError(w, http.StatusForbidden, "forbidden", "action forbidden")
	case errors.Is(err, booking.ErrStaleRevision):
		WriteError(w, http.StatusConflict, "stale_revision", "expected_revision does not match current reservation revision")
	case errors.Is(err, booking.ErrAlreadyInSeries):
		WriteError(w, http.StatusConflict, "already_in_series", "anchor reservation is already part of a recurring series")
	case errors.Is(err, booking.ErrTableUnavailable):
		WriteError(w, http.StatusConflict, "table_unavailable", "table unavailable for requested time")
	case errors.Is(err, booking.ErrCutoffPassed):
		WriteError(w, http.StatusConflict, "cutoff_passed", "cancellation cutoff has passed")
	case errors.Is(err, booking.ErrReservationCancelled):
		WriteError(w, http.StatusConflict, "reservation_cancelled", "reservation is already cancelled")
	case errors.Is(err, booking.ErrNotOnSlotGrid):
		WriteError(w, http.StatusUnprocessableEntity, "not_on_slot_grid", "requested time is not on the slot grid")
	case errors.Is(err, booking.ErrOutsideOpeningHours):
		WriteError(w, http.StatusUnprocessableEntity, "outside_opening_hours", "outside restaurant opening hours")
	case errors.Is(err, booking.ErrPartyExceedsCapacity):
		WriteError(w, http.StatusUnprocessableEntity, "party_exceeds_capacity", "party size exceeds table capacity")
	case errors.Is(err, booking.ErrCombinationNotAllowed):
		WriteError(w, http.StatusUnprocessableEntity, "combination_not_allowed", "combination not allowed")
	case errors.Is(err, booking.ErrInvalidLocalTime) || errors.Is(err, calendar.ErrInvalidLocalTime):
		WriteError(w, http.StatusUnprocessableEntity, "invalid_local_time", "local time does not exist due to DST transition")
	case errors.Is(err, booking.ErrValidationFailed) || errors.Is(err, calendar.ErrValidationFailed):
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "validation failed")
	default:
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
