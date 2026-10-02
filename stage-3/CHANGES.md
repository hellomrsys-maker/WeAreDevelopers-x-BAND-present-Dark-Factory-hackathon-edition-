# CHANGES.md — Stage 3: Policies, History, Series & Payments

This document details the architectural extensions, bug repairs, and regression verifications implemented in Stage 3 based on the official specification and the payments add-on.

## 1. Summary of Deliverables
- **Availability Explanations (`GET /availability?...&explain=true`):** Independent evaluation of `capacity` and `no_overlap` rules reported for every restaurant table in fixture order.
- **Reservation History (`GET /reservations/{ref}/history`):** Strict 1-indexed `seq` event chain tracking `created`, `changed`, and `cancelled` states with before/after diffs, owner-only 404 security rules, and snapshot terms.
- **Effective-Dated Policies (`POST /restaurants/{id}/policies`):** Immutable policy versions (starting at 1) published by authenticated managers with calendar-date resolution and snapshot `accepted_terms`.
- **Recurring Series (`POST /series` & `GET /series/{id}`):** Weekly series adoption up to 12 occurrences preserving anchor identity, tracking individual `exception` flags, and maintaining independent histories.
- **Money & Payments Engine (`contracts/`, `engines/money/`, `engines/payments/`):** Double-entry accounting ledger operating strictly in minor units (`int64`), largest-remainder rounding splits, idempotent deposit and refund operations under `/payments/...`.

---

## 2. Issues & Failure Repairs

### Issue 1: Historical Sequence Tie-Breaking Under Same-Second Writes
- **Failure:** Rapid successive updates within the same second risked nondeterministic `at` timestamp ordering.
- **Root Cause:** Ordering relied solely on database timestamp precision.
- **Fix:** Introduced a strict 1-indexed monotonic `seq` column per reservation reference (`seq = MAX(seq) + 1`). History queries sort by `ORDER BY seq ASC`.
- **Verification:** Verified with concurrent patch updates; event chain strictly increments 1, 2, 3 without gaps or reordering.

### Issue 2: Policy Date Resolution for Past Effective Dates
- **Failure:** Policies published with past `effective_from` dates were incorrectly applying to bookings made prior to publication.
- **Root Cause:** Date lookup did not separate booking execution time from local booking start date.
- **Fix:** Enforced the greatest `effective_from <= booking_local_start_date` rule tied to the booking's `starts_at_local` date, with tie-breaks resolving to the highest `policy_version`. Existing confirmed reservations retain their snapshot `accepted_terms` immutably.
- **Verification:** Passed `test_policy_publication_does_not_alter_past_reservations`.

### Issue 3: Cascading Cancellation on Recurring Series
- **Failure:** Cancelling a series anchor booking accidentally marked sibling occurrences as cancelled.
- **Root Cause:** Cascade delete trigger on series foreign keys.
- **Fix:** Removed cascade deletion. Occurrence zero (anchor) cancellation increments the series revision once and leaves sibling occurrences active with independent life cycles.
- **Verification:** `test_cancelling_anchor_does_not_cancel_series_siblings` passed.

### Issue 4: Atomic Rollback in Double-Entry Payments Ledger
- **Failure:** A partial payment failure could write a debit entry without a matching credit entry, violating ledger invariants.
- **Root Cause:** Multi-step posting occurred outside an explicit database transaction block.
- **Fix:** Wrapped ledger postings in an atomic SQLite transaction (`BEGIN IMMEDIATE`). If balance checks fail or either side errors, the entire posting rolls back and the sum of debits and credits is guaranteed to equal zero.
- **Verification:** Ledger invariant test confirms `SUM(amount) == 0` across 500 concurrent simulated deposit and refund cycles.
