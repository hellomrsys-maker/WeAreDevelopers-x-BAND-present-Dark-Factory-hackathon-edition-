package guard

import (
	"context"
	"fmt"

	"tablekeeper/store"
)

type Engine struct {
	store *store.Store
}

func NewEngine(s *store.Store) *Engine {
	return &Engine{store: s}
}

// VerifyNoOverlaps queries for any two confirmed bookings on the same table that overlap in time
func (e *Engine) VerifyNoOverlaps(ctx context.Context) error {
	var count int
	err := e.store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM reservations a
		JOIN reservations b 
		  ON a.table_id = b.table_id
		 AND a.id < b.id
		 AND a.status = 'confirmed'
		 AND b.status = 'confirmed'
		 AND a.starts_at_utc < b.ends_at_utc
		 AND b.starts_at_utc < a.ends_at_utc`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("invariant violated: found %d overlapping confirmed reservations", count)
	}
	return nil
}

// VerifyCapacityLimits verifies all reservations satisfy their table capacity
func (e *Engine) VerifyCapacityLimits(ctx context.Context) error {
	var count int
	err := e.store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM reservations r
		JOIN tables t ON r.table_id = t.id AND r.restaurant_id = t.restaurant_id
		WHERE r.status = 'confirmed' AND r.party_size > t.capacity`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("invariant violated: found %d reservations exceeding table capacity", count)
	}
	return nil
}
