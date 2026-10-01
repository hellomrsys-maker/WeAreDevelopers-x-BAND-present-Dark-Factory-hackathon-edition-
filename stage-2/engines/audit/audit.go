package audit

import (
	"context"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/store"
)

type Engine struct {
	store *store.Store
}

func NewEngine(s *store.Store) *Engine {
	return &Engine{store: s}
}

func (e *Engine) LogEvent(ctx context.Context, entity, entityID, action, payload string) error {
	_, err := e.store.DB().ExecContext(ctx, `
		INSERT INTO audit_log(entity, entity_id, action, payload, created_at)
		VALUES(?, ?, ?, ?, ?)`,
		entity, entityID, action, payload, time.Now().Unix())
	return err
}

func (e *Engine) GetHistory(ctx context.Context, entity, entityID string) ([]contracts.AuditEvent, error) {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT id, entity, entity_id, action, payload, created_at
		FROM audit_log WHERE entity = ? AND entity_id = ? ORDER BY id ASC`, entity, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []contracts.AuditEvent
	for rows.Next() {
		var ev contracts.AuditEvent
		var createdU int64
		if err := rows.Scan(&ev.ID, &ev.Entity, &ev.EntityID, &ev.Action, &ev.Payload, &createdU); err != nil {
			return nil, err
		}
		ev.CreatedAt = time.Unix(createdU, 0)
		events = append(events, ev)
	}
	return events, nil
}
