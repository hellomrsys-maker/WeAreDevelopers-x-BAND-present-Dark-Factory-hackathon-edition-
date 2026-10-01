# FACTORY.md — Dark Factory Engineering & Operating Manual

## 1. Executive Summary

This Dark Factory stands up an autonomous software engineering organization within BAND Desktop to construct, verify, and evolve a mission-critical service through four progressive stages without continuous human intervention.

The factory adopts an **"AI per engine"** paradigm: rather than having a single model attempt to write an entire monolithic service, responsibilities are segregated into specialized functional engines bounded by strict interface contracts.

---

## 2. Band Roster & Seat Topology

The factory operates four specialized seats configured with Claude 3.5 Sonnet on the Claude Code harness:

```
+-------------------------------------------------------------+
|                        BAND DESKTOP                         |
|                                                             |
|   +-------------+       Handoff        +---------------+    |
|   |  @planner   | -------------------> |   @builder    |    |
|   +-------------+                      +---------------+    |
|          ^                                     |            |
|          | Defect                              | Review     |
|          | Feedback                            v Handoff    |
|   +-------------+       Verify         +---------------+    |
|   |   @tester   | <------------------- |   @reviewer   |    |
|   +-------------+                      +---------------+    |
+-------------------------------------------------------------+
```

### Seat Mandates & Profiles

| Seat | Role | Mandate File | Harness & Model | Responsibilities |
|---|---|---|---|---|
| `@planner` | System Architect | `mandates/planner.md` | Claude Code / Claude 3.5 Sonnet | Deconstructs incoming stage prompts into formal Go interface contracts. Coordinates stage flow and verifies participant presence. |
| `@builder` | Implementation Engineer | `mandates/builder.md` | Claude Code / Claude 3.5 Sonnet | Implements engine components, data store schemas, and HTTP endpoints against the contracts. Runs local unit tests. |
| `@reviewer` | QA & Specification Auditor | `mandates/reviewer.md` | Claude Code / Claude 3.5 Sonnet | Audits committed code against specification invariants, checks edge-case handling, and rejects contract regressions. |
| `@tester` | Integration & Stress Tester | `mandates/tester.md` | Claude Code / Claude 3.5 Sonnet | Executes integration suites, race detection (`go test -race`), and container build validation in clean offline environments. |

---

## 3. Architectural Design Rationale

### Why Go for Every Engine?
1. **Single Binary Deployment:** Compiles to a single static binary. No external runtime, interpreter, or runtime package manager is needed inside the final container.
2. **Zero Runtime Network Requirement:** Using pure Go SQLite (`modernc.org/sqlite`) means no CGO, no dynamic linking, and zero outbound network calls at startup.
3. **Concurrency Safety:** Go's channels, mutexes, and goroutines provide clean primitive building blocks for the strict non-overlapping table reservation invariants.
4. **Embedded Frontend (`go:embed`):** Web templates, styles, and scripts are baked directly into the Go executable, eliminating Node.js or separate web server runtimes.

---

## 4. Engine Layout

```
stage-1/
├── contracts/        # Pure interfaces defining domain boundaries
├── engines/
│   ├── calendar/     # IANA timezones, opening hours, slot intervals
│   ├── booking/      # Table allocation, half-open interval collision check
│   ├── audit/        # Append-only history logger
│   └── guard/        # Invariant self-testing assertions
├── api/              # HTTP routers, validation, idempotency key caching
├── store/            # SQLite with WAL mode & ACID transactions
└── web/              # HTML5, CSS3, Vanilla JS embedded in Go binary
```

---

## 5. Recovery & Error Handling Protocol

When bad work or a defect is produced:
1. **Reviewer Rejection:** If `@builder` misses a specification detail (e.g., incorrect error code or missing idempotency replay), `@reviewer` generates a concrete failure log referencing the contract and requests a revised commit.
2. **Tester Race Detection:** If `@tester` discovers a race condition during concurrent booking simulations (`go test -race`), the exact goroutine trace is routed back to `@builder` to add mutex locks or transactional isolation (`BEGIN IMMEDIATE`).
3. **Zero Human Steering:** All corrections occur via direct inter-agent handoffs inside the room.
