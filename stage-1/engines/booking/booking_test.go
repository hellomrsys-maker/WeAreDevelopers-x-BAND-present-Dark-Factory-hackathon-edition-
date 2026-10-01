package booking

import (
	"context"
	"os"
	"testing"
	"time"

	"tablekeeper/contracts"
	"tablekeeper/engines/audit"
	"tablekeeper/engines/calendar"
	"tablekeeper/store"
)

func setupTestStore(t *testing.T) (*store.Store, *Engine, *contracts.Restaurant) {
	dbFile := "test_booking.db"
	_ = os.Remove(dbFile)

	st, err := store.NewStore("file:" + dbFile + "?cache=shared")
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		_ = os.Remove(dbFile)
	})

	calEng := calendar.NewEngine()
	auditEng := audit.NewEngine(st)
	bookEng := NewEngine(st, calEng, auditEng)

	rest := contracts.Restaurant{
		ID:                         "r_anker",
		Name:                       "Zum Anker",
		Timezone:                   "Europe/Berlin",
		SlotMinutes:                30,
		ReservationDurationMinutes: 90,
		CancellationCutoffMinutes:  120,
		OpeningHours: []contracts.OpeningHour{
			{Weekday: "thu", Opens: "18:00", Closes: "23:00"},
		},
		Tables: []contracts.Table{
			{ID: "t_1", Label: "1", Capacity: 2},
			{ID: "t_2", Label: "2", Capacity: 4},
			{ID: "t_3", Label: "3", Capacity: 6},
		},
	}

	user := contracts.User{
		ID:          "u_ada",
		Email:       "ada@example.com",
		Password:    "correct horse",
		DisplayName: "Ada",
	}

	err = st.Reset(context.Background(), &contracts.FixtureData{
		Users:       []contracts.User{user},
		Restaurants: []contracts.Restaurant{rest},
	})
	if err != nil {
		t.Fatalf("failed to reset fixture: %v", err)
	}

	return st, bookEng, &rest
}

func TestBookingCreationAndCollision(t *testing.T) {
	ctx := context.Background()
	_, bookEng, _ := setupTestStore(t)

	// 2026-09-24 is Thursday
	// First booking on t_2 at 19:00 -> duration 90m (19:00 - 20:30)
	res1, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_2",
		StartsAtLocal: "2026-09-24T19:00",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("failed to create first booking: %v", err)
	}
	if res1.Reference == "" {
		t.Fatal("expected reference to be set")
	}

	// Overlapping booking on same table t_2 at 20:00 (overlaps with 19:00-20:30) -> must be table_unavailable
	_, err = bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_2",
		StartsAtLocal: "2026-09-24T20:00",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != ErrTableUnavailable {
		t.Fatalf("expected ErrTableUnavailable, got %v", err)
	}

	// Adjacent non-overlapping booking at 20:30 on t_2 -> must SUCCEED!
	res3, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_2",
		StartsAtLocal: "2026-09-24T20:30",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("expected adjacent booking at 20:30 to succeed, got %v", err)
	}
	if res3.Status != "confirmed" {
		t.Errorf("expected confirmed status, got %s", res3.Status)
	}

	// Different table t_3 at the same time 19:00 -> must SUCCEED!
	res4, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_3",
		StartsAtLocal: "2026-09-24T19:00",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("expected booking on t_3 to succeed, got %v", err)
	}
	if res4.TableID != "t_3" {
		t.Errorf("expected t_3, got %s", res4.TableID)
	}
}

func TestCapacityValidation(t *testing.T) {
	ctx := context.Background()
	_, bookEng, _ := setupTestStore(t)

	// t_1 capacity is 2. Booking party_size 4 must be party_exceeds_capacity
	_, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_1",
		StartsAtLocal: "2026-09-24T19:00",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != ErrPartyExceedsCapacity {
		t.Fatalf("expected ErrPartyExceedsCapacity, got %v", err)
	}
}

func TestAtomicMoves(t *testing.T) {
	ctx := context.Background()
	_, bookEng, _ := setupTestStore(t)

	// Book BOOK01 on t_1 (19:00) and BOOK02 on t_2 (19:00)
	b1, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_1",
		StartsAtLocal: "2026-09-24T19:00",
		PartySize:     2,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("booking 1 failed: %v", err)
	}

	b2, err := bookEng.CreateReservation(ctx, contracts.CreateReservationParams{
		RestaurantID:  "r_anker",
		TableID:       "t_2",
		StartsAtLocal: "2026-09-24T19:00",
		PartySize:     4,
		UserID:        "u_ada",
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("booking 2 failed: %v", err)
	}

	// Use simulated time before cutoff (e.g. 2026-09-20)
	simNow := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	// Move b1 to t_3 (valid)
	t3 := "t_3"
	results, err := bookEng.MoveReservations(ctx, "u_ada", []contracts.ReservationMoveRequest{
		{Reference: b1.Reference, TableID: &t3},
	}, simNow)
	if err != nil {
		t.Fatalf("move failed: %v", err)
	}
	if len(results) != 1 || results[0].TableID != "t_3" {
		t.Fatalf("expected moved table t_3, got %v", results)
	}

	// Move swap test: try to move b2 onto occupied table where b1 was -> t_1 is now free, t_3 is occupied
	// Attempt moving b2 to t_3 should fail with table_unavailable
	_, err = bookEng.MoveReservations(ctx, "u_ada", []contracts.ReservationMoveRequest{
		{Reference: b2.Reference, TableID: &t3},
	}, simNow)
	if err != ErrTableUnavailable {
		t.Fatalf("expected ErrTableUnavailable, got %v", err)
	}
}
