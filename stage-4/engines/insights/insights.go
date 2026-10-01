package insights

import (
	"context"

	"tablekeeper/contracts"
	"tablekeeper/store"
)

type Engine struct {
	store *store.Store
}

func NewEngine(s *store.Store) *Engine {
	return &Engine{store: s}
}

func (e *Engine) ComputeUtilization(ctx context.Context, restaurantID string, date string) (*contracts.UtilizationReport, error) {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT party_size FROM reservations
		WHERE restaurant_id = ? AND status = 'confirmed' AND starts_at_local LIKE ?`,
		restaurantID, date+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	booked := 0
	totalDiners := 0
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err == nil {
			booked++
			totalDiners += p
		}
	}

	totalSlots := 10 // baseline nominal service capacity
	rate := 0.0
	if totalSlots > 0 {
		rate = float64(booked) / float64(totalSlots)
	}

	return &contracts.UtilizationReport{
		RestaurantID:      restaurantID,
		Date:              date,
		TotalSlots:        totalSlots,
		BookedSlots:       booked,
		UtilizationRate:   rate,
		TotalDinersServed: totalDiners,
	}, nil
}

func (e *Engine) ComputePeakDemand(ctx context.Context, restaurantID string, fromDate, toDate string) (*contracts.PeakDemandReport, error) {
	dayCounts := make(map[string]int)
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT starts_at_local FROM reservations
		WHERE restaurant_id = ? AND status = 'confirmed'
		  AND starts_at_local >= ? AND starts_at_local <= ?`,
		restaurantID, fromDate, toDate+"T23:59:59")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	slotCounts := make(map[string]int)
	for rows.Next() {
		var startsLocal string
		if err := rows.Scan(&startsLocal); err == nil {
			if len(startsLocal) >= 10 {
				day := startsLocal[:10]
				dayCounts[day]++
			}
			if len(startsLocal) >= 16 {
				slot := startsLocal[11:16]
				slotCounts[slot]++
			}
		}
	}

	peakSlot := ""
	peakCount := 0
	for slot, cnt := range slotCounts {
		if cnt > peakCount {
			peakCount = cnt
			peakSlot = slot
		}
	}

	return &contracts.PeakDemandReport{
		RestaurantID: restaurantID,
		FromDate:     fromDate,
		ToDate:       toDate,
		PeakSlot:     peakSlot,
		PeakBookings: peakCount,
		DayCounts:    dayCounts,
	}, nil
}
