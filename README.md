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

#### Transfer fees

A transfer can carry a **fee leg**: the payer is charged `fee_cents` on
top of the transfer amount, booked to `fee_account` as a second journal
entry (`debit fee_account`, `credit from_account`, id
`<transfer_id>/fee`). The payee always receives the full transfer
amount; the payer's total outflow is amount + fee. Both entries commit
atomically — if the fee leg fails (frozen fee account, overdraft on the
payer's total outflow), the principal entry is not recorded either.

```bash
curl -s -X POST localhost:8080/transfers \
  -d '{"transfer_id":"tx-2026-002","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":25,"fee_account":"fees"}'
# 201 {"transfer_id":"tx-2026-002","entries":[{...},{...}],"fee_cents":25,"duplicate":false}
```

A positive `fee_cents` requires `fee_account` (and vice versa); a
negative fee is rejected with `400`.

Without an explicit fee, the server-wide fee policy applies unless
`skip_fee` is set. The policy is an **amount-tiered schedule**: a transfer
of `A` cents falls into the last tier whose minimum does not exceed `A`
and is charged `floor(A * tierRateBps / 10000)` cents to the revenue
account. The receipt discloses the applied tier as `fee_tier_index`
(0-based; `-1` when no policy tier applied — explicit fee, `skip_fee`,
or no policy) and `fee_rate_bps`.

Configure it with `LEDGER_TRANSFER_FEE` in one of two forms:

```bash
# Flat (legacy): "<rateBps>:<revenueAccount>"
LEDGER_TRANSFER_FEE="250:fee-revenue"            # flat 2.5%

# Tiered: "<minCents>:<rateBps>,...@<revenueAccount>"
LEDGER_TRANSFER_FEE="0:0,10000:250,1000000:100@fee-revenue"
#   fee-free dust below $100, 2.5% from $100, 1% from $10k
#   (the cheap top band caps the fee curve on large transfers)
```

Each tier rounds down with overflow-safe integer arithmetic (no
intermediate product can overflow `int64` for any amount at rates up to
10000 bps); a computed fee of zero (a free dust tier, or a dust amount
under a low rate) posts no leg, while the receipt still discloses the
tier that produced it. An explicit fee always wins over the policy. A
malformed value fails the startup fast (`log.Fatal`) instead of
silently mispricing transfers. Total fee cents booked are exposed as
`ledger_transfer_fee_cents_total`.

### `POST /sweeps`

Treasury sweep: atomically moves the positive per-currency balances of the
source accounts into the target account — one journal entry per
`(source, currency)`, all under the sweep ID. The classic shape is a
merchant sweeping its sub-merchants' settlement balances (see
`GET /accounts/{id}/rollup` for the subtree) into the master account at
the end of the day.

```bash
curl -X POST localhost:8080/sweeps -d '{
  "sweep_id": "sweep-2026-10-08",
  "from_accounts": ["sub-merchant-1", "sub-merchant-2"],
  "to_account": "treasury",
  "idempotency_key": "sweep-day-2026-10-08"
}'
# 201 {"sweep_id":"sweep-2026-10-08",
#      "legs":[{"from_account":"sub-merchant-1","currency":"USD","amount_cents":10000,
#               "entry_id":"sweep-2026-10-08/sub-merchant-1/USD"}, ...],
#      "entries":[{...}],"duplicate":false}
```

Each leg reuses transfer semantics (debit target, credit source) but a
sweep **never charges a fee** — sweeps are internal treasury movements,
so the `LEDGER_TRANSFER_FEE` policy does not apply. Sweep legs are marked
by their entry-ID prefix `<sweep ID>/<source>/<currency>`, so they are
directly visible in journal exports and the audit chain.

Only positive balances move: zero balances are skipped, and negative
balances are skipped too (a negative balance means the account owes money;
sweeping it would move debt into the treasury account). A sweep whose
sources all have non-positive balances succeeds with an empty receipt.
Overdraft protection needs no check — each leg moves its source's full
positive balance, so every source lands at exactly zero. Holds are
advisory reservations, not journaled money: a sweep moves the full journal
balance, so settle or release holds before sweeping.

Validation mirrors `POST /transfers`: a first-time sweep returns `201`
with the receipt; a duplicate idempotency key returns `200` with the
original receipt (its own key namespace, expiring with the TTL); invalid
sweeps return `400`; a sweep touching a frozen account returns `403`.

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

### Authorization holds (auth/capture)

Fintech pre-authorization flow: `POST /holds` reserves funds on an
account until a time-bound expiry *without* moving any money through the
journal — the card-network auth/capture pattern. While a hold is active,
the account's **available** balance is reduced by the held amount
(`available = balance − held`); the net balance, debit/credit totals,
and the audit chain are untouched. `POST /holds/{id}/capture` then
settles up to the held amount as an ordinary double-entry posting from
the held account to a payee (in the hold's currency — captures never
cross currencies), consuming the hold; the un-captured remainder is
released back to available automatically. `POST /holds/{id}/release`
drops a hold without settling. `POST /holds/expire` is the
operator-facing sweep that marks lapsed holds expired — expiry itself is
lazy (an expired hold already counts as inactive for available-balance
purposes before the sweep). A background worker can run the same sweep on
a timer — see `LEDGER_HOLD_SWEEP_INTERVAL` under [Environment](#running) —
so operators don't need to poll the endpoint; every worker tick is counted
by the same `ledger_hold_sweeps_total` counter.

Holds are off-journal by design: they create no journal rows, no
audit-chain links, and no ledger-version bumps (only the capture, which
journals a real posting, bumps the version). A hold is rejected with
`422` when the account's available funds cannot cover it — for
overdraft-protected accounts this check is also the overdraft guard,
since their balance floor is zero. Frozen accounts cannot place holds
or be capture legs (`403`); release still works on a frozen account
because it frees funds rather than moving them. Captures run the same
frozen/overdraft checks as ordinary postings: the reservation is
advisory, not an escrow, so a capture that would overdraw a protected
account whose balance moved after the hold is rejected with `422`.

Lifecycle: `active → captured | released | expired`. Captures are
single-shot (one capture consumes the hold); release is idempotent. Hold
and capture idempotency keys live in their own per-operation namespaces,
independent of `POST /entries` and `POST /transfers` keys, and honor the
same `LEDGER_IDEMPOTENCY_TTL`.

```bash
curl -s -X POST localhost:8080/holds \
  -d '{"account":"card-42","amount_cents":5000,"expires_at":"2026-10-08T12:00:00Z"}'
# 201 {"id":"...","account":"card-42","amount_cents":5000,"currency":"USD",
#      "expires_at":"...","status":"active",...}
curl -s localhost:8080/accounts/card-42/balance
# {"account":"card-42","balance_cents":10000,"available_cents":5000,"frozen":false}
curl -s -X POST localhost:8080/holds/<id>/capture \
  -d '{"to_account":"merchant-7","amount_cents":3200}'
# 201 {"capture_id":"...","hold_id":"...","captured_cents":3200,"released_cents":1800,...}
curl -s -X POST localhost:8080/holds/<id>/release
# 200 {"id":"...","status":"released",...}
curl -s -X POST localhost:8080/holds/expire
# 200 {"expired":3}
```

Status codes: `201` first booking / `200` idempotent replay or release;
`400` malformed request; `403` frozen account; `404` unknown hold;
`422` insufficient available funds, capture exceeding the hold, or
capture on a non-active/expired hold.

### Multi-currency

Every entry carries a `currency`: a three-letter uppercase ISO 4217 code
(`USD`, `EUR`, `CNY`, …). The field is optional on input — omitting it
books the entry in the default currency, `USD` — and anything else must
be exactly three ASCII uppercase letters, otherwise the post is rejected
with `400`. The ledger normalizes the code before committing, so the
journal, the idempotency index, and the tamper-evident audit-chain hash
all store the canonical code.

```bash
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"cash","credit_account":"equity","amount_cents":2000,"currency":"EUR"}'
# 201 {"id":"...","debit_account":"cash","credit_account":"equity","amount_cents":2000,"currency":"EUR",...}
```

Balances, debit totals, and credit totals are tracked per **(account,
currency)** pair, so one account can hold USD, EUR, and CNY side by side
without the currencies ever mixing. The accounting equation is verified
**per currency, never across them** — adding USD cents to EUR cents would
be meaningless, so each currency's books must balance on their own.
`GET /accounts/{id}/trial-balance` reports the default-currency view on
top and a `by_currency` breakdown of every currency the account holds;
`POST /reconcile` adds a `currency_totals` rollup with per-currency
debit/credit sums.

`POST /transfers` accepts the same `currency` field and books **every
leg — principal and fee — in that one currency**. This ledger performs
no FX conversion, so a transfer spanning currencies is rejected with
`422` (`ledger: cross-currency transfers are not supported`); both
currency rejections (malformed code `400`, cross-currency `422`) are
counted in `ledger_currency_rejections_total`.

Two read-path notes: `GET /accounts/{id}/balance` and
`GET /accounts/{id}/snapshot` keep their historical meaning — the
**default-currency** balance — so existing consumers are unaffected; use
the trial balance's `by_currency` rows (or `BalanceIn` in the Go API) for
a specific currency. Overdraft protection is likewise per-currency: a
protected payer's EUR balance cannot cover a USD outflow.

### `GET /accounts/{id}/balance`

```bash
curl -s localhost:8080/accounts/cash/balance
# {"account":"cash","balance_cents":1000,"available_cents":1000,"frozen":false}
```

`balance_cents` is the journaled net; `available_cents` is the spendable
amount — net minus active authorization holds (see Authorization holds
above). With no holds outstanding the two are equal.

### `GET /accounts/{id}/snapshot`

Balance plus the ledger version at read time. The version bumps on every
successful post (idempotent replays and rejected entries don't count), so
two snapshots with the same version are guaranteed to show the same
balance — a cheap change detector for reconciliation jobs.

```bash
curl -s localhost:8080/accounts/cash/snapshot
# {"account":"cash","balance_cents":1000,"version":3,"frozen":false}
```

### `GET /accounts/{id}/balance-at?version=N[&currency=XXX]`

Time-travel balance query: the account's net balance as of ledger version
N — the balance after exactly N successful posts. The version `/snapshot`
reports can be fed back here to reproduce the balance that was current
then, the primitive for audit replay and point-in-time reconciliation
("what did this account hold when the incident happened"). Version 0 is
the genesis (zero for every account); a version beyond the current one is
`422` (`ErrVersionInFuture` — the future has no balance yet). `currency`
is optional and defaults to USD; unknown accounts report zero, like
`/balance` and `/snapshot`. The scan folds the audit-chain prefix under a
single read lock, so the result is a consistent point-in-time view;
O(version) per query, and the append-only journal means a cached answer
never goes stale.

```bash
curl -s "localhost:8080/accounts/cash/balance-at?version=2"
# {"account":"cash","currency":"USD","version":2,"balance_cents":200}
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
`total_credits_cents`; unknown accounts report zeros. The top-level totals
are the default-currency (USD) view; `by_currency` breaks the account
down per currency it holds.

```bash
curl -s localhost:8080/accounts/cash/trial-balance
# {"account":"cash","currency":"USD","total_debits_cents":1500,"total_credits_cents":500,"net_balance_cents":1000,
#  "by_currency":[{"currency":"EUR","total_debits_cents":2000,"total_credits_cents":0,"net_balance_cents":2000},
#                 {"currency":"USD","total_debits_cents":1500,"total_credits_cents":500,"net_balance_cents":1000}],
#  "version":3,"frozen":false}
```

### `POST /accounts/{id}/parent` and `GET /accounts/{id}/rollup`

The **sub-account hierarchy**: link accounts into a parent/child tree —
the classic shape is a merchant account with sub-merchant children, each
of which may have its own children — and roll up balances over the whole
subtree. Linking is structural, not bookkeeping: like freeze/unfreeze, it
does not bump the ledger version.

```bash
curl -s -X POST localhost:8080/accounts/sub-1/parent \
  -H 'Content-Type: application/json' -d '{"parent":"merchant"}'
# 200 {"account":"sub-1","parent":"merchant"}

curl -s localhost:8080/accounts/merchant/rollup
# {"account":"merchant","descendant_count":2,
#  "accounts":["merchant","sub-1","sub-2"],
#  "by_currency":[{"currency":"EUR","balance_cents":300},
#                 {"currency":"USD","balance_cents":1200}],
#  "version":5,"frozen":false}
```

- `POST /accounts/{id}/parent` with `{"parent":"..."}` assigns the
  parent; `{"parent":""}` clears the link. Accounts need no prior
  registration — a sub-merchant can be linked before its first posting.
- An account assigned as its own parent → `400`; an assignment that would
  close a cycle (the child is already an ancestor of the proposed parent)
  → `422`. The hierarchy stays a forest, so rollups always terminate.
- `GET /accounts/{id}/rollup` returns the account's own balances plus every
  descendant's, aggregated **per currency** (currencies are never summed
  together, mirroring the accounting equation). Unknown accounts roll up
  to just themselves with zero balances.

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
| `overdraft_protected_accounts` | accounts currently guarded against overdrafts |
| `held_totals`          | per-currency rollup of active authorization holds (`currency`, `held_cents`, `active_holds`), sorted by currency — the cents currently reserved from available balances; expired holds count as inactive |
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

### Disaster-recovery snapshots (`ledger/snapshot.go`)

The ledger can be exported to a JSONL disaster-recovery snapshot and
rebuilt from it. The export is a library-level API (no HTTP endpoint —
backups are an operator concern, taken from the process embedding the
ledger):

```go
var buf bytes.Buffer
if err := l.ExportSnapshot(&buf); err != nil { /* ... */ }
// buf now holds one JSON object per line:
//   {"record":"meta",...}                 exactly one, always first
//   {"record":"entry",...}                journal entries, in Post (chain) order
//   {"record":"link",...}                 audit-chain links, seq 1..N
//   {"record":"hold",...}                 authorization holds
//   {"record":"idempotency","namespace":"entry|transfer|hold|capture|sweep",...}
//   {"record":"config",...}               frozen/overdraft/hierarchy/fee-policy/TTL
```

The export holds the read lock for its whole duration (one consistent
point in time) and is byte-deterministic for identical state, so
`sha256sum` fingerprints work for backup verification.

`ImportSnapshot` rebuilds a fully working ledger from a snapshot:
balances, totals, and indexes are refolded from the journal in chain
order, holds and all five idempotency-key namespaces are restored with
referential checks, and operational config (frozen accounts, overdraft
guards, sub-account hierarchy, transfer fee policy, idempotency TTL) comes
back intact. The import finishes with a full audit-chain verification —
a rewritten amount, spliced link, reordered journal, or dangling registry
reference rejects the whole import (`ErrSnapshotInvalid`); no
half-restored ledger is ever returned.

The operator's pre-promotion check is to rerun reconciliation against the
restored copy:

```go
restored, err := ImportSnapshot(bytes.NewReader(snapshotBytes))
if err != nil {
    log.Fatalf("backup failed verification: %v", err) // do NOT promote
}
report := restored.Reconcile(time.Now())
// report.AccountingEquationOK && report.AuditChain.VerifyOK => safe to promote
```

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
# ledger_transfer_fee_cents_total 310
```

- `ledger_posts_total` — every `POST /entries` request received.
- `ledger_transfers_total` — every `POST /transfers` request received.
- `ledger_idempotency_hits_total` — posts that replayed an existing
  idempotency key (returned the original entry, booked nothing).
- `ledger_transfer_idempotency_hits_total` — transfers that replayed an
  existing idempotency key (returned the original receipt, booked
  nothing).
- `ledger_transfer_fee_cents_total` — total fee cents booked by transfer
  fee legs.
- `ledger_sweeps_total` — every `POST /sweeps` request received.
- `ledger_sweep_idempotency_hits_total` — sweeps that replayed an existing
  idempotency key (returned the original receipt, booked nothing).
- `ledger_balance_queries_total` — `GET /accounts/{id}/balance` requests
  served. Snapshot reads are not counted.
- `ledger_balance_at_queries_total` — `GET /accounts/{id}/balance-at`
  requests served.
- `ledger_verify_requests_total` — `GET /entries/verify` requests served.
- `ledger_reconcile_runs_total` — `POST /reconcile` requests served.
- `ledger_frozen_rejections_total` — `POST /entries`, `POST /transfers`,
  `POST /sweeps`, and hold requests rejected with `403` because an account
  was frozen.
- `ledger_overdraft_rejections_total` — `POST /entries`, `POST
  /transfers`, and capture requests rejected with `422` because the
  posting would have overdrawn an overdraft-protected account.
- `ledger_holds_total` — every `POST /holds` request received.
- `ledger_hold_idempotency_hits_total` — holds that replayed an existing
  idempotency key (returned the original hold, reserved nothing).
- `ledger_hold_rejections_total` — hold-domain requests rejected with
  `422`: insufficient available funds, a capture exceeding the held
  amount, or a capture on a non-active or expired hold.
- `ledger_captures_total` — every `POST /holds/{id}/capture` request
  received.
- `ledger_capture_idempotency_hits_total` — captures that replayed an
  existing idempotency key (returned the original receipt, booked
  nothing).
- `ledger_releases_total` — `POST /holds/{id}/release` requests received.
- `ledger_hold_sweeps_total` — hold-expiry sweeps: `POST /holds/expire`
  requests received plus background hold-sweeper ticks (see
  `LEDGER_HOLD_SWEEP_INTERVAL` below).

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
- `LEDGER_HOLD_SWEEP_INTERVAL` — enables the background hold-expiry
  worker, e.g. `LEDGER_HOLD_SWEEP_INTERVAL=30s`. While enabled, the server
  sweeps lapsed holds on the interval (the same `ExpireHolds` sweep
  `POST /holds/expire` runs, off-journal, version untouched) and counts
  every tick in `ledger_hold_sweeps_total` alongside operator requests.
  The first tick fires after one full interval; SIGINT/SIGTERM stops the
  worker with the server. Unset or invalid means disabled — expiry stays
  lazy by predicate and available on demand via `POST /holds/expire`.
- `LEDGER_TRANSFER_FEE` — default transfer fee policy. Flat form
  `"<rateBps>:<revenueAccount>"` (e.g. `LEDGER_TRANSFER_FEE="250:fee-revenue"`
  for 2.5%), or tiered form `"<minCents>:<rateBps>,...@<revenueAccount>"`
  (e.g. `"0:0,10000:250,1000000:100@fee-revenue"`). Applies to
  `POST /transfers` without an explicit fee unless the request sets
  `skip_fee`. Unset means no default fee; an invalid value fails startup
  fast.

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
│   ├── hierarchy.go           # sub-account parent links (cycle-guarded) + per-currency balance rollup
│   ├── ledger_hierarchy_test.go# parent assignment, cycle rejection, rollup aggregation, concurrency
│   ├── ledger_idempotency_ttl_test.go# TTL eviction, lazy prune, interval guard
│   ├── ledger_snapshot_test.go# versioned snapshot semantics
│   ├── snapshot.go            # disaster-recovery snapshots: JSONL export / verified import (entries + chain + holds + idempotency registries + config)
│   ├── ledger_dr_test.go      # snapshot round-trip, tamper/splice rejection, reconcile-rerun parity
│   ├── timetravel.go          # BalanceAt: point-in-time balance at a ledger version (audit-chain prefix scan)
│   ├── ledger_timetravel_test.go# time-travel correctness, currency isolation, future-version rejection, concurrent readers
│   └── ledger_list_test.go    # cursor pagination, time windows, interleaved inserts
├── main.go                    # net/http JSON API (thin assembly only)
├── metrics.go                 # Prometheus-format /metrics counters (stdlib only)
├── metrics_test.go            # /metrics exposition + counter semantics tests
├── main_test.go               # HTTP handler tests (httptest)
├── main_timetravel_test.go    # GET /accounts/{id}/balance-at handler tests (httptest)
├── main_reconcile_test.go     # POST /reconcile handler tests (httptest)
├── main_graceful_test.go      # SIGTERM drain: in-flight requests complete, listener closes
└── .github/workflows/ci.yml
```

## License

MIT
