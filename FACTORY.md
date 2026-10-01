# FACTORY.md

## 1. Summary
The Solo Rock Dark Factory is an autonomous 4-seat software engineering organization built in BAND Desktop to develop, verify, and evolve a clean-room restaurant reservation service (`tablekeeper`). Across 4 progressive stages, the factory achieved a 100% test pass rate (120/120 Stage 1 tests, 24/24 Stage 2 browser tests, 7/7 Stage 3 policy tests, and 6/6 Stage 4 replanning tests). The proudest result is a zero-CGO single compiled Go binary with embedded web UI, pure Go SQLite (`modernc.org/sqlite`), and an exact combinatorial branch-and-bound replanning solver that runs completely offline with zero outbound network calls and zero regressions.

## 2. Seat setup
| Seat | Mandate file | Model/agent used | What it is responsible for |
|------|--------------|------------------|----------------------------|
| Planner | `mandates/planner.md` | Claude 3.5 Sonnet / Claude Code | Stage requirement decomposition, Go interface contracts definition in `contracts/`, dependency ordering, and cross-seat workflow dispatch |
| Builder 1-3 | `mandates/builder.md` | Claude 3.5 Sonnet / Claude Code | Component implementations across 7 modular engines (`calendar`, `booking`, `audit`, `guard`, `api`, `store`, `web`, `money`, `payments`, `insights`), schema migrations, and unit tests |
| Reviewer | `mandates/reviewer.md` | Claude 3.5 Sonnet / Claude Code | Specification audits, interface contract compliance, error code validation, and rejecting code lacking verifiable test evidence |
| Adversarial tester | `mandates/adversarial-tester.md` | Claude 3.5 Sonnet / Claude Code | Edge-case fuzzing, concurrency simulations, time-zone DST boundaries, offline container builds, and pytest harness regression runs |

## 3. Design rationale
- **Why these seats and not fewer or more:** Fewer seats (e.g., merging Builder and Reviewer) leads to confirmation bias and missed edge cases in specification enforcement. More seats (e.g., individual seat per micro-engine) introduces communication overhead, message token bloat, and context fragmentation without added rigor.
- **Why engines are separated behind contracts:** Defining pure Go interfaces in `contracts/` enables true clean-room development, decoupled unit testing, and parallel engine development. No engine directly inspects another engine's database tables or internal structs.
- **Why the mandates are generic (the swap test):** The standing instructions in `mandates/` contain zero track-specific vocabulary (`table_id`, `party_size`, `starts_at_local`, etc. are strictly forbidden). The exact same mandates can direct a factory building a financial ledger, a hospital triage system, or a weather app. All domain specifics are injected solely via the initial stage task prompt.
- **What was tried and abandoned, and why:**
  - *Abandoned CGO-based SQLite (`mattn/go-sqlite3`):* Required gcc toolchains in the offline container and complicated cross-compilation; replaced with pure Go `modernc.org/sqlite` which compiles into a static binary.
  - *Abandoned external Node.js/Vite frontend builds:* A separate frontend build step introduced external npm dependencies and container bloat; replaced with single-file modern responsive HTML5/CSS3/ES6 embedded directly into the Go executable via `//go:embed web/index.html`.
  - *Abandoned greedy table reallocations:* Early greedy replanning heuristics failed lexicographical option tie-breakers; replaced with an exhaustive combinatorial branch-and-bound solver strictly minimizing `(moved_count, unused_seats, option_ranks)`.

## 4. Handoff protocol
Every handoff between seats is explicit, self-contained, and reproducible:
- **Builder -> Reviewer:** Must include:
  1. What was built (files created/modified, interfaces satisfied).
  2. How to run it (exact CLI commands and environmental requirements).
  3. Objective test evidence (full stdout of `go test -v ./...` showing passing assertions).
- **Reviewer -> Adversarial Tester:** Requires passing test output, verification that error codes match the spec exactly (e.g., `table_unavailable`, `stale_revision`), and zero unhandled errors. If any requirement fails, Reviewer rejects with reproducible reasons.
- **Adversarial Tester -> Planner:** Runs end-to-end integration suites, concurrency races (`go test -race`), and container builds, reporting stage completion or exact reproduction steps for failures.

## 5. How the factory catches bad work
- **Reviewer rejects work without test evidence:** Claims of completion without raw test logs are rejected immediately. Code is audited against the exact specification clauses.
- **Tester concurrency / retry / time zone / boundary tests:** Adversarial tester runs multi-threaded booking simulations (50 concurrent requests for the same table slot yielding exactly 1 success and 49 conflicts), spring-forward skipped hours, and fall-back repeated hours.
- **Guard invariant checks:** Guard engine verifies database integrity: no overlapping half-open intervals `[starts_at_utc, ends_at_utc)` on the same table, valid foreign keys, monotonic revision counters, and non-empty history seq chains.
- **Clean-container build check:** Validates that the multi-stage `Dockerfile` compiles and starts up within 60 seconds with no outbound network connectivity (`--network none`).

## 6. How the factory recovers
- **Routing & retry limits:** Rejected work returns to the original Builder seat with concrete reproduction commands and expected vs actual diffs. Retries are capped at 3 cycles; if an approach fails twice, Planner intervenes to revise interface contracts or architectural assumptions.
- **Stall & loop prevention:** Timeouts are enforced on test executions. If a seat repeats identical code without addressing the root cause, Reviewer halts the cycle and requires a minimal reproducer test first.
- **Real examples from our run:**
  1. *Empty JSON Slice Serialization (`null` vs `[]`):*
     - **Bug found:** Go uninitialized slices (`var s []string`) serialized to `null` in JSON, failing pytest assertions `slot["available_table_ids"] == []`.
     - **Caught by:** Adversarial tester running `tablekeeper/test/stage_1` availability tests.
     - **How fixed:** Refactored queries to initialize non-nil empty slices (`availableTableIDs := []string{}`) across all API responses.
     - **Evidence:** 120/120 Stage 1 tests passed.
  2. *Idempotency Replay Status Code (`201` vs `200`):*
     - **Bug found:** Idempotency middleware was replaying cached responses with HTTP 201 Created on subsequent requests.
     - **Caught by:** Reviewer verifying spec §7 ("First use: 201; Replay with same body: 200").
     - **How fixed:** `IdempotencyMiddleware` explicitly writes `w.WriteHeader(http.StatusOK)` on cache hits.
     - **Evidence:** Replay tests passed with HTTP 200 and identical body hashes.
  3. *Strict Type Checking (Banning Booleans as Integers):*
     - **Bug found:** Standard JSON unmarshaling into Go structs implicitly accepted booleans or floats in integer fields.
     - **Caught by:** Reviewer checking spec §5 ("invalid party_size values (including strings and booleans) ... are 422 validation_failed").
     - **How fixed:** Parsed request payloads into `map[string]interface{}` and enforced strict type assertions (`float64` with `float64(int(v)) == v`) while explicitly checking `v != true && v != false`.
     - **Evidence:** All invalid type test cases returned 422 `validation_failed`.
  4. *Table Closure Replanning Optimization Order:*
     - **Bug found:** Preliminary table reassignment solver minimized seat differences before checking moved booking count, violating spec §1.
     - **Caught by:** Adversarial tester comparing solver outputs against the test fixture hierarchy.
     - **How fixed:** Implemented hierarchical comparator prioritizing: 1. `moved_count`, 2. `unused_seats`, 3. lexicographical `option_ranks`.
     - **Evidence:** `test_a_closure_preview_returns_a_plan` and Stage 4 suites passed completely.

## 7. Measured costs (from the real run, per stage)
| Stage | Wall-clock time | Tokens / cost | Rejections | Fix cycles | Tests passing |
|-------|-----------------|---------------|------------|------------|---------------|
| 1 | 35 min | ~385k tokens | 1 | 2 | 120 / 120 (100%) |
| 2 | 20 min | ~220k tokens | 0 | 1 | 24 / 24 (100%) |
| 3 | 25 min | ~260k tokens | 0 | 1 | 7 / 7 + 120 regr (100%) |
| 4 | 30 min | ~340k tokens | 1 | 1 | 6 / 6 + 127 regr (100%) |

## 8. Autonomy statement
Across all 4 stages, the only human interaction with the factory was the initial dispatch of the stage task prompt into the BAND Desktop room. The factory operated with 100% autonomous code generation, contract negotiation, test execution, regression verification, and error recovery. Complete session provenance is preserved in [`room.json`](room.json) at the repository root.

## 9. Known limits
- **Single-node SQLite concurrency:** Under extreme write loads (>500 concurrent booking requests), SQLite file locking may produce short lock contention delays (mitigated by WAL mode and `PRAGMA busy_timeout = 5000`).
- **Memory footprint for large combinatorial replanning:** The replanning solver supports up to 6 tables, 4 combinable pairs, and 6 considered bookings within sub-millisecond execution; scaling to hundreds of concurrent bookings would require an integer linear programming (ILP) solver.

## 10. How to reproduce
1. **Prerequisites:** Go 1.22+, Python 3.11+, and Docker.
2. **Clone submission:**
   ```bash
   git clone <repo-url> dark-factory-submission
   cd dark-factory-submission
   ```
3. **Verify mandates (Gate 4):**
   ```bash
   python -m harness check . --track tablekeeper
   ```
4. **Build and test any stage (e.g., Stage 4):**
   ```bash
   cd stage-4
   go test -v ./...
   go build -o server.exe ./cmd/server
   ./server.exe &
   ```
5. **Run test harness suite:**
   ```bash
   python -m pytest tablekeeper/test/stage_4/test_sample.py -v -p harness.plugin --base-url http://localhost:8080
   ```
