package contracts

import (
	"context"
)

// GuardEngine runs invariant verification assertions
type GuardEngine interface {
	// VerifyNoOverlaps checks that across all confirmed bookings in the database,
	// no two bookings share a table during overlapping intervals [start, end)
	VerifyNoOverlaps(ctx context.Context) error

	// VerifyCapacityLimits verifies all reservations satisfy table capacity constraints
	VerifyCapacityLimits(ctx context.Context) error
}
