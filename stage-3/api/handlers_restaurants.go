package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/engines/booking"
	"tablekeeper/engines/calendar"
	"tablekeeper/store"
)

var digitsOnlyRegex = regexp.MustCompile(`^[0-9]+$`)

type RestaurantsHandler struct {
	store   *store.Store
	booking contracts.BookingEngine
}

func NewRestaurantsHandler(s *store.Store, b contracts.BookingEngine) *RestaurantsHandler {
	return &RestaurantsHandler{store: s, booking: b}
}

func (h *RestaurantsHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListRestaurants(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	type restSummary struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Timezone string `json:"timezone"`
	}
	var res []restSummary
	for _, r := range list {
		res = append(res, restSummary{
			ID:       r.ID,
			Name:     r.Name,
			Timezone: r.Timezone,
		})
	}
	if res == nil {
		res = []restSummary{}
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"restaurants": res,
	})
}

func (h *RestaurantsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/restaurants/")
	if id == "" || strings.Contains(id, "/") {
		WriteError(w, http.StatusNotFound, "not_found", "restaurant not found")
		return
	}

	rest, err := h.store.GetRestaurant(r.Context(), id)
	if err != nil || rest == nil {
		WriteError(w, http.StatusNotFound, "not_found", "restaurant not found")
		return
	}

	WriteJSON(w, http.StatusOK, rest)
}

func (h *RestaurantsHandler) Availability(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	restaurantID := query.Get("restaurant_id")
	dateStr := query.Get("date")
	partySizeStr := query.Get("party_size")

	if restaurantID == "" || dateStr == "" || partySizeStr == "" {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "restaurant_id, date, and party_size are required")
		return
	}

	// Strictly validate integer digits without exponent or signs (§5)
	if !digitsOnlyRegex.MatchString(partySizeStr) {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "party_size must be plain integer digits")
		return
	}

	partySize, err := strconv.Atoi(partySizeStr)
	if err != nil || partySize < 1 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "party_size must be positive integer")
		return
	}

	// Explain parameter handling
	var explain bool
	if _, hasExplain := query["explain"]; hasExplain {
		val := query.Get("explain")
		if val != "true" {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "explain parameter must be 'true'")
			return
		}
		explain = true
	}

	result, err := h.booking.GetAvailability(r.Context(), restaurantID, dateStr, partySize, explain)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", "restaurant not found")
			return
		}
		if errors.Is(err, booking.ErrValidationFailed) || errors.Is(err, calendar.ErrValidationFailed) {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid availability query")
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, result)
}

func (h *RestaurantsHandler) PublishPolicy(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "restaurants" || parts[2] != "policies" {
		WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	restaurantID := parts[1]

	user := GetUserFromContext(r.Context())
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	var raw map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "invalid JSON")
		return
	}

	// Strict type checking for required fields: booleans are not integers
	effFromRaw, ok := raw["effective_from"].(string)
	if !ok || len(effFromRaw) != 10 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "effective_from must be YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", effFromRaw); err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "effective_from must be a valid calendar date")
		return
	}

	slotMinF, ok := raw["slot_minutes"].(float64)
	if !ok || float64(int(slotMinF)) != slotMinF {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "slot_minutes must be integer")
		return
	}
	slotMinutes := int(slotMinF)

	durMinF, ok := raw["reservation_duration_minutes"].(float64)
	if !ok || float64(int(durMinF)) != durMinF {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "reservation_duration_minutes must be integer")
		return
	}
	durationMinutes := int(durMinF)

	cutoffF, ok := raw["cancellation_cutoff_minutes"].(float64)
	if !ok || float64(int(cutoffF)) != cutoffF {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "cancellation_cutoff_minutes must be integer")
		return
	}
	cutoffMinutes := int(cutoffF)

	hoursRaw, ok := raw["opening_hours"].([]interface{})
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "opening_hours required")
		return
	}
	var hours []contracts.OpeningHour
	for _, hr := range hoursRaw {
		hMap, ok := hr.(map[string]interface{})
		if !ok {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid opening_hour")
			return
		}
		wd, okWd := hMap["weekday"].(string)
		op, okOp := hMap["opens"].(string)
		cl, okCl := hMap["closes"].(string)
		if !okWd || !okOp || !okCl {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "opening_hour fields required")
			return
		}
		hours = append(hours, contracts.OpeningHour{Weekday: wd, Opens: op, Closes: cl})
	}

	capsRaw, ok := raw["capacities"].(map[string]interface{})
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "capacities required")
		return
	}
	capacities := make(map[string]int)
	for tid, capRaw := range capsRaw {
		capF, ok := capRaw.(float64)
		if !ok || float64(int(capF)) != capF {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "table capacity must be integer")
			return
		}
		capacities[tid] = int(capF)
	}

	policyReq := contracts.Policy{
		EffectiveFrom:              effFromRaw,
		SlotMinutes:                slotMinutes,
		ReservationDurationMinutes: durationMinutes,
		CancellationCutoffMinutes:  cutoffMinutes,
		OpeningHours:               hours,
		Capacities:                 capacities,
	}

	published, err := h.booking.PublishPolicy(r.Context(), restaurantID, user.ID, &policyReq)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", "restaurant not found")
			return
		}
		if errors.Is(err, booking.ErrForbidden) {
			WriteError(w, http.StatusForbidden, "forbidden", "user is not a manager of this restaurant")
			return
		}
		if errors.Is(err, booking.ErrValidationFailed) {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "policy validation failed")
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	WriteJSON(w, http.StatusCreated, published)
}

func (h *RestaurantsHandler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "restaurants" || parts[2] != "policies" {
		WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	restaurantID := parts[1]

	policies, err := h.booking.ListPolicies(r.Context(), restaurantID)
	if err != nil {
		if errors.Is(err, booking.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", "restaurant not found")
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"policies": policies,
	})
}
