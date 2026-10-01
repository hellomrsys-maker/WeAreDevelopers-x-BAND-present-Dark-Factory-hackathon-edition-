package calendar

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"tablekeeper/contracts"
)

var (
	ErrInvalidLocalTime   = errors.New("invalid_local_time")
	ErrValidationFailed   = errors.New("validation_failed")
	localTimeFormatRegex  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$`)
	dateOnlyFormatRegex   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type Engine struct{}

func NewEngine() *Engine {
	return &Engine{}
}

var weekdayMap = map[time.Weekday]string{
	time.Monday:    "mon",
	time.Tuesday:   "tue",
	time.Wednesday: "wed",
	time.Thursday:  "thu",
	time.Friday:    "fri",
	time.Saturday:  "sat",
	time.Sunday:    "sun",
}

func (e *Engine) ParseLocalTime(timezone, localStr string) (time.Time, error) {
	if !localTimeFormatRegex.MatchString(localStr) {
		return time.Time{}, ErrValidationFailed
	}

	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %s: %w", timezone, err)
	}

	t, err := time.ParseInLocation("2006-01-02T15:04", localStr, loc)
	if err != nil {
		return time.Time{}, ErrValidationFailed
	}

	// Spring forward detection: Go normalizes non-existent wall-clock times to the shifted hour.
	// If the formatted time in that location does not match what was requested, it was a skipped hour.
	if t.In(loc).Format("2006-01-02T15:04") != localStr {
		return time.Time{}, ErrInvalidLocalTime
	}

	// Fall back detection: Local times in the repeated hour occur twice.
	// The spec requires resolving to the first occurrence (before clocks change).
	// If an earlier UTC instant yields the exact same local time, choose that first occurrence.
	for _, delta := range []time.Duration{time.Hour, 30 * time.Minute} {
		earlier := t.Add(-delta)
		if earlier.In(loc).Format("2006-01-02T15:04") == localStr {
			t = earlier
			break
		}
	}

	return t, nil
}

func (e *Engine) FormatLocalTime(timezone string, t time.Time) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return t.Format("2006-01-02T15:04")
	}
	return t.In(loc).Format("2006-01-02T15:04")
}

func (e *Engine) FormatRFC3339(t time.Time) string {
	return t.Format(time.RFC3339)
}

func (e *Engine) GenerateSlots(restaurant *contracts.Restaurant, dateStr string) ([]contracts.SlotWindow, error) {
	if !dateOnlyFormatRegex.MatchString(dateStr) {
		return nil, ErrValidationFailed
	}

	loc, err := time.LoadLocation(restaurant.Timezone)
	if err != nil {
		return nil, err
	}

	date, err := time.ParseInLocation("2006-01-02", dateStr, loc)
	if err != nil {
		return nil, ErrValidationFailed
	}

	dayWeekday := weekdayMap[date.Weekday()]

	// Find opening hours for this day
	var hours *contracts.OpeningHour
	for i := range restaurant.OpeningHours {
		if strings.EqualFold(restaurant.OpeningHours[i].Weekday, dayWeekday) {
			hours = &restaurant.OpeningHours[i]
			break
		}
	}

	// Closed day returns empty list
	if hours == nil {
		return []contracts.SlotWindow{}, nil
	}

	openH, openM, err := parseHHMM(hours.Opens)
	if err != nil {
		return nil, err
	}
	closeH, closeM, err := parseHHMM(hours.Closes)
	if err != nil {
		return nil, err
	}

	openTime := time.Date(date.Year(), date.Month(), date.Day(), openH, openM, 0, 0, loc)
	closeTime := time.Date(date.Year(), date.Month(), date.Day(), closeH, closeM, 0, 0, loc)

	var slots []contracts.SlotWindow
	slotDuration := time.Duration(restaurant.ReservationDurationMinutes) * time.Minute
	step := time.Duration(restaurant.SlotMinutes) * time.Minute

	if step <= 0 {
		step = 30 * time.Minute
	}

	curr := openTime
	for {
		// Absolute duration: curr + slotDuration <= closeTime
		slotEnd := curr.Add(slotDuration)
		if slotEnd.After(closeTime) {
			break
		}

		// Verify this slot's local time actually exists (not skipped by spring forward)
		localStr := curr.Format("2006-01-02T15:04")
		parsed, err := e.ParseLocalTime(restaurant.Timezone, localStr)
		if err == nil && parsed.Equal(curr) {
			slots = append(slots, contracts.SlotWindow{
				StartsAtLocal: localStr,
				StartsAt:      curr,
				EndsAt:        slotEnd,
			})
		}

		curr = curr.Add(step)
	}

	return slots, nil
}

func (e *Engine) IsWithinOpeningHours(restaurant *contracts.Restaurant, start time.Time) bool {
	loc, err := time.LoadLocation(restaurant.Timezone)
	if err != nil {
		return false
	}
	localStart := start.In(loc)
	dayWeekday := weekdayMap[localStart.Weekday()]

	var hours *contracts.OpeningHour
	for i := range restaurant.OpeningHours {
		if strings.EqualFold(restaurant.OpeningHours[i].Weekday, dayWeekday) {
			hours = &restaurant.OpeningHours[i]
			break
		}
	}
	if hours == nil {
		return false
	}

	openH, openM, err := parseHHMM(hours.Opens)
	if err != nil {
		return false
	}
	closeH, closeM, err := parseHHMM(hours.Closes)
	if err != nil {
		return false
	}

	openTime := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), openH, openM, 0, 0, loc)
	closeTime := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), closeH, closeM, 0, 0, loc)

	if localStart.Before(openTime) {
		return false
	}

	end := start.Add(time.Duration(restaurant.ReservationDurationMinutes) * time.Minute)
	return !end.After(closeTime)
}

func (e *Engine) IsOnSlotGrid(restaurant *contracts.Restaurant, start time.Time) bool {
	dateStr := e.FormatLocalTime(restaurant.Timezone, start)[:10]
	slots, err := e.GenerateSlots(restaurant, dateStr)
	if err != nil {
		return false
	}
	for _, s := range slots {
		if s.StartsAt.Equal(start) {
			return true
		}
	}
	return false
}

func (e *Engine) IsCutoffPassed(startsAt time.Time, cutoffMinutes int, now time.Time) bool {
	cutoffTime := startsAt.Add(-time.Duration(cutoffMinutes) * time.Minute)
	return !now.Before(cutoffTime) // now >= cutoffTime
}

func parseHHMM(s string) (int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid HH:MM: %s", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return h, m, nil
}
