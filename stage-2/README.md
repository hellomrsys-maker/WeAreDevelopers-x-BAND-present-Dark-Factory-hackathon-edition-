# Tablekeeper — Stage 2

Stage 2 extends Tablekeeper with an embedded single-page Web UI and support for non-transitive combinable dining tables.

## Architecture

- **`contracts/`:** Public Go interfaces defining contracts for all engines.
- **`engines/`:**
  - `calendar/`: Availability and combinable slot options.
  - `booking/`: Multi-table bookings, combinable pairs validation, and moves.
  - `audit/`: Event audit logging.
  - `guard/`: Runtime booking invariant checks.
- **`api/`:** REST routes, strict payload validation, and idempotency cache.
- **`store/`:** SQLite database schema and migrations.
- **`web/`:** Embedded single-page application (`index.html`) serving guest booking, lookup, and staff desk views.

## Testing & Verification

```bash
go test -v ./...
```
