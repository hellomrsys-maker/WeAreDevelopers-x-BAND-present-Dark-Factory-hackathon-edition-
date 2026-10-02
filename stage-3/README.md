# Tablekeeper — Stage 3

Stage 3 extends the Tablekeeper reservation service with dated booking policies, availability explanations, reservation history tracking, recurring series agreements, and an isolated double-entry payments module.

## Architecture

- **`contracts/`:** Public Go interfaces defining contracts for `Calendar`, `Booking`, `Audit`, `Guard`, `Money`, and `Payments`.
- **`engines/`:** Independent engine implementations communicating strictly via interfaces:
  - `calendar/`: Opening hours, slot generation, dated policy version resolution, and DST calculations.
  - `booking/`: Reservation lifecycle, availability explanations, history logging, and recurring series adoptions.
  - `money/`: Double-entry accounting ledger operating exclusively with `int64` minor units and largest-remainder rounding splits.
  - `payments/`: Online booking deposits, cancellation/no-show fee logic, and staff offline payments under `/payments/...`.
  - `audit/`: Append-only structured event log.
  - `guard/`: Runtime invariant verification (no overlapping bookings, double-entry sum = 0).
- **`api/`:** HTTP routes, strict type validation (banning booleans as integers), idempotency replay cache.
- **`store/`:** Pure Go SQLite database driver (`modernc.org/sqlite`) with WAL mode, foreign keys, and zero CGO dependencies.
- **`web/`:** Embedded single-page web application.

## Endpoints Added in Stage 3

- `GET /availability?...&explain=true`: Reports `capacity` and `no_overlap` rules per table.
- `GET /reservations/{reference}/history`: 1-indexed historical event log.
- `GET /reservations/{reference}/decision`: Accepted terms and revision snapshot.
- `POST /restaurants/{id}/policies`: Manager policy publication.
- `GET /restaurants/{id}/policies`: Public publication history.
- `POST /series`: Recurring series adoption.
- `GET /series/{series_id}`: Recurring series state and occurrences.
- `/payments/...`: Isolated deposit and refund endpoints.

## Testing & Verification

```bash
# Run all Go tests
go test -v ./...

# Run Stage 3 harness tests
python -m pytest tablekeeper/test/stage_3/test_sample.py -v -p harness.plugin --base-url http://localhost:8080
```
