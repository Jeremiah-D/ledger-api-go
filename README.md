# ledger-api-go

> **Portfolio reconstruction** — a rebuild project created to demonstrate ledger/settlement engineering skills. This is not production code from any employer.

## Inspiration

The design borrows its posting semantics from a publicly described concept: **"idempotent fill settlement"** — the idea that re-submitting the same settlement must never book twice. Double-entry accounting is used here purely as this repo's own implementation pattern; no claim is made about any employer's systems.

## What this implements

- An in-memory double-entry ledger (`ledger` package) written with the Go standard library only.
- Money is represented as integer cents (`int64`); floating point is never used for amounts.
- Every journal entry touches exactly two accounts: the debit account's balance increases by the amount, the credit account's decreases by the same amount, so the sum of all balances is always zero.
- Entries are validated before booking: non-empty ID, distinct non-empty debit/credit accounts, and `amount_cents > 0`.
- Every post commits atomically under one mutex: the journal row, the idempotency index, both net balances, and both per-account debit/credit totals land together — readers never see a half-posted entry.
- The ledger tracks per-account total debits and total credits alongside net balances, so any account's trial balance is a single read. `VerifyAccountingEquation` checks the books: every account's net equals its debits minus its credits, and total debits equal total credits ledger-wide.
- Posting is idempotent: submitting an entry with a previously used `IdempotencyKey` returns the original entry and books nothing again.
- All state is guarded by an `RWMutex`: reads (`Balance`, `Snapshot`, listings, chain verification) take the read lock and run concurrently, while `Post` and idempotency-key expiry take the write lock. Reads never block each other — only writers serialize.
- A per-account index maps each account to the IDs of the entries that touched it (as debit or credit leg), so per-account listing scans only that account's entries instead of the whole journal.
- Every post appends a tamper-evident audit-chain link: `SHA-256(prevHash || entry)` over a length-prefixed encoding of the entry's fields (standard library only). Rewriting a journaled entry, deleting a link, or splicing the chain breaks the hash continuity, and `VerifyChain` / `GET /entries/verify` detect it by recomputation.
- A thin `net/http` JSON API exposes the ledger: no external dependencies.

## API

### `POST /entries`

```bash
curl -s -X POST localhost:8080/entries \
  -H 'Content-Type: application/json' \
  -d '{"debit_account":"cash","credit_account":"equity","amount_cents":1000,"idempotency_key":"inv-001"}'
```

- First-time post → `201` with the entry JSON (the server generates `id` and `created_at`).
- Re-post with the same `idempotency_key` → `200` with the originally posted entry.
- Invalid entry (empty account, debit == credit, amount ≤ 0, malformed JSON,
  unknown JSON field, or trailing data after the JSON value) → `400` with an
  `{"error": ...}` body.
- Body larger than `LEDGER_MAX_BODY_BYTES` (default 1 MiB) → `413`.
- Either leg of the entry names a **frozen** account → `403`
  `{"error":"ledger: account is frozen"}`. The rejection is counted in
  `ledger_frozen_rejections_total`.
- The credit (payer) account is **overdraft-protected** and the posting would
  take its balance below zero → `422`
  `{"error":"ledger: posting would overdraw a protected account"}`. The
  rejection is counted in `ledger_overdraft_rejections_total`.

### `POST /transfers`

The payment-domain view of a posting: instead of debit/credit legs, the
caller names the payer (`from_account`) and the payee (`to_account`) and
the ledger books the double-entry pair **atomically** — either both
balance effects land or nothing does. A transfer that fails on either leg
(frozen account, overdraft, validation) records nothing: no journal row,
no chain link, no version bump.

```bash
curl -s -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"transfer_id":"tx-2026-001","from_account":"alice","to_account":"bob","amount_cents":2500,"idempotency_key":"pay-001"}'
# 201 {"transfer_id":"tx-2026-001","entries":[{...}],"duplicate":false}
```

- First-time transfer → `201` with the receipt; the journal entry carries
  the transfer's ID (`entries[0].id == transfer_id`), so transfers are
  directly visible in journal exports and the audit chain.
- Re-post with the same `idempotency_key` → `200` with the originally
  posted receipt (the idempotency namespace is shared with `POST
  /entries`).
- `transfer_id` is optional — the server generates one when omitted.
- Invalid transfer (empty ID/accounts, from == to, amount ≤ 0, transfer ID
  colliding with an existing entry ID, malformed JSON, unknown field) →
  `400`.
- Either leg names a **frozen** account → `403`; the payer is
  **overdraft-protected** and the transfer would take it below zero →
  `422`. Both are counted in the shared `ledger_frozen_rejections_total` /
  `ledger_overdraft_rejections_total` counters.

### `POST /accounts/{id}/freeze` and `POST /accounts/{id}/unfreeze`

Risk-control stop for an account (fintech wind-down / fraud hold). A frozen
account rejects every new `POST /entries` that names it as debit or credit
leg with `403`, while balance, snapshot, trial-balance, per-account
entries, chain verification, and the end-of-day reconcile keep working —
risk and reconciliation tooling can keep watching a stopped account.
Freezing is idempotent, never bumps the ledger version (balances are
unchanged by a freeze), and never touches the journal or the audit chain.

Ordering note: the frozen check runs *after* the idempotency-key replay
check. A key posted before the freeze still replays to its original entry
after the freeze (the replay books nothing new); only fresh bookings
through the frozen account are refused.

```bash
curl -s -X POST localhost:8080/accounts/cash/freeze
# {"account":"cash","frozen":true}
curl -s -X POST localhost:8080/accounts/cash/unfreeze
# {"account":"cash","frozen":false}
```

The reconcile report lists frozen accounts (`frozen_accounts`), and each
trial balance carries a `frozen` flag, so the day-end job shows which
accounts were stopped.

### Overdraft protection

Per-account guard against negative balances (fintech risk control —
customer cash accounts that must never go negative). Enable it with the
`LEDGER_NO_OVERDRAFT_ACCOUNTS` environment variable (comma-separated
account IDs) or the `ledger.WithOverdraftProtection` / 
`EnableOverdraftProtection` API. A `POST /entries` that would take a
protected credit (payer) account's balance below zero is rejected with
`422`; draining the account to exactly zero is allowed.

Protection is opt-in per account on purpose: internal accounts (suspense,
revenue, settlement) routinely carry negative balances under this
ledger's sign convention, so a blanket rule would break ordinary
bookkeeping. Check order inside `Post`: field validation → idempotency
replay → frozen (`403`) → overdraft (`422`). Like frozen rejections, an
overdraft rejection books nothing — no journal row, no chain link, no
version bump — and reads (balance, snapshot, trial balance, reconcile)
keep working. The reconcile report lists protected accounts
(`overdraft_protected_accounts`) and each trial balance carries an
`overdraft_protected` flag.

```bash
LEDGER_NO_OVERDRAFT_ACCOUNTS="cust-123,cust-456" ./ledger-api-go
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"cash","credit_account":"cust-123","amount_cents":1000}'
# 422 {"error":"ledger: posting would overdraw a protected account"}
```

### `GET /accounts/{id}/balance`

```bash
curl -s localhost:8080/accounts/cash/balance
# {"account":"cash","balance_cents":1000,"frozen":false}
```

### `GET /accounts/{id}/snapshot`

Balance plus the ledger version at read time. The version bumps on every
successful post (idempotent replays and rejected entries don't count), so
two snapshots with the same version are guaranteed to show the same
balance — a cheap change detector for reconciliation jobs.

```bash
curl -s localhost:8080/accounts/cash/snapshot
# {"account":"cash","balance_cents":1000,"version":3,"frozen":false}
```

### `GET /accounts/{id}/entries`

Per-account, time-windowed, cursor-paginated journal export — every entry
where the account appears as the debit or the credit leg. Same pagination
contract as `GET /entries`; the per-account index makes it O(k) in the
account's own entries rather than O(N) over the whole journal.

```bash
curl -s 'localhost:8080/accounts/cash/entries?limit=100'
# {"entries":[...],"next_cursor":"..."}   # empty next_cursor = last page
```

- Unknown accounts return `{"entries":[],"next_cursor":""}`, not 404 — the
  journal is append-only, so absence means "nothing yet".

### `GET /accounts/{id}/trial-balance`

The account's double-entry breakdown at the current ledger version:
every cent ever debited to it, every cent ever credited from it, and the
net balance. `net_balance_cents` always equals `total_debits_cents` minus
`total_credits_cents`; unknown accounts report zeros.

```bash
curl -s localhost:8080/accounts/cash/trial-balance
# {"account":"cash","total_debits_cents":1500,"total_credits_cents":500,"net_balance_cents":1000,"version":3,"frozen":false}
```

### `GET /entries`

Time-windowed, cursor-paginated export of the journal, sorted by
`(CreatedAt, ID)`.

```bash
curl -s 'localhost:8080/entries?since=2026-10-01T00:00:00Z&limit=100'
# {"entries":[...],"next_cursor":"..."}   # empty next_cursor = last page
```

- `since` / `until`: RFC3339 timestamps filtering `CreatedAt` in
  `[since, until)`. Omitted `since` means the beginning of time; omitted
  `until` means no upper bound.
- `limit`: page size, defaults to 100, capped at 1000.
- `cursor`: opaque cursor from the previous response's `next_cursor`.
  Resumption is keyed on entry ID, so entries posted between two pages are
  never duplicated or skipped.
- Malformed timestamps, cursors, or limits → `400` with an `{"error": ...}` body.

### `GET /entries/verify`

Recomputes the ledger's tamper-evident audit chain and reports whether it
is intact. The chain links every posted entry to the previous link's
SHA-256 hash (`SHA-256(prevHash || entry)`); idempotent replays add no link.

```bash
curl -s localhost:8080/entries/verify
# {"ok":true,"links":128,"head":"9f2c..."}   # empty ledger: links=0, head=64 zeros
```

- `200 {"ok":true,"links":N,"head":"<hex>"}` — the chain is intact; `head`
  moves if and only if a new entry was posted.
- `500 {"ok":false,"error":"..."}` — verification failed (rewritten entry,
  missing journal row, spliced/reordered chain). This is an
  operator-level integrity incident, not a client error.

### `POST /reconcile`

The end-of-day reconciliation job. Runs a full read-only scan of the live
ledger under a single read lock — one consistent snapshot — and returns
the report as indented JSON in the response body, ready to archive. A
reconcile run is always `200` even when it finds problems: the findings
live inside the report, not the status code.

```bash
curl -s -X POST localhost:8080/reconcile | tee reconcile-$(date +%F).json
```

Report fields:

| field                  | meaning |
|------------------------|---------|
| `generated_at`         | scan time (the server passes `time.Now()`) |
| `version`              | ledger version at scan time |
| `accounting_equation_ok` | full-ledger equation scan passed (every account: net == debits − credits; globally: debits == credits) |
| `accounting_error`     | the first equation violation, if any (omitted when clean) |
| `trial_balances`       | per-account trial balances for every account ever touched, sorted by account |
| `discrepancies`        | accounts where net != debits − credits, each with `total_debits_cents`, `total_credits_cents`, `net_balance_cents`, `expected_net_cents`, `difference_cents` (`[]` on a healthy ledger) |
| `total_debits_cents` / `total_credits_cents` | ledger-wide sums for at-a-glance balancing |
| `idempotency_keys`     | `ttl_configured`, `ttl`, `total_keys`, and `expired_eligible` (keys older than the TTL, i.e. the next sweep's eviction set) |
| `audit_chain`          | `verify_ok` / `verify_error`, `head`, `links`, plus `head_consistent` — the chain-length == ledger-version check with `consistency_error` when the counters desync |

A clean run looks like (trimmed):

```json
{
  "generated_at": "2026-10-07T18:00:00Z",
  "version": 3,
  "total_debits_cents": 1650,
  "total_credits_cents": 1650,
  "accounting_equation_ok": true,
  "trial_balances": [
    {"account": "cash", "total_debits_cents": 1500, "total_credits_cents": 150, "net_balance_cents": 1350, "version": 3},
    {"account": "equity", "total_debits_cents": 0, "total_credits_cents": 1500, "net_balance_cents": -1500, "version": 3},
    {"account": "fees", "total_debits_cents": 150, "total_credits_cents": 0, "net_balance_cents": 150, "version": 3}
  ],
  "discrepancies": [],
  "idempotency_keys": {"ttl_configured": false, "ttl": "0s", "total_keys": 3, "expired_eligible": 0},
  "audit_chain": {"verify_ok": true, "head": "9f2c…", "links": 3, "head_consistent": true}
}
```

Daily cron example (midnight, keep 90 days of reports):

```cron
0 0 * * * curl -s -X POST localhost:8080/reconcile | tee /var/ledger-reconcile/reconcile-$(date +\%F).json && find /var/ledger-reconcile -name 'reconcile-*.json' -mtime +90 -delete
```

Reports from a healthy ledger are byte-identical when scanned at the same
`generated_at`, so plain `diff` works for day-over-day comparisons.

### `GET /metrics`

Prometheus-format counters, rendered by hand with the standard library
only (no client library dependency):

```bash
curl -s localhost:8080/metrics
# # HELP ledger_posts_total Total POST /entries requests received.
# # TYPE ledger_posts_total counter
# ledger_posts_total 128
# ledger_idempotency_hits_total 5
# ledger_balance_queries_total 42
# ledger_transfers_total 12
# ledger_transfer_idempotency_hits_total 3
```

- `ledger_posts_total` — every `POST /entries` request received.
- `ledger_transfers_total` — every `POST /transfers` request received.
- `ledger_idempotency_hits_total` — posts that replayed an existing
  idempotency key (returned the original entry, booked nothing).
- `ledger_transfer_idempotency_hits_total` — transfers that replayed an
  existing idempotency key (returned the original receipt, booked
  nothing).
- `ledger_balance_queries_total` — `GET /accounts/{id}/balance` requests
  served. Snapshot reads are not counted.
- `ledger_verify_requests_total` — `GET /entries/verify` requests served.
- `ledger_reconcile_runs_total` — `POST /reconcile` requests served.
- `ledger_frozen_rejections_total` — `POST /entries` and `POST /transfers`
  requests rejected with `403` because an account was frozen.
- `ledger_overdraft_rejections_total` — `POST /entries` and `POST
  /transfers` requests rejected with `422` because the posting would have
  overdrawn an overdraft-protected account.

## Running

Requires Go 1.27+.

```bash
go vet ./...
go test -race ./...
go run .                 # listens on :8080; override with PORT, e.g. PORT=9090 go run .
```

Environment:

- `PORT` — listen address (default `8080`).
- `LEDGER_MAX_BODY_BYTES` — max accepted `POST /entries` body in bytes
  (default `1048576` = 1 MiB). Larger bodies are rejected with `413`.
  Entry payloads are tiny, so the default is generous while bounding the
  memory a single request can force the server to buffer.
- `LEDGER_IDEMPOTENCY_TTL` — how long idempotency keys are retained, e.g.
  `LEDGER_IDEMPOTENCY_TTL=72h`. Keys older than the TTL are evicted so the
  replay-detection index can't grow forever in a long-running process; the
  journal itself is append-only and never pruned. Pick a TTL longer than any
  client retry window. Unset means keys never expire.
- `SHUTDOWN_TIMEOUT` — how long a graceful shutdown waits for in-flight
  requests to drain after SIGINT/SIGTERM (default `10s`, e.g. `SHUTDOWN_TIMEOUT=30s`).
  New connections are refused immediately; requests already being served run to
  completion or until the timeout.

## Benchmarks

Single-goroutine `Ledger.Post` throughput and per-op latency, sampled on
every post:

| metric      | measured (2 runs, 2026-10-07) |
|-------------|-------------------------------|
| throughput  | 535k–583k entries/sec         |
| p50 latency | < 1 µs/op                     |
| p99 latency | 1–3 µs/op                     |

Machine: linux/amd64, AMD EPYC 9D25, Go 1.27.1. Numbers are
machine-specific; reproduce them with:

```bash
go test -run=NONE -bench=BenchmarkPost -benchtime=3s ./ledger/
```

(The raw runs were 535,093 entries/sec with p99 3 µs/op and
582,839 entries/sec with p99 1 µs/op.)

## Layout

```
.
├── ledger/
│   ├── ledger.go              # Ledger, JournalEntry, Post, Balance, Snapshot, ListEntries
│   ├── reconcile.go           # end-of-day reconciliation report (equation scan, trial-balance diffs, chain + idempotency checks)
│   ├── ledger_reconcile_test.go # reconciliation report tests (healthy, tampered, TTL, determinism)
│   ├── ledger_bench_test.go   # BenchmarkPost: throughput + p99 latency (numbers → README)
│   ├── ledger_test.go         # validation, idempotency, concurrency tests
│   ├── ledger_idempotency_ttl_test.go# TTL eviction, lazy prune, interval guard
│   ├── ledger_snapshot_test.go# versioned snapshot semantics
│   └── ledger_list_test.go    # cursor pagination, time windows, interleaved inserts
├── main.go                    # net/http JSON API (thin assembly only)
├── metrics.go                 # Prometheus-format /metrics counters (stdlib only)
├── metrics_test.go            # /metrics exposition + counter semantics tests
├── main_test.go               # HTTP handler tests (httptest)
├── main_reconcile_test.go     # POST /reconcile handler tests (httptest)
├── main_graceful_test.go      # SIGTERM drain: in-flight requests complete, listener closes
└── .github/workflows/ci.yml
```

## License

MIT
