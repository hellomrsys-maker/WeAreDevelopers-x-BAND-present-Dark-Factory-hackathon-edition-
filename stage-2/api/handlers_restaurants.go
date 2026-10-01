package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

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

	result, err := h.booking.GetAvailability(r.Context(), restaurantID, dateStr, partySize)
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
