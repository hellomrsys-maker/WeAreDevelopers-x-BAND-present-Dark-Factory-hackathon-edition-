# Tablekeeper — Stage 4

Stage 4 is the final extension and hardening stage of Tablekeeper, featuring optimal seating replans for closed tables, recurring series clock-time amendments, an isolated Insights engine, and zero-concurrency-flake runtime hardening.

## Architecture

- **`contracts/`:** Public Go interfaces defining contracts for all system engines.
- **`engines/`:** Modular engine implementations:
  - `calendar/`: Availability calculations, dated policy rules, and opening intervals.
  - `booking/`: Reservation lifecycle, history logging, recurring series adoptions, and optimal combinatorial replanning.
  - `money/`: Double-entry accounting ledger in `int64` minor units.
  - `payments/`: Isolated payment processing under `/payments/...`.
  - `insights/`: Read-only risk scoring and busy-time summaries under `/insights/...`.
  - `audit/`: Append-only structured audit trails.
  - `guard/`: Runtime invariant verification (no overlapping bookings, double-entry sum = 0).
- **`api/`:** HTTP routes, strict type validation, and idempotent replay handlers.
- **`store/`:** Pure Go SQLite database driver (`modernc.org/sqlite`) configured in WAL mode with 5000ms busy timeout.
- **`web/`:** Embedded single-page guest booking and staff management interface.

## Endpoints Added in Stage 4

- `POST /restaurants/{id}/replans`: Operator preview for table closure seating rearrangement.
- `POST /restaurants/{id}/replans/{plan_id}/apply`: Atomic application of proposed seating plan.
- `POST /series/{series_id}/amend`: Batch clock-time amendments for recurring series.
- `GET /insights/risk/{reference}`: Deterministic no-show risk assessment.
- `GET /insights/busy-times`: Aggregated restaurant utilization metrics.

## Testing & Verification

```bash
# Run all Go tests
go test -v ./...

# Run Stage 4 harness tests
python -m pytest tablekeeper/test/stage_4/test_sample.py -v -p harness.plugin --base-url http://localhost:8080
```
