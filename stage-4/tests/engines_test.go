package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/engines/insights"
	"tablekeeper/engines/money"
	"tablekeeper/engines/payments"
	"tablekeeper/store"
)

func TestMoneyAndPaymentsEngine(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_money.db")

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	moneyEng := money.NewEngine(st)
	paymentsEng := payments.NewEngine(st, moneyEng)

	// Credit account
	accountID := "acc_test_1"
	bal, err := moneyEng.Credit(ctx, accountID, 10000, "EUR") // 100.00 EUR
	if err != nil {
		t.Fatalf("credit failed: %v", err)
	}
	if bal.Balance != 10000 {
		t.Fatalf("expected balance 10000, got %d", bal.Balance)
	}

	// Create payment intent
	intent, err := paymentsEng.CreatePaymentIntent(ctx, contracts.PaymentIntentRequest{
		AccountID: accountID,
		Amount:    3500, // 35.00 EUR
		Currency:  "EUR",
		Reference: "RES123",
	})
	if err != nil {
		t.Fatalf("create payment intent failed: %v", err)
	}
	if intent.Status != "requires_capture" {
		t.Fatalf("expected status requires_capture, got %s", intent.Status)
	}

	// Capture payment
	receipt, err := paymentsEng.CapturePayment(ctx, intent.IntentID)
	if err != nil {
		t.Fatalf("capture payment failed: %v", err)
	}
	if receipt.Amount != 3500 {
		t.Fatalf("expected receipt amount 3500, got %d", receipt.Amount)
	}

	// Verify balance after capture
	curBal, err := moneyEng.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance failed: %v", err)
	}
	if curBal.Balance != 6500 {
		t.Fatalf("expected balance 6500 after debit, got %d", curBal.Balance)
	}

	// Refund partial amount
	refund, err := paymentsEng.RefundPayment(ctx, intent.IntentID, 1500)
	if err != nil {
		t.Fatalf("refund failed: %v", err)
	}
	if refund.Amount != 1500 {
		t.Fatalf("expected refund amount 1500, got %d", refund.Amount)
	}

	// Verify balance after refund
	afterRefundBal, err := moneyEng.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance failed: %v", err)
	}
	if afterRefundBal.Balance != 8000 {
		t.Fatalf("expected balance 8000 after partial refund, got %d", afterRefundBal.Balance)
	}
}

func TestInsightsEngine(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_insights.db")

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insightsEng := insights.NewEngine(st)
	today := time.Now().Format("2006-01-02")

	util, err := insightsEng.ComputeUtilization(ctx, "r_test", today)
	if err != nil {
		t.Fatalf("utilization failed: %v", err)
	}
	if util.BookedSlots != 0 {
		t.Fatalf("expected 0 booked slots on empty db, got %d", util.BookedSlots)
	}

	peak, err := insightsEng.ComputePeakDemand(ctx, "r_test", today, today)
	if err != nil {
		t.Fatalf("peak demand failed: %v", err)
	}
	if peak.PeakBookings != 0 {
		t.Fatalf("expected 0 peak bookings on empty db, got %d", peak.PeakBookings)
	}
}
