# CHANGES.md — Stage 2: Web UI & Combinable Tables

This document outlines the enhancements, bug repairs, and architectural extensions implemented in Stage 2 based on the official specification and test audit.

## 1. Summary of Deliverables
- **Embedded Web UI (`web/index.html`):** Single-file responsive interface embedded in the Go binary serving `/`, `/signup`, `/login`, and `/lookup`.
- **Combinable Table Engine (`engines/booking/`):** Generation of single and combined table options, multi-table occupancy tracking, and non-transitive combination validation.
- **Frontend Resilience:** Monotonic search sequence IDs for out-of-order resolution, pre-generated UUID idempotency keys, `booking-uncertain` state on lost connection, and `booking-error` on concurrency conflict.
- **Backward Upgrade Compatibility:** Full support for Stage 1 export/import with preserved tokens, hashed passwords, and pending idempotency retries.

---

## 2. Issues & Failure Repairs

### Issue 1: Out-of-Order Search Responses Overwriting New Grid Results
- **Failure:** When a user searched date A then quickly date B, a slower response from A could arrive after B and overwrite the screen with stale slots.
- **Root Cause:** Asynchronous `fetch` calls did not track request issue sequence.
- **Fix:** Introduced monotonic request counter `currentSearchSeq`. Responses are discarded if their sequence number is less than the active search sequence.
- **Verification:** Verified with Playwright rapid search tests; late responses are properly ignored.

### Issue 2: Combinable Table Transitivity Violation
- **Failure:** Having combinable pairs `["t_1", "t_2"]` and `["t_2", "t_3"]` improperly allowed booking `{t_1, t_3}`.
- **Root Cause:** Initial combination check checked if tables shared an adjacency group rather than checking exact declared pairs.
- **Fix:** Enforced strict bidirectional declared pair lookup (`table_a == a && table_b == b || table_a == b && table_b == a`). Undeclared pairs reject with `422 combination_not_allowed`.
- **Verification:** `test_combining_is_not_transitive` passed with 422 `combination_not_allowed`.

### Issue 3: Table ID Representation in Responses
- **Failure:** Multi-table bookings incorrectly emitted `"table_id": "t_1"` or failed JSON schema validation when 2 tables were booked.
- **Root Cause:** Single table ID field was populated when `len(table_ids) > 1`.
- **Fix:** Custom serialization omitting `"table_id"` when multiple tables are assigned, including `"table_id"` only when exactly 1 table is booked, and always providing `"table_ids": [...]`.
- **Verification:** `test_booking_a_declared_pair` and `test_table_id_is_still_accepted_and_means_a_set_of_one` passed.

### Issue 4: Idempotent Retry on Uncertain Network Outcome
- **Failure:** Network failure after submission did not retain the original idempotency key, causing retries to attempt a new booking.
- **Root Cause:** Key was generated on form submit rather than form display.
- **Fix:** Pre-generate the idempotency UUID when the modal opens; on fetch failure, display `booking-uncertain` and retry using the exact same body and key.
- **Verification:** Browser retry recovering original confirmation passed Playwright test suite.
