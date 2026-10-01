package contracts

import (
	"encoding/json"
	"time"
)

// User represents an authenticated account
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Password    string `json:"password,omitempty"` // Hashed, omitted in API responses
	DisplayName string `json:"display_name"`
}

// OpeningHour defines operating hours for a weekday
type OpeningHour struct {
	Weekday string `json:"weekday"` // mon, tue, wed, thu, fri, sat, sun
	Opens   string `json:"opens"`   // HH:MM 24-hour
	Closes  string `json:"closes"`  // HH:MM 24-hour
}

// Table represents a dining table in a restaurant
type Table struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Capacity int    `json:"capacity"`
}

// Restaurant defines the properties and policies of a venue
type Restaurant struct {
	ID                         string        `json:"id"`
	Name                       string        `json:"name"`
	Timezone                   string        `json:"timezone"`
	SlotMinutes                int           `json:"slot_minutes"`
	ReservationDurationMinutes int           `json:"reservation_duration_minutes"`
	CancellationCutoffMinutes  int           `json:"cancellation_cutoff_minutes"`
	OpeningHours               []OpeningHour `json:"opening_hours"`
	Tables                     []Table       `json:"tables"`
	Combinable                 [][]string    `json:"combinable,omitempty"`
	ManagerUserIDs             []string      `json:"manager_user_ids,omitempty"`
	Revision                   int           `json:"revision,omitempty"`
}

// Policy defines a dated booking policy for a restaurant
type Policy struct {
	PolicyVersion              int            `json:"policy_version"`
	EffectiveFrom              string         `json:"effective_from"`
	SlotMinutes                int            `json:"slot_minutes"`
	ReservationDurationMinutes int            `json:"reservation_duration_minutes"`
	CancellationCutoffMinutes  int            `json:"cancellation_cutoff_minutes"`
	OpeningHours               []OpeningHour  `json:"opening_hours"`
	Capacities                 map[string]int `json:"capacities"`
}

// AcceptedTerms is a snapshot of the selected policy, excluding effective_from
type AcceptedTerms struct {
	PolicyVersion              int            `json:"policy_version"`
	SlotMinutes                int            `json:"slot_minutes"`
	ReservationDurationMinutes int            `json:"reservation_duration_minutes"`
	CancellationCutoffMinutes  int            `json:"cancellation_cutoff_minutes"`
	OpeningHours               []OpeningHour  `json:"opening_hours"`
	Capacities                 map[string]int `json:"capacities"`
}

// ExplainRule describes the evaluation of a single availability rule
type ExplainRule struct {
	Rule  string `json:"rule"`
	Holds bool   `json:"holds"`
}

// TableExplain describes why a table is available or unavailable
type TableExplain struct {
	TableID       string        `json:"table_id"`
	PolicyVersion int           `json:"policy_version"`
	Available     bool          `json:"available"`
	Rules         []ExplainRule `json:"rules"`
}

// Reservation represents a booked or cancelled table assignment
type Reservation struct {
	ID            string         `json:"reservation_id"`
	Reference     string         `json:"reference"`
	RestaurantID  string         `json:"restaurant_id"`
	TableIDs      []string       `json:"table_ids"`
	TableID       *string        `json:"table_id,omitempty"`
	UserID        string         `json:"user_id,omitempty"`
	PartySize     int            `json:"party_size"`
	Status        string         `json:"status"` // confirmed or cancelled
	StartsAtLocal string         `json:"starts_at_local"`
	Revision      int            `json:"revision"`
	AcceptedTerms *AcceptedTerms `json:"accepted_terms"`
	StartsAtUTC   int64          `json:"starts_at_utc,omitempty"`
	EndsAtUTC     int64          `json:"ends_at_utc,omitempty"`
	CreatedAtUTC  int64          `json:"created_at_utc,omitempty"`
	StartsAt      time.Time      `json:"-"`
	EndsAt        time.Time      `json:"-"`
	CreatedAt     time.Time      `json:"-"`
}

// Custom UnmarshalJSON to handle table_id, table_ids, default revision and confirmed status
func (r *Reservation) UnmarshalJSON(data []byte) error {
	type Alias Reservation
	aux := struct {
		RawTableID  *string   `json:"table_id"`
		RawTableIDs []string  `json:"table_ids"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if len(aux.RawTableIDs) > 0 {
		r.TableIDs = aux.RawTableIDs
	} else if aux.RawTableID != nil && *aux.RawTableID != "" {
		r.TableIDs = []string{*aux.RawTableID}
	}

	if len(r.TableIDs) == 1 {
		r.TableID = &r.TableIDs[0]
	} else {
		r.TableID = nil
	}

	if r.Status == "" {
		r.Status = "confirmed"
	}
	if r.Revision == 0 {
		r.Revision = 1
	}
	return nil
}

// AvailabilityOption represents an available single table or combinable pair
type AvailabilityOption struct {
	TableIDs []string `json:"table_ids"`
	Capacity int      `json:"capacity"`
}

// AvailabilitySlot represents an available interval on the booking grid
type AvailabilitySlot struct {
	StartsAtLocal     string               `json:"starts_at_local"`
	StartsAt          string               `json:"starts_at"`
	AvailableTableIDs []string             `json:"available_table_ids"`
	AvailableOptions  []AvailabilityOption `json:"available_options,omitempty"`
	Explain           []TableExplain       `json:"explain,omitempty"`
}

// ReservationMoveRequest defines a target change for an existing reservation
type ReservationMoveRequest struct {
	Reference        string   `json:"reference"`
	TableIDs         []string `json:"table_ids,omitempty"`
	TableID          *string  `json:"table_id,omitempty"`
	StartsAtLocal    *string  `json:"starts_at_local,omitempty"`
	PartySize        *int     `json:"party_size,omitempty"`
	ExpectedRevision *int     `json:"expected_revision,omitempty"`
}

// DecisionResponse is returned by GET /reservations/{ref}/decision
type DecisionResponse struct {
	Reference     string        `json:"reference"`
	Revision      int           `json:"revision"`
	AcceptedTerms AcceptedTerms `json:"accepted_terms"`
}

// HistoryChange describes a modified field
type HistoryChange struct {
	Field string      `json:"field"`
	From  interface{} `json:"from"`
	To    interface{} `json:"to"`
}

// HistoryEntry is a single state transition in a reservation's lifecycle
type HistoryEntry struct {
	Seq           int             `json:"seq"`
	At            string          `json:"at"`
	Event         string          `json:"event"` // "created", "changed", "cancelled", "reassigned"
	Changes       []HistoryChange `json:"changes"`
	Revision      int             `json:"revision"`
	AcceptedTerms AcceptedTerms   `json:"accepted_terms"`
	PlanID        *string         `json:"plan_id,omitempty"`
}

// HistoryResponse is returned by GET /reservations/{ref}/history
type HistoryResponse struct {
	Reference string         `json:"reference"`
	Entries   []HistoryEntry `json:"entries"`
}

// SeriesOccurrence is a single booking within a recurring series
type SeriesOccurrence struct {
	Index       int         `json:"index"`
	Reference   string      `json:"reference"`
	Exception   bool        `json:"exception"`
	Reservation Reservation `json:"reservation"`
}

// SeriesResponse is returned by POST /series and GET /series/{series_id}
type SeriesResponse struct {
	SeriesID      string             `json:"series_id"`
	Revision      int                `json:"revision"`
	IntervalWeeks int                `json:"interval_weeks"`
	Occurrences   []SeriesOccurrence `json:"occurrences"`
}

// Closure represents a scheduled table downtime interval
type Closure struct {
	TableID string `json:"table_id"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// ReplanRequest is the body for POST /restaurants/{id}/replans
type ReplanRequest struct {
	TableID string `json:"table_id"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Assignment represents a table assignment for a considered booking in a replan
type Assignment struct {
	Reference string   `json:"reference"`
	TableIDs  []string `json:"table_ids"`
	Changed   bool     `json:"changed"`
}

// ReplanResponse is returned by POST /restaurants/{id}/replans
type ReplanResponse struct {
	PlanID             string       `json:"plan_id"`
	RestaurantRevision int          `json:"restaurant_revision"`
	Closure            Closure      `json:"closure"`
	Assignments        []Assignment `json:"assignments"`
	MovedCount         int          `json:"moved_count"`
	UnusedSeats        int          `json:"unused_seats"`
}

// ApplyPlanResponse is returned by POST /restaurants/{id}/replans/{plan_id}/apply
type ApplyPlanResponse struct {
	PlanID             string        `json:"plan_id"`
	RestaurantRevision int           `json:"restaurant_revision"`
	Reservations       []Reservation `json:"reservations"`
}

// AmendSeriesRequest is the body for POST /series/{series_id}/amend
type AmendSeriesRequest struct {
	ExpectedRevision int    `json:"expected_revision"`
	FromIndex        int    `json:"from_index"`
	LocalTime        string `json:"local_time"`
}

// FixtureData is used by POST /_test/reset
type FixtureData struct {
	Users        []User        `json:"users"`
	Restaurants  []Restaurant  `json:"restaurants"`
	Reservations []Reservation `json:"reservations"`
}

// ExportState represents the opaque serializable snapshot for /_test/export and /_test/import
type ExportPayload struct {
	Track         string      `json:"track"`
	FormatVersion int         `json:"format_version"`
	State         ExportState `json:"state"`
}

type ExportState struct {
	Users        []User               `json:"users"`
	Tokens       map[string]string    `json:"tokens"` // token -> user_id
	Restaurants  []Restaurant         `json:"restaurants"`
	Reservations []Reservation        `json:"reservations"`
	Idempotency  []IdempotencyItem    `json:"idempotency"`
	Policies     []PolicyExport       `json:"policies,omitempty"`
	Series       []SeriesExport       `json:"series,omitempty"`
	Closures     []TableClosureExport `json:"closures,omitempty"`
}

type PolicyExport struct {
	RestaurantID string `json:"restaurant_id"`
	Policy       Policy `json:"policy"`
}

type SeriesExport struct {
	SeriesID      string             `json:"series_id"`
	RestaurantID  string             `json:"restaurant_id"`
	OwnerUserID   string             `json:"owner_user_id"`
	Revision      int                `json:"revision"`
	IntervalWeeks int                `json:"interval_weeks"`
	Occurrences   []SeriesOccurrence `json:"occurrences"`
}

type TableClosureExport struct {
	RestaurantID string `json:"restaurant_id"`
	TableID      string `json:"table_id"`
	FromUTC      int64  `json:"from_utc"`
	ToUTC        int64  `json:"to_utc"`
}

type IdempotencyItem struct {
	UserID       string `json:"user_id"`
	Key          string `json:"key"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	BodyHash     string `json:"body_hash"`
	StatusCode   int    `json:"status_code"`
	ResponseBody string `json:"response_body"`
}
