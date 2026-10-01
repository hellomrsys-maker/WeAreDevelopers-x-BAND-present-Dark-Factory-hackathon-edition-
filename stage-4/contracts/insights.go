package contracts

import (
	"context"
)

type UtilizationReport struct {
	RestaurantID      string  `json:"restaurant_id"`
	Date              string  `json:"date"`
	TotalSlots        int     `json:"total_slots"`
	BookedSlots       int     `json:"booked_slots"`
	UtilizationRate   float64 `json:"utilization_rate"`
	TotalDinersServed int     `json:"total_diners_served"`
}

type PeakDemandReport struct {
	RestaurantID string         `json:"restaurant_id"`
	FromDate     string         `json:"from_date"`
	ToDate       string         `json:"to_date"`
	PeakSlot     string         `json:"peak_slot"`
	PeakBookings int            `json:"peak_bookings"`
	DayCounts    map[string]int `json:"day_counts"`
}

type InsightsEngine interface {
	ComputeUtilization(ctx context.Context, restaurantID string, date string) (*UtilizationReport, error)
	ComputePeakDemand(ctx context.Context, restaurantID string, fromDate, toDate string) (*PeakDemandReport, error)
}
