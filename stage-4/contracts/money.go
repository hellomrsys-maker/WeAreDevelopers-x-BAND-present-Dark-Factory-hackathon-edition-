package contracts

import (
	"context"
	"errors"
)

var (
	ErrInsufficientFunds = errors.New("insufficient_funds")
	ErrInvalidAmount     = errors.New("invalid_amount")
	ErrAccountNotFound   = errors.New("account_not_found")
)

type AccountBalance struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Balance   int64  `json:"balance"` // Minor units (cents)
}

type MoneyEngine interface {
	Credit(ctx context.Context, accountID string, amount int64, currency string) (*AccountBalance, error)
	Debit(ctx context.Context, accountID string, amount int64, currency string) (*AccountBalance, error)
	GetBalance(ctx context.Context, accountID string) (*AccountBalance, error)
}
