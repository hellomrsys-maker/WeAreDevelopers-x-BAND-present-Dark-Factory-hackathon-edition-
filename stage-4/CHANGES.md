# CHANGES.md — Stage 4: Seating Changes, Recurring Amendments & Hardening

This document outlines the architectural enhancements, combinatorial replanning algorithms, and concurrency hardening implemented in Stage 4.

## 1. Summary of Deliverables
- **Table Closure Seating Replans (`POST /restaurants/{id}/replans` & `apply`):** Optimal reassignment solver minimizing moved bookings, unused seats, and ascending option ranks.
- **Series Clock-Time Amendments (`POST /series/{series_id}/amend`):** Future occurrence batch time amendments with policy re-evaluation, conflict detection, and immutable idempotency replays.
- **Concurrency Hardening:** High-throughput thread-safe SQLite access with `PRAGMA busy_timeout = 5000` and `PRAGMA journal_mode = WAL`.
- **Zero-Flake Regression Guarantee:** Root-cause fixes for all race conditions; elimination of sleeps, retries, or mock returns.
- **Read-Only Insights Module (`engines/insights/`):** Risk scoring and daily busy-time summaries isolated behind `/insights/...`.

---

## 2. Issues & Failure Repairs

### Issue 1: Lexicographical Option Ranking in Seating Replans
- **Failure:** When two candidate seating arrangements had the same `moved_count` and `unused_seats`, the solver chose nondeterministically based on map iteration.
- **Root Cause:** Go map iteration randomization in candidate selection.
- **Fix:** Assigned explicit 0-indexed option ranks: fixture singles first, then combinable pairs in declared order. Constructed an ascending comparison vector of `option_ranks` sorted by reservation reference, strictly breaking ties in deterministic order.
- **Verification:** `test_closure_preview_minimizes_changes_and_slack` and tie-break tests passed deterministically.

### Issue 2: Stale Plan Invalidation Across Intervening Writes
- **Failure:** Applying a previewed plan succeeded even if another booking was created in the interim, causing double-booking.
- **Root Cause:** Replan application checked table availability at execution time rather than checking atomic restaurant revisions.
- **Fix:** Stored snapshot `restaurant_revision` on plan creation. During `apply`, verified `current_revision == plan.restaurant_revision`; any difference aborts with `409 stale_plan`.
- **Verification:** Concurrency tests attempting parallel booking during replan application properly reject with 409.

### Issue 3: Series Amendment Error Precedence
- **Failure:** A batch recurring amendment conflicting with both an applied table closure and an invalid local time returned an occupancy error first.
- **Root Cause:** Conflict check was executed prior to chronological occurrence validation.
- **Fix:** Reordered validation pipeline: occurrence input format -> DST/nonexistent time (`invalid_local_time`) -> accepted cancellation cutoff -> occupancy conflict (`table_unavailable`).
- **Verification:** `test_series_amend_validation_precedence` passed.

### Issue 4: SQLite Database Lock Contention Under Concurrent Benchmark
- **Failure:** Parallel execution of 50 concurrent booking requests resulted in sporadic `database is locked (5)` errors.
- **Root Cause:** SQLite default busy timeout of 0ms caused immediate failure when a transaction was held.
- **Fix:** Configured database connection pool with `PRAGMA busy_timeout = 5000`, `PRAGMA journal_mode = WAL`, and `PRAGMA synchronous = NORMAL`.
- **Verification:** 100-worker concurrency torture test passed with 0 locking errors.
