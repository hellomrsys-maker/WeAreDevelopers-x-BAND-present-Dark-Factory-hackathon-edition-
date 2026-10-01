# AI Dark Factory — Autonomous Multi-Engine System

This repository is an autonomous AI Dark Factory implementation for the WeAreDevelopers x BAND Hackathon.

## Architecture

The system is constructed with a modular multi-engine architecture compiled into a single self-contained Go binary with embedded Web UI assets and an offline SQLite transactional store:

- **Contracts Layer (`contracts/`):** Strict, decoupled interfaces defining domain boundaries.
- **Calendar Engine (`engines/calendar/`):** Manages time zones (IANA), opening hour intervals, slot grids, and cutoff rules.
- **Booking Engine (`engines/booking/`):** Handles table allocation, non-overlapping half-open interval occupancy `[start, start+duration)`, concurrency locks, and atomic multi-reservation moves.
- **Audit Engine (`engines/audit/`):** Append-only event store capturing all state transitions and history.
- **Guard Engine (`engines/guard/`):** Invariant assertion engine verifying safety guarantees (e.g., zero double-bookings).
- **Store Layer (`store/`):** SQLite database in WAL mode using pure Go (`modernc.org/sqlite`), zero CGO, fully transactional.
- **API & Idempotency Layer (`api/`):** Standard HTTP routing, request validation, and atomic `Idempotency-Key` replay caching.

## Stages

- `stage-1/`: Complete headless JSON API, atomic moves, and state export/import.
- `stage-2/`: Embedded responsive Web UI with Playwright `data-testid` endpoints and table pair combinations.
- `stage-3/`: Effective-dated policy versions, reservation history, and recurring series agreements.
- `stage-4/`: Table closure replanning solver and domain extensions (Money & Payments engine).
