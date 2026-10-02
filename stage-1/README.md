# Tablekeeper — Stage 1

Stage 1 is the core reservation engine implementing table availability, bookings, cancellations, amendments, collective moves, and idempotent replay semantics.

## Architecture

- **`contracts/`:** Public Go interfaces for `Calendar`, `Booking`, `Audit`, and `Guard`.
- **`engines/`:**
  - `calendar/`: Opening hours, slot generation, and timezone offsets.
  - `booking/`: Reservation creation, amendments, cancellations, and collective atomic moves.
  - `audit/`: Event audit trail.
  - `guard/`: Runtime booking invariant checks.
- **`api/`:** REST routes, strict payload validation, and idempotency cache.
- **`store/`:** SQLite database schema and migrations (`modernc.org/sqlite`).

## Testing & Verification

```bash
go test -v ./...
```
