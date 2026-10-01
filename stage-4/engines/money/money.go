package money

import (
	"context"
	"database/sql"
	"sync"
	"tablekeeper/contracts"
	"tablekeeper/store"
)

type Engine struct {
	store *store.Store
	mu    sync.Mutex
}

func NewEngine(s *store.Store) *Engine {
	return &Engine{store: s}
}

func (e *Engine) Credit(ctx context.Context, accountID string, amount int64, currency string) (*contracts.AccountBalance, error) {
	if amount <= 0 {
		return nil, contracts.ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var bal int64
	var cur string
	err = tx.QueryRowContext(ctx, "SELECT balance, currency FROM accounts WHERE id = ?", accountID).Scan(&bal, &cur)
	if err != nil {
		if err == sql.ErrNoRows {
			cur = currency
			if cur == "" {
				cur = "EUR"
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO accounts(id, currency, balance) VALUES(?, ?, ?)", accountID, cur, amount)
			if err != nil {
				return nil, err
			}
			bal = amount
		} else {
			return nil, err
		}
	} else {
		bal += amount
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET balance = ? WHERE id = ?", bal, accountID)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &contracts.AccountBalance{
		AccountID: accountID,
		Currency:  cur,
		Balance:   bal,
	}, nil
}

func (e *Engine) Debit(ctx context.Context, accountID string, amount int64, currency string) (*contracts.AccountBalance, error) {
	if amount <= 0 {
		return nil, contracts.ErrInvalidAmount
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var bal int64
	var cur string
	err = tx.QueryRowContext(ctx, "SELECT balance, currency FROM accounts WHERE id = ?", accountID).Scan(&bal, &cur)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, contracts.ErrAccountNotFound
		}
		return nil, err
	}

	if bal < amount {
		return nil, contracts.ErrInsufficientFunds
	}

	bal -= amount
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET balance = ? WHERE id = ?", bal, accountID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &contracts.AccountBalance{
		AccountID: accountID,
		Currency:  cur,
		Balance:   bal,
	}, nil
}

func (e *Engine) GetBalance(ctx context.Context, accountID string) (*contracts.AccountBalance, error) {
	var bal int64
	var cur string
	err := e.store.DB().QueryRowContext(ctx, "SELECT balance, currency FROM accounts WHERE id = ?", accountID).Scan(&bal, &cur)
	if err != nil {
		if err == sql.ErrNoRows {
			return &contracts.AccountBalance{
				AccountID: accountID,
				Currency:  "EUR",
				Balance:   0,
			}, nil
		}
		return nil, err
	}
	return &contracts.AccountBalance{
		AccountID: accountID,
		Currency:  cur,
		Balance:   bal,
	}, nil
}
