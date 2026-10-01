package calendar

import (
	"testing"
	"time"

	"tablekeeper/contracts"
)

func TestSpringForwardSkippedHour(t *testing.T) {
	eng := NewEngine()
	// Berlin springs forward 2026-03-29 02:00 -> 03:00.
	// 02:30 does not exist.
	_, err := eng.ParseLocalTime("Europe/Berlin", "2026-03-29T02:30")
	if err != ErrInvalidLocalTime {
		t.Fatalf("expected ErrInvalidLocalTime, got %v", err)
	}

	// 01:30 exists
	valid, err := eng.ParseLocalTime("Europe/Berlin", "2026-03-29T01:30")
	if err != nil {
		t.Fatalf("expected 01:30 to be valid, got %v", err)
	}
	if valid.IsZero() {
		t.Fatal("expected non-zero time")
	}
}

func TestFallBackRepeatedHour(t *testing.T) {
	eng := NewEngine()
	// Berlin falls back 2026-10-25 03:00 -> 02:00.
	// 02:30 occurs twice. Must resolve to first occurrence (+02:00).
	first, err := eng.ParseLocalTime("Europe/Berlin", "2026-10-25T02:30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, offset := first.Zone()
	// Berlin summer time offset is 7200 seconds (+02:00)
	if offset != 7200 {
		t.Fatalf("expected fall back to resolve to first occurrence (+02:00 / 7200s), got offset %d", offset)
	}
}

func TestSlotGeneration(t *testing.T) {
	eng := NewEngine()
	rest := &contracts.Restaurant{
		ID:                         "r_anker",
		Timezone:                   "Europe/Berlin",
		SlotMinutes:                30,
		ReservationDurationMinutes: 90,
		OpeningHours: []contracts.OpeningHour{
			{Weekday: "thu", Opens: "18:00", Closes: "23:00"},
		},
	}

	// 2026-09-24 is a Thursday
	slots, err := eng.GenerateSlots(rest, "2026-09-24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 18:00 to 23:00 with 90 min duration means last slot starts at 21:30 (21:30 + 90m = 23:00)
	// 18:00, 18:30, 19:00, 19:30, 20:00, 20:30, 21:00, 21:30 -> 8 slots
	if len(slots) != 8 {
		t.Fatalf("expected 8 slots, got %d", len(slots))
	}
	if slots[0].StartsAtLocal != "2026-09-24T18:00" {
		t.Errorf("expected first slot 18:00, got %s", slots[0].StartsAtLocal)
	}
	if slots[len(slots)-1].StartsAtLocal != "2026-09-24T21:30" {
		t.Errorf("expected last slot 21:30, got %s", slots[len(slots)-1].StartsAtLocal)
	}
}

func TestCutoffPassed(t *testing.T) {
	eng := NewEngine()
	start := time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)
	cutoffMins := 120 // 2 hours before start -> 17:00

	// 16:59 is before cutoff -> cutoff NOT passed
	if eng.IsCutoffPassed(start, cutoffMins, time.Date(2026, 9, 24, 16, 59, 0, 0, time.UTC)) {
		t.Errorf("16:59 should not be cutoff passed")
	}

	// 17:00 is at cutoff -> cutoff passed
	if !eng.IsCutoffPassed(start, cutoffMins, time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("17:00 should be cutoff passed")
	}

	// Past start time is cutoff passed
	if !eng.IsCutoffPassed(start, cutoffMins, time.Date(2026, 9, 24, 19, 30, 0, 0, time.UTC)) {
		t.Errorf("19:30 should be cutoff passed")
	}
}
