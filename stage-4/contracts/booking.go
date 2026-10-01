package contracts

import (
	"context"
	"time"
)

// BookingEngine governs reservations, collision avoidance, policies, history, series, and replanning
type BookingEngine interface {
	// CreateReservation attempts to atomically book a table or combinable pair for [startsAt, startsAt + duration)
	CreateReservation(ctx context.Context, req CreateReservationParams) (*Reservation, error)

	// GetReservation retrieves a reservation by its reference
	GetReservation(ctx context.Context, reference string) (*Reservation, error)

	// ListReservations returns reservations for a user, sorted startsAt descending
	ListReservations(ctx context.Context, userID string) ([]*Reservation, error)

	// CancelReservation cancels a reservation if cutoff has not passed
	CancelReservation(ctx context.Context, reference string, now time.Time) (*Reservation, error)

	// PatchReservation amends an existing reservation's tables, time, or party size
	PatchReservation(ctx context.Context, reference string, patch PatchReservationParams, now time.Time) (*Reservation, error)

	// MoveReservations performs an atomic all-or-nothing move of 1..8 reservations
	MoveReservations(ctx context.Context, userID string, moves []ReservationMoveRequest, now time.Time) ([]*Reservation, error)

	// GetAvailability computes available tables and combinable options for all slots on a date
	GetAvailability(ctx context.Context, restaurantID string, dateStr string, partySize int, explain bool) (*AvailabilityResult, error)

	// GetDecision returns the revision and accepted terms for a reservation (owner-only)
	GetDecision(ctx context.Context, reference string, userID string) (*DecisionResponse, error)

	// GetHistory returns the lifecycle event log for a reservation (owner-only)
	GetHistory(ctx context.Context, reference string, userID string) (*HistoryResponse, error)

	// CreateSeries adopts an existing confirmed reservation as occurrence 0 of a recurring series
	CreateSeries(ctx context.Context, userID string, req CreateSeriesRequest, now time.Time) (*SeriesResponse, error)

	// GetSeries returns the recurring series with current reservation states (owner-only)
	GetSeries(ctx context.Context, seriesID string, userID string) (*SeriesResponse, error)

	// AmendSeries changes the local clock time for occurrences >= from_index
	AmendSeries(ctx context.Context, seriesID string, userID string, req AmendSeriesRequest, now time.Time) (*SeriesResponse, error)

	// PublishPolicy creates an immutable dated policy version for a restaurant (manager-only)
	PublishPolicy(ctx context.Context, restaurantID string, userID string, p *Policy) (*Policy, error)

	// ListPolicies returns all published policies for a restaurant in publication order (public)
	ListPolicies(ctx context.Context, restaurantID string) ([]Policy, error)

	// CreateReplan computes a proposed seating plan for a table closure (manager-only)
	CreateReplan(ctx context.Context, restaurantID string, userID string, req ReplanRequest) (*ReplanResponse, error)

	// ApplyReplan atomically commits a replan, records the closure, and moves bookings (manager-only)
	ApplyReplan(ctx context.Context, restaurantID string, planID string, userID string, idempotencyKey string, now time.Time) (*ApplyPlanResponse, error)
}

type CreateReservationParams struct {
	RestaurantID  string
	TableIDs      []string
	TableID       string // optional if single table
	StartsAtLocal string
	PartySize     int
	UserID        string
	Now           time.Time
}

type PatchReservationParams struct {
	TableIDs         []string
	TableID          *string
	StartsAtLocal    *string
	PartySize        *int
	ExpectedRevision *int
}

type CreateSeriesRequest struct {
	AnchorReference string `json:"anchor_reference"`
	Count           int    `json:"count"`
	IntervalWeeks   int    `json:"interval_weeks"`
}

type AvailabilityResult struct {
	RestaurantID string             `json:"restaurant_id"`
	Date         string             `json:"date"`
	Timezone     string             `json:"timezone"`
	Slots        []AvailabilitySlot `json:"slots"`
}
