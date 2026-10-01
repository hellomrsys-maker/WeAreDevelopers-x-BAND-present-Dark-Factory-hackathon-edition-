package contracts

import (
	"context"
	"time"
)

// AuditEvent represents an immutable log of a state transition
type AuditEvent struct {
	ID        int64     `json:"id"`
	Entity    string    `json:"entity"`
	EntityID  string    `json:"entity_id"`
	Action    string    `json:"action"` // e.g. create, cancel, patch, move
	Payload   string    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditEngine defines append-only event logging
type AuditEngine interface {
	LogEvent(ctx context.Context, entity, entityID, action, payload string) error
	GetHistory(ctx context.Context, entity, entityID string) ([]AuditEvent, error)
}
