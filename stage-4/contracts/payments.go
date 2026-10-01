package contracts

import (
	"context"
	"errors"
	"time"
)

var (
	ErrPaymentFailed   = errors.New("payment_failed")
	ErrIntentNotFound  = errors.New("intent_not_found")
	ErrAlreadyCaptured = errors.New("already_captured")
)

type PaymentIntentRequest struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Reference string `json:"reference"`
}

type PaymentIntent struct {
	IntentID  string    `json:"intent_id"`
	AccountID string    `json:"account_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Status    string    `json:"status"` // requires_capture, captured, refunded
	CreatedAt time.Time `json:"created_at"`
}

type PaymentReceipt struct {
	ReceiptID string    `json:"receipt_id"`
	IntentID  string    `json:"intent_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	CapturedAt time.Time `json:"captured_at"`
}

type RefundReceipt struct {
	RefundID   string    `json:"refund_id"`
	IntentID   string    `json:"intent_id"`
	Amount     int64     `json:"amount"`
	RefundedAt time.Time `json:"refunded_at"`
}

type PaymentsEngine interface {
	CreatePaymentIntent(ctx context.Context, req PaymentIntentRequest) (*PaymentIntent, error)
	CapturePayment(ctx context.Context, intentID string) (*PaymentReceipt, error)
	RefundPayment(ctx context.Context, intentID string, amount int64) (*RefundReceipt, error)
}
