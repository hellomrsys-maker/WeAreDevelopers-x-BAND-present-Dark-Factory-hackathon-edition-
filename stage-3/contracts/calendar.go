package contracts

import "time"

// CalendarEngine handles timezone conversions, opening hour schedules, slot grids, and cutoff rules
type CalendarEngine interface {
	// ParseLocalTime parses a bare local YYYY-MM-DDTHH:MM in the restaurant's timezone,
	// returning an error if it falls in a DST spring-forward gap (invalid_local_time),
	// and resolving to the earlier occurrence in a DST fall-back repeated hour.
	ParseLocalTime(timezone, localStr string) (time.Time, error)

	// FormatLocalTime formats a time instant into a bare YYYY-MM-DDTHH:MM in the restaurant's timezone.
	FormatLocalTime(timezone string, t time.Time) string

	// FormatRFC3339 formats a time instant as RFC3339 with explicit offset (e.g. +02:00 or -04:00)
	FormatRFC3339(t time.Time) string

	// GenerateSlots calculates all valid booking slots for a restaurant on a given calendar date (YYYY-MM-DD)
	GenerateSlots(restaurant *Restaurant, dateStr string) ([]SlotWindow, error)

	// GenerateSlotsForPolicy calculates all valid booking slots under a specific dated policy
	GenerateSlotsForPolicy(timezone string, slotMinutes, durationMinutes int, hours []OpeningHour, dateStr string) ([]SlotWindow, error)

	// IsWithinOpeningHours checks if the interval [start, start+duration] falls within opening hours on that weekday
	IsWithinOpeningHours(restaurant *Restaurant, start time.Time) bool

	// IsWithinOpeningHoursForPolicy checks if the interval [start, start+duration] falls within policy opening hours
	IsWithinOpeningHoursForPolicy(hours []OpeningHour, durationMinutes int, start time.Time) bool

	// IsOnSlotGrid verifies that start matches a calculated slot for that day
	IsOnSlotGrid(restaurant *Restaurant, start time.Time) bool

	// IsOnSlotGridForPolicy verifies that start matches a calculated slot under policy rules
	IsOnSlotGridForPolicy(timezone string, slotMinutes, durationMinutes int, hours []OpeningHour, start time.Time) bool

	// IsCutoffPassed checks if now >= startsAt - cutoffMinutes
	IsCutoffPassed(startsAt time.Time, cutoffMinutes int, now time.Time) bool
}

// SlotWindow describes an interval for a potential booking
type SlotWindow struct {
	StartsAtLocal string
	StartsAt      time.Time
	EndsAt        time.Time
}
