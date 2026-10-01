package contracts

import (
	"context"
	"time"
)

// BookingEngine governs reservations, collision avoidance, and atomic multi-reservation moves
type BookingEngine interface {
	// CreateReservation attempts to atomically book a table for [startsAt, startsAt + duration)
	CreateReservation(ctx context.Context, req CreateReservationParams) (*Reservation, error)

	// GetReservation retrieves a reservation by its reference
	GetReservation(ctx context.Context, reference string) (*Reservation, error)

	// ListReservations returns reservations for a user, sorted startsAt descending
	ListReservations(ctx context.Context, userID string) ([]*Reservation, error)

	// CancelReservation cancels a reservation if cutoff has not passed
	CancelReservation(ctx context.Context, reference string, now time.Time) (*Reservation, error)

	// PatchReservation amends an existing reservation's table, time, or party size
	PatchReservation(ctx context.Context, reference string, patch PatchReservationParams, now time.Time) (*Reservation, error)

	// MoveReservations performs an atomic all-or-nothing move of 1..8 reservations
	MoveReservations(ctx context.Context, userID string, moves []ReservationMoveRequest, now time.Time) ([]*Reservation, error)

	// GetAvailability computes available tables for all slots on a date
	GetAvailability(ctx context.Context, restaurantID string, dateStr string, partySize int) (*AvailabilityResult, error)
}

type CreateReservationParams struct {
	RestaurantID  string
	TableID       string
	StartsAtLocal string
	PartySize     int
	UserID        string
	Now           time.Time
}

type PatchReservationParams struct {
	TableID       *string
	StartsAtLocal *string
	PartySize     *int
}

type AvailabilityResult struct {
	RestaurantID string             `json:"restaurant_id"`
	Date         string             `json:"date"`
	Timezone     string             `json:"timezone"`
	Slots        []AvailabilitySlot `json:"slots"`
}
