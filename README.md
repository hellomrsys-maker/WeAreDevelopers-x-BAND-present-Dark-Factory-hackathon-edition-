# 🏭 AI Dark Factory — Autonomous Tablekeeper System

[![Track: Tablekeeper](https://img.shields.io/badge/Track-Tablekeeper-orange.svg)](https://lablab.ai/ai-hackathons/wearedevelopers-hackathon)
[![Event](https://img.shields.io/badge/Hackathon-WeAreDevelopers%20x%20BAND-blue.svg)](https://lablab.ai/ai-hackathons/wearedevelopers-hackathon)
[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![Tests](https://img.shields.io/badge/Tests-100%25%20Passing-brightgreen.svg)]()
[![Gates](https://img.shields.io/badge/Gates%201--4-Passed-success.svg)]()

> Official submission for **WeAreDevelopers x BAND present: Dark Factory (hackathon edition)**.  
> An autonomous software factory engineered inside **BAND Desktop** that planned, implemented, reviewed, and tested a clean-room restaurant reservation service across all 4 stages with zero human intervention.

---

## 👥 Team & Track

- **Track:** `tablekeeper` (Restaurant reservation service)
- **Factory Platform:** BAND Desktop
- **Agent Roster:** 4 Distinct Seats (`Planner`, `Builder`, `Reviewer`, `Adversarial Tester`)
- **Execution Model:** Fully autonomous dark-factory pipeline (Human input strictly limited to initial task dispatch).
- **Session Provenance:** Preserved in [`room.json`](./room.json).

---

## ⚡ Highlights & Key Achievements

- **Zero-CGO Pure Go Binary:** Entire service compiles into a single, self-contained static executable with embedded Web UI assets (`//go:embed web/index.html`) and zero external npm/node dependencies.
- **Offline SQLite in WAL Mode:** Transactional ACID store using pure Go `modernc.org/sqlite` with `PRAGMA busy_timeout = 5000` and thread-safe connection pooling.
- **Exact Combinatorial Replanning Solver:** Exhaustive branch-and-bound solver strictly minimizing `(moved_bookings, unused_seats, option_ranks)` for table closures in Stage 4.
- **100% Test Conformance:**
  - **Stage 1:** 120 / 120 passed (100%)
  - **Stage 2:** 24 / 24 browser tests passed (100%)
  - **Stage 3:** 7 / 7 policy & series tests + 120 regressions passed (100%)
  - **Stage 4:** 6 / 6 replanning tests + 127 regressions passed (100%)

---

## 🏛️ System Architecture

The engine is structured as decoupled domain engines communicating exclusively through strict Go interfaces in `contracts/`:

```
┌────────────────────────────────────────────────────────┐
│                      HTTP Routing                      │
│             (/api, /health, /_test/reset)              │
├──────────────────────────┬─────────────────────────────┤
│   Idempotency Filter     │    Search Sequence Guard    │
├──────────────┬───────────┴───┬──────────────┬──────────┤
│   Calendar   │    Booking    │    Audit     │  Guard   │
│    Engine    │    Engine     │    Engine    │  Engine  │
├──────────────┴───────────────┴──────────────┴──────────┤
│             Double-Entry Accounting / Ledger           │
├────────────────────────────────────────────────────────┤
│           Pure Go SQLite Store (WAL + Locks)           │
└────────────────────────────────────────────────────────┘
```

- **Contracts Layer (`contracts/`):** Pure abstract interfaces enforcing clean-room separation.
- **Calendar Engine (`engines/calendar/`):** IANA timezone parsing, operational grids, cutoffs, and slot computation.
- **Booking Engine (`engines/booking/`):** Table allocation, half-open interval occupancy `[start, end)`, combinable pair validation, and atomic multi-reservation moves (`POST /reservation-moves`).
- **Audit Engine (`engines/audit/`):** Monotonic append-only event store capturing all state transitions with strict sequence numbers.
- **Guard Engine (`engines/guard/`):** Database invariant assertions (zero overlapping reservations, foreign-key integrity).
- **Payments Engine (`engines/payments/`):** Integer minor-unit ledger guaranteeing `SUM(amount) == 0`.

---

## 📂 Stage Breakdown

Every stage folder is a complete, buildable, independent service:

| Stage | Folder | What was Built | Test Pass Rate |
|---|---|---|---|
| **1** | [`stage-1/`](./stage-1) | JSON API, atomic moves, idempotent requests, state export/import | **120 / 120 (100%)** |
| **2** | [`stage-2/`](./stage-2) | Embedded responsive UI, Playwright test IDs, table pairs, search seq deduplication | **24 / 24 (100%)** |
| **3** | [`stage-3/`](./stage-3) | Effective-dated policy versions, 1-indexed history event chains, recurring series | **7 / 7 (100%)** |
| **4** | [`stage-4/`](./stage-4) | Branch-and-bound table closure replanning solver, series batch amendments | **6 / 6 (100%)** |

---

## 🛡️ Hackathon Gate Compliance

1. **Gate 1 (Distinct Seats & Mandates):** 4 configured seats with explicit `mandates/` naming runtime harness and model.
2. **Gate 2 (Seat Collaboration):** Verified reciprocal `@handle` handoffs and reviews recorded in `room.json`.
3. **Gate 3 (Clean Container Execution):** Every stage folder provides an isolated `Dockerfile` and `RUN.md` serving `/health` cleanly with no network egress.
4. **Gate 4 (Generic Mandates):** Zero track-specific keywords in `mandates/` — fully generic software engineering organizational model.

---

## 🚀 How to Run & Verify

### 1. Run Offline Compliance Gate Check
```bash
python -m harness check . --track tablekeeper
```

### 2. Build and Run Stage 4 Service Locally
```bash
cd stage-4
go build -o server ./cmd/server
./server
```

Check health:
```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

### 3. Run with Docker
```bash
cd stage-4
docker build -t tablekeeper:stage-4 .
docker run -p 8080:8080 tablekeeper:stage-4
```

### 4. Run Conformance Test Harness
```bash
python -m harness run --track tablekeeper --base-url http://localhost:8080 --stages 1
```

---

## 📖 Factory Documentation
For full details on the seat design rationale, handoff protocol, failure recovery loops, and measured token/wall-clock costs, see **[`FACTORY.md`](./FACTORY.md)** and **[`SEATS.md`](./SEATS.md)**.
