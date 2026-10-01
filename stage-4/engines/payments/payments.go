package payments

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"sync"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/store"
)

type Engine struct {
	store *store.Store
	money contracts.MoneyEngine
	mu    sync.Mutex
}

func NewEngine(s *store.Store, m contracts.MoneyEngine) *Engine {
	return &Engine{store: s, money: m}
}

func (e *Engine) CreatePaymentIntent(ctx context.Context, req contracts.PaymentIntentRequest) (*contracts.PaymentIntent, error) {
	if req.Amount <= 0 {
		return nil, contracts.ErrInvalidAmount
	}
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	intentID := "pi_" + hex.EncodeToString(bytes)

	now := time.Now().UTC()
	_, err := e.store.DB().ExecContext(ctx, `
		INSERT INTO payment_intents(intent_id, account_id, amount, currency, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		intentID, req.AccountID, req.Amount, req.Currency, "requires_capture", now.Unix())
	if err != nil {
		return nil, err
	}

	return &contracts.PaymentIntent{
		IntentID:  intentID,
		AccountID: req.AccountID,
		Amount:    req.Amount,
		Currency:  req.Currency,
		Status:    "requires_capture",
		CreatedAt: now,
	}, nil
}

func (e *Engine) CapturePayment(ctx context.Context, intentID string) (*contracts.PaymentReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var accountID, currency, status string
	var amount int64
	err := e.store.DB().QueryRowContext(ctx, "SELECT account_id, amount, currency, status FROM payment_intents WHERE intent_id = ?", intentID).Scan(&accountID, &amount, &currency, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, contracts.ErrIntentNotFound
		}
		return nil, err
	}

	if status == "captured" {
		return nil, contracts.ErrAlreadyCaptured
	}

	// Debit the account
	_, err = e.money.Debit(ctx, accountID, amount, currency)
	if err != nil {
		return nil, err
	}

	_, err = e.store.DB().ExecContext(ctx, "UPDATE payment_intents SET status = 'captured' WHERE intent_id = ?", intentID)
	if err != nil {
		return nil, err
	}

	rBytes := make([]byte, 8)
	_, _ = rand.Read(rBytes)
	receiptID := "rcpt_" + hex.EncodeToString(rBytes)

	now := time.Now().UTC()
	return &contracts.PaymentReceipt{
		ReceiptID:  receiptID,
		IntentID:   intentID,
		Amount:     amount,
		Currency:   currency,
		CapturedAt: now,
	}, nil
}

func (e *Engine) RefundPayment(ctx context.Context, intentID string, amount int64) (*contracts.RefundReceipt, error) {
	if amount <= 0 {
		return nil, contracts.ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	var accountID, currency, status string
	var origAmount int64
	err := e.store.DB().QueryRowContext(ctx, "SELECT account_id, amount, currency, status FROM payment_intents WHERE intent_id = ?", intentID).Scan(&accountID, &origAmount, &currency, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, contracts.ErrIntentNotFound
		}
		return nil, err
	}

	if amount > origAmount {
		return nil, contracts.ErrInvalidAmount
	}

	// Credit back the account
	_, err = e.money.Credit(ctx, accountID, amount, currency)
	if err != nil {
		return nil, err
	}

	_, err = e.store.DB().ExecContext(ctx, "UPDATE payment_intents SET status = 'refunded' WHERE intent_id = ?", intentID)
	if err != nil {
		return nil, err
	}

	rBytes := make([]byte, 8)
	_, _ = rand.Read(rBytes)
	refundID := "ref_" + hex.EncodeToString(rBytes)

	now := time.Now().UTC()
	return &contracts.RefundReceipt{
		RefundID:   refundID,
		IntentID:   intentID,
		Amount:     amount,
		RefundedAt: now,
	}, nil
}
