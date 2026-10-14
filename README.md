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
- Snapshots commit to the balance sheet too: every export builds a deterministic Merkle tree over `(account, currency) -> balance` leaves (`SHA-256`, standard library) and writes the root into the snapshot header; `GET /accounts/{id}/merkle-proof` serves offline-verifiable inclusion proofs, the backup worker anchors the root with the snapshot version in the sealed audit trail, and `ImportSnapshot` rejects a header whose root does not recompute from the refolded balances.
- A thin `net/http` JSON API exposes the ledger: no external dependencies.
- Opt-in journal rules (`PUT /rules/{id}`): match raw postings on account/currency/amount range and auto-append derived journal legs (tax withholding, platform fees). Rules fire only after the principal clears the full `Post` check order; each derived leg re-runs the frozen/overdraft/daily-limit/period gates on the aggregate outflow, the commit is atomic, illegal rules are rejected at construction, rule changes land in the structured audit log, and the rule set round-trips through snapshots.

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
- Either leg of the entry names an account on the **sanctions/denylist**
  (exact account ID or prefix pattern, see `POST /denylist`) → `422`
  `{"error":"ledger: account is on the sanctions/denylist: ..."}`. Screening
  runs before the idempotency replay check — a denied request leaves no
  idempotency trace — and the attempt is recorded as a `denylist_rejected`
  audit event. The rejection is counted in
  `ledger_denylist_rejections_total`.
- The credit (payer) account is **overdraft-protected** and the posting would
  take its balance below zero → `422`
  `{"error":"ledger: posting would overdraw a protected account"}`. The
  rejection is counted in `ledger_overdraft_rejections_total`.
- The credit (payer) account has a **daily outflow limit** and the posting
  would take its UTC-day cumulative outflow above the limit → `422`
  `{"error":"ledger: posting would exceed the account's daily outflow limit"}`.
  The rejection is counted in `ledger_daily_limit_rejections_total`.
- `memo`: optional free-form note, at most 255 UTF-8 characters (an order
  ID, an invoice reference, a reconciliation tag). It is journaled with
  the entry, covered by the audit-chain hash, queryable via
  `GET /entries?memo=`, and echoed by dry-run legs. Overlong memos → `400`.
- `external_ref`: optional external business reference, at most 64 UTF-8
  characters (a payment provider's transaction ID, an external order
  number). Unlike the memo it is a **uniqueness domain**: no two journal
  rows in the ledger may carry the same non-empty reference, so downstream
  systems can correlate journal rows with external records unambiguously.
  It is journaled with the entry, covered by the audit-chain hash,
  queryable via `GET /entries?ref=` (exact match), and rejected with `400`
  when overlong or duplicated. Independent of the idempotency-key
  namespace: an idempotent replay returns the original entry without
  consulting the reference index.
- `attachments`: optional evidence references for the entry (a receipt
  scan, an invoice PDF, a compliance document) — at most 8 `https` URIs,
  each at most 512 UTF-8 characters. They are journaled with the entry,
  covered by the audit-chain hash (rewriting a journaled attachment breaks
  the chain exactly like rewriting an amount), carried through snapshots
  and idempotent replays, and echoed by `GET /entries`. `POST /transfers`
  stamps them on the principal entry (the fee leg carries none);
  `POST /entries/{id}/reverse` inherits the original entry's attachments;
  `POST /holds/{id}/capture` stamps the capture's attachments on the
  settlement entry. Anything else — more than 8, a non-`https` URI, or an
  overlong URI — is rejected with `400` before anything is recorded.

### `POST /entries/batch`

Atomic bulk settlement: one request commits several double-entry entries
(a payroll run, a merchant batch settlement) — either every new entry
lands or nothing does. One bad leg rejects the whole batch; the frozen /
overdraft / daily-limit risk controls apply per entry, evaluated
sequentially in batch order, so a batch cannot dodge a guard by splitting
one overdrawing outflow into small legs.

```bash
curl -s -X POST localhost:8080/entries/batch \
  -H 'Content-Type: application/json' \
  -d '{"batch_id":"payroll-2026-10","idempotency_key":"bk-001","entries":[
    {"entry_id":"p-001","debit_account":"alice","credit_account":"payroll","amount_cents":500000,"currency":"USD"},
    {"entry_id":"p-002","debit_account":"bob","credit_account":"payroll","amount_cents":450000,"currency":"USD"}
  ]}'
# 201 {"batch_id":"payroll-2026-10","entries":[...],"duplicate":false}
```

- First-time batch → `201` with the receipt. The server generates
  `batch_id` and per-entry `id` values when the client omits them.
- Re-post with the same batch `idempotency_key` → `200` with the original
  receipt (`duplicate: true`); nothing is booked again.
- Each entry's own `idempotency_key` shares the `POST /entries` key
  namespace: a key already posted resolves to the originally posted entry
  (returned in the receipt, booked nothing), while the batch's fresh
  entries commit normally.
- Invalid batch (empty batch, bad leg, duplicate entry IDs in the batch,
  an entry ID already journaled, or a duplicated new entry key) → `400`.
- A leg through a **frozen** account → `403`; a leg that would overdraw an
  **overdraft-protected** payer or breach a **daily outflow limit** →
  `422`. Rejections are counted in the same
  `ledger_frozen/overdraft/daily_limit_rejections_total` counters as
  single posts.
- Committed entries carry `batch_id`, so a bulk settlement traces back to
  its batch; each entry gets its own audit-chain link, so `GET
  /entries/verify` and `POST /reconcile` cover batch entries exactly like
  ordinary postings. Batch keys expire with the idempotency TTL like every
  other key namespace and survive disaster-recovery snapshots.
- Attempts and replays are counted in `ledger_batch_total` and
  `ledger_batch_idempotency_hits_total`.

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
  `422`, as is a transfer that would take the payer's daily outflow above
  its **daily outflow limit**. All are counted in the shared
  `ledger_frozen_rejections_total` / `ledger_overdraft_rejections_total` /
  `ledger_daily_limit_rejections_total` counters.
- `memo` (optional, ≤ 255 UTF-8 characters) is carried onto the principal
  journal entry (on cross-currency transfers, onto the `<id>/fx`
  source-currency leg); the fee leg carries no memo.
- `external_ref` (optional, ≤ 64 UTF-8 characters) follows the same leg
  as the memo — the principal entry, or the `<id>/fx` leg on
  cross-currency transfers; the fee leg carries none. A reference already
  journaled on another entry rejects the transfer with `400`.

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

#### Large-transfer dual review

Opt-in maker-checker for big money movements (the fintech dual-control
flow). With a review threshold configured, a transfer whose principal
amount reaches the threshold does **not** settle: `POST /transfers`
returns `201` with a receipt carrying `review_status: "pending_review"`,
its legs journaled with the `pending_review` marker — visible in journal
exports, excluded from balances, totals, the ledger version, and the
audit chain — and the payer's funds **frozen atomically** (deducted from
`available_cents` like an authorization hold, and reserved against the
daily outflow budget).

An operator then decides, with their identity recorded in the structured
audit log's hash chain:

```bash
curl -s -X POST localhost:8080/transfers/tx-2026-007/approve \
  -H 'Content-Type: application/json' \
  -d '{"reviewer":"op-alice"}'
# 200 {"transfer_id":"tx-2026-007",...,"review_status":"approved","duplicate":false}

curl -s -X POST localhost:8080/transfers/tx-2026-007/reject \
  -H 'Content-Type: application/json' \
  -d '{"reviewer":"op-carol","reason":"sanctions screen"}'
# 200 {"transfer_id":"tx-2026-007","status":"rejected",...}
```

- `POST /transfers/{id}/approve` settles the review: the marker flips to
  effective and the legs commit exactly like a direct transfer (balances,
  version bumps, chain links — the chain hash covers the marker, so the
  effective form is tamper-evident). Frozen/overdraft/period checks
  re-run against current state; a leg through a now-frozen account
  (`403`), an overdraft-protected payer that can no longer cover the
  outflow (`422`), or legs dated in a now-closed period (`422`) leave the
  review pending for a later retry. Approval is **idempotent**:
  re-approving returns `200` with the original receipt
  (`duplicate: true`) and settles nothing twice.
- `POST /transfers/{id}/reject` drops the review without settling: the
  freeze is released back to `available_cents`. Idempotent; rejecting an
  approved review returns `409`.
- Replaying the transfer's `idempotency_key` while the review is pending
  returns the review receipt (`duplicate: true`, `review_status:
  "pending_review"`) instead of freezing twice.
- Reviews can carry an opt-in expiry (`LEDGER_REVIEW_EXPIRY`, e.g.
  `"24h"`): `POST /transfers/reviews/expire` — or the background worker
  (`LEDGER_REVIEW_SWEEP_INTERVAL`) — auto-rejects lapsed reviews so
  frozen funds can never strand in dual-control limbo.
- `Reconcile` lists every pending review (`pending_reviews`: frozen legs,
  reserved outflow, expiry) and every per-account threshold
  (`review_thresholds`); reviews survive disaster-recovery snapshots with
  their statuses, and `GET /metrics` exposes
  `ledger_review_pending_total` / `ledger_review_approved_total` /
  `ledger_review_rejected_total`.
- Configure with `LEDGER_REVIEW_THRESHOLD` (global, cents, e.g. `"100000"`
  for $1,000.00) and/or `LEDGER_REVIEW_THRESHOLDS`
  (`"<account>:<cents>,..."`, e.g. `"treasury:1000000,ops:500000"` — a
  per-account threshold wins over the global one for that account's
  outbound transfers). Unset means no review; invalid values fail startup
  fast.

### Scheduled transfers (`POST /transfer-schedules`)

Recurring transfer plans — payroll, rent, loan servicing. `POST
/transfer-schedules` registers a plan (`schedule_id`, legs, amount,
currency, optional fee/FX fields like a transfer, `interval` as a Go
duration string such as `"24h"`, `next_run_at` RFC3339, optional
`ends_at`); the background sweeper (`LEDGER_SCHEDULE_SWEEP_INTERVAL`,
e.g. `"1m"`) fires every due plan via `Ledger.SweepDue`. Each run posts
through the full `PostTransfer` validation chain (frozen, overdraft,
daily-limit, period, fee policy) under the idempotency key
`"<schedule_id>/run/<seq>"`, so a crash between the commit and the
schedule-state advance replays instead of double-booking. A failed run
is recorded (run history + `schedule_run_failed` audit event) and keeps
its `NextRunAt` so the next sweep retries the same run — one plan's
rejection never blocks the others, and missed runs are skipped (counted
as `skipped_runs`) rather than caught up. Plans support
pause/resume/cancel (`POST /transfer-schedules/{id}/pause|resume|cancel`;
cancelled/completed are terminal), run history (`GET
/transfer-schedules/{id}/runs`, newest first, `?limit=`), and the
`Reconcile` report counts active plans (`active_transfer_schedules`).
Fired runs increment `ledger_scheduled_transfers_total`. Schedules are
structural state: they survive disaster-recovery snapshots (run history
does not).

### Scheduled reports (`Ledger.CreateReportSchedule`)

Recurring report subscriptions — compliance packs, client statements,
treasury snapshots. `Ledger.CreateReportSchedule` registers a
subscription (`id`, `kind: "reconcile"|"statement"`, kind-specific
`params`, `interval` as a Go duration string such as `"24h"`,
`next_run_at` RFC3339, optional `ends_at`, and a delivery `sink`); the
background sweeper (`LEDGER_REPORT_SWEEP_INTERVAL`, e.g. `"1h"`) fires
every due subscription via `Ledger.SweepReportsDue` (also callable
directly). Each run generates its report **read-only** — no version
bump, no journal entries, no audit-chain links — and delivers it:

- `sink: {url, secret}` — one signed HTTPS POST per sweep,
  `HMAC-SHA256(secret, body)` as `X-Ledger-Signature: sha256=<hex>`
  (the exact `audit sink` convention, so the same receiver
  infrastructure works), plus `X-Ledger-Event-Type: report_generated`,
  `X-Ledger-Report-Schedule`, and `X-Ledger-Report-Run` headers. The
  secret is mandatory with a URL: unsigned webhooks can be forged, so
  creation fails fast without one. One attempt per sweep; a failed
  delivery records the run as failed and the next sweep retries it with
  `NextRunAt` unchanged (at-least-once delivery).
- `sink: {dir}` — the report JSON is written atomically to
  `<dir>/<schedule-id>-<run-seq>.json` (temp file + rename); retries
  overwrite the same path, so dir delivery is idempotent.

Kind params: `reconcile` takes an optional `base_currency` (every
currency row folded into it, LG-31); `statement` takes `account`
(required) and an optional `[from, to)` RFC3339 window (defaults to the
current UTC month, LG-71). Missed runs are skipped (counted as
`skipped_runs`) rather than caught up, runs are recorded
(`report_generated` / `report_run_failed` audit events), and schedules
support pause/resume/cancel with the same lifecycle states as transfer
schedules. Run history is bounded to the newest 100 runs per schedule.
Report subscriptions are process-lifetime operational state: they do not
survive disaster-recovery snapshots (recreate them after a restore).

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

### `POST /merges`

Account merge: atomically moves **every** currency balance of the source
account into the target account — one journal entry per currency,
`<merge ID>/<currency>` — and freezes the source in the same commit. The
shape is a merchant entity changing hands: the old account is drained to
exactly zero in every currency and decommissioned, so it can never move
money again.

```bash
curl -X POST localhost:8080/merges -d '{
  "merge_id": "merge-2026-10-09",
  "from_account": "old-corp",
  "to_account": "new-corp",
  "idempotency_key": "merge-old-corp-2026-10-09"
}'
# 201 {"merge_id":"merge-2026-10-09","from_account":"old-corp","to_account":"new-corp",
#      "legs":[{"from_account":"old-corp","to_account":"new-corp","currency":"USD",
#               "amount_cents":10000,"debt_absorbed":false,"entry_id":"merge-2026-10-09/USD"}],
#      "entries":[{...}],"source_frozen":true,"duplicate":false}
```

Unlike a sweep, a merge also consolidates **negative** balances: a
negative source balance (the account owes money) is absorbed as debt —
the leg zeroes the source by debiting it and crediting the target, so the
target's balance decreases. Debt legs are flagged `"debt_absorbed":true`
in the receipt so readers never mistake them for ordinary credits. When
the target is overdraft-protected, a merge that would take it below zero
in any currency — only possible via debt absorption, since positive
balances only ever grow the target — is rejected with `422`.

Check order is validation, then the idempotency replay check, then the
merge-ID conflict check, then the frozen check on **both** accounts, then
the overdraft check on the target. A duplicate idempotency key returns
`200` with the original receipt even though the source is now frozen
(the replay books nothing new). A merge from an already-frozen source
returns `403`. A merge whose source held no balances still succeeds and
still freezes the source — decommissioning an empty account is the point —
with an empty leg list. Holds are advisory (as with sweeps): settle or
release them before merging; daily outflow limits do not apply to merges.

`POST /reconcile` reports every committed merge in its `merges` list, and
the frozen source shows up in `frozen_accounts` for cross-checking.

### `POST /entries/{id}/reverse` (reversal / 冲正)

Reverses a posted journal entry: appends a new entry with the debit and
credit legs swapped (`<entry ID>/reversal`), netting the original's
balance effects to zero. The original entry is never modified — the audit
chain stays append-only, so both the mistake and its correction remain
visible forever. The shape is a payment-operations correction: a
fat-fingered amount, a duplicate posting found after the fact, a transfer
sent to the wrong account.

```bash
curl -X POST localhost:8080/entries/9f2c.../reverse -d '{}'
# 201 {"id":"9f2c.../reversal","debit_account":"bank","credit_account":"alice",
#      "amount_cents":1000,"currency":"USD","memo":"reversal_of:9f2c...","duplicate":false}
```

The reversal ID is deterministic, so the operation is idempotent:
repeating the request (no key, or the same `idempotency_key`) returns
`200` with the existing reversal instead of booking again — even if an
involved account was frozen afterwards, because the replay books nothing
new. A *new* idempotency key on an already-reversed entry returns `409`
(the deterministic ID cannot express a second reversal of the same
entry). Reversals run the full `POST` risk gate order — frozen `403`,
overdraft `422`, daily-limit `422`, closed period `422` — and never
trigger journal rules or enter dual-control review. Reversing a transfer
reverses one journal entry at a time: reverse the principal entry and its
`<transfer ID>/fee` leg individually, each by its journal entry ID. A
pending-review entry cannot be reversed (`409` — approve or reject the
review instead). The reversal entry's memo is `reversal_of:<entry ID>`,
and a reversed entry that carried an `external_ref` passes it on as
`external_ref:"reversal_of:<original ref>"` (no reference when the
original had none; if the prefixed value would exceed 64 UTF-8
characters the reversal is rejected with `400` — references are never
truncated). Reversals are searchable via `GET /entries?memo=reversal_of:`
and counted by `ledger_reversals_total`.

### What-if dry-run (`POST /entries/dry-run`, `POST /transfers/dry-run`, `POST /sweeps/dry-run`)

Answers "what would happen if I submitted this now?" without changing
anything. The request body is decoded exactly like the real endpoint, and
the ledger runs the **full** check sequence — field validation, idempotency
replay, frozen, overdraft, period, and daily-limit checks, plus fee
resolution and FX conversion for transfers, leg planning for sweeps —
against a private deep copy of its state, then discards the copy. Because
the simulation executes the same code path as the real call, the result
cannot drift from it.

A dry run that would succeed returns `200` with the balance and version
changes the real call would cause:

```bash
curl -X POST localhost:8080/entries/dry-run -d '{
  "debit_account": "cash", "credit_account": "equity", "amount_cents": 500
}'
# 200 {"would_succeed":true,"duplicate":false,
#      "version_before":0,"version_after":1,
#      "legs":[{"entry_id":"...","debit_account":"cash","credit_account":"equity",
#               "amount_cents":500,"currency":"USD",
#               "debit_balance_before_cents":0,"debit_balance_after_cents":500,
#               "credit_balance_before_cents":0,"credit_balance_after_cents":-500}]}
```

A dry run that would fail returns **the same status code the real call
would return** — `400` for bad fields, `403` for frozen accounts, `422`
for overdrafts, closed periods, and daily-limit breaches — so a client can
swap the `/dry-run` suffix for the real endpoint and keep its error
handling unchanged. Zero side effects either way: no journal row, no chain
link, no version bump, no idempotency-key registration, no audit event.
Idempotent replays are reported honestly (`"duplicate":true`, zero balance
movement), exactly what the real call would return.

The result is advisory: it describes the ledger at the instant the
simulation ran. A concurrent writer can change the outcome between the dry
run and the real submission, so the real call must still handle
rejections. Dry-run requests are counted in `ledger_dry_runs_total`.

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

A freeze can be *timed*: pass `{"until": "<RFC3339>"}` to schedule the
freeze to lift automatically at that time (`ledger.FreezeUntil`).
A zero/absent `until` is a manual freeze, exactly as before; a non-future
timestamp is rejected with `400`. When the deadline passes, the freeze is
released either lazily — every write path (`POST /entries`, transfers,
batches, reversals, holds) checks for expired timed freezes before its
frozen gate — or by the background freeze sweeper
(`LEDGER_FREEZE_SWEEP_INTERVAL`, Go duration string; unset/disabled by
default), or an explicit `ExpireFrozenAccounts` call. Every automatic
release is recorded as a `frozen_auto_released` structured audit event
and counted by `ledger_freeze_auto_releases_total` on `GET /metrics`.
A manual `Freeze` clears any scheduled deadline, so a manual freeze never
auto-releases; timed-freeze deadlines round-trip through snapshots.

```bash
curl -s -X POST localhost:8080/accounts/cash/freeze \
  -H 'Content-Type: application/json' -d '{"until":"2026-10-12T08:00:00Z"}'
# {"account":"cash","frozen":true,"auto_unfreeze_at":"2026-10-12T08:00:00Z"}
```

**Operator attribution (`x-ledger-actor`).** Every operator action —
freeze/unfreeze, transfer-review approve/reject, account close, period
close/reopen — records *who* invoked it in the audit event's `actor`
field. Send the operator identity as the `x-ledger-actor` request header;
it defaults to `"system"` when absent (overlong values or control
characters are rejected with `400`). In-process ledger callers use the
`Ctx` variants (`FreezeCtx`, `ApproveTransferReviewCtx`,
`CloseAccountCtx`, …) or `ledger.ContextWithActor`; plain calls keep the
entrypoint name as before. The actor is sealed into the audit hash chain
like every other field, so forging or editing it breaks chain
verification — but it is pure metadata: it never enters balance math,
version bumps, journal entries, or snapshots.

The reconcile report lists frozen accounts (`frozen_accounts`), annotates
timed freezes with their deadline (`frozen_account_auto_unfreeze_at`),
and each trial balance carries a `frozen` flag, so the day-end job shows
which accounts were stopped.

### `POST /accounts/{id}/close` — account lifecycle close

The terminal lifecycle state of an account (Go: `ledger.CloseAccount`,
`ledger.IsClosed`). Where the freeze above is a *reversible* risk-control
stop, closing is **irreversible**: there is no unclose. A closed account
rejects every write path that would book a new journal entry through it —
`POST /entries`, batches, transfers (principal *and* fee legs), FX
transfers, reversals, sweeps, merges, holds and hold captures, closing
entries, revaluation entries, and journal-rule derived legs — with
`403` (`ledger.ErrAccountClosed`, counted separately as
`ledger_closed_rejections_total`). Reads are unaffected by design:
balance, snapshot, trial-balance, per-account entries, chain verification,
and reconcile keep working, so risk and reconciliation tooling can keep
watching a closed account forever.

```bash
curl -s -X POST localhost:8080/accounts/cash/close
# {"account":"cash","closed":true,"already_closed":false,"closed_at":"2026-10-12T05:56:05Z","cascade":false}
```

Closing is idempotent: re-closing returns the original closure record
(`already_closed: true`) instead of an error, mirroring the idempotency
replay contract — an operator retry after a crash never creates a second
record. The idempotency-key replay check also runs *before* the closed
gate, so a key posted before the closure still replays to its original
entry (the replay books nothing new). Closing never bumps the ledger
version (balances are unchanged) and never touches the journal or the
audit chain; it is recorded as an `account_close` structured audit event
plus one `account_closed` outbox event per closed account, and closures
round-trip through snapshots (a disaster-recovery restore must not
silently reopen closed accounts). The trial balance carries a `closed`
flag and the reconcile report lists `closed_accounts`.

With `{"cascade": true}`, the close propagates to the whole sub-account
subtree (`POST /accounts/{id}/parent` hierarchy) in the same call — the
merchant-offboarding path; each member gets its own closure record with
the same `closed_at`.

### Period close (`POST /periods/{id}/close` / `POST /periods/{id}/reopen`)

Accounting-period lock for month-end close (fintech compliance). A period
is a UTC calendar month (`"YYYY-MM"`); closing it locks the books for that
month: every journal-writing operation — `POST /entries`,
`POST /entries/batch`, `POST /transfers`, `POST /sweeps`, `POST /merges`,
hold capture — rejects an entry whose timestamp falls in the closed month
with `422` (`ErrPeriodClosed`). A posting dated in any open month keeps
working, and reads (balances, snapshots, trial balances, the audit chain,
reconcile) are unaffected — a closed month is final, not invisible.

Closing is idempotent, and reopening a never-closed period is a no-op.
Neither bumps the ledger version (no money moves), but both are recorded
in the structured audit log's hash chain (`period_close` / `period_reopen`
events) so the lock action itself is tamper-evident. Replays are exempt:
an idempotency key posted before the close still replays to its original
entry after the close, because the replay books nothing new — the same
replay-first ordering as frozen accounts. The close applies to the entry's
timestamp, not the account, so backdated postings into a closed month are
rejected while current-month traffic continues.

```bash
curl -s -X POST localhost:8080/periods/2025-05/close
# {"period":"2025-05","closed":true}
# ... POST /entries with a May timestamp now returns 422 ...
curl -s -X POST localhost:8080/periods/2025-05/reopen
# {"period":"2025-05","closed":false}
```

The reconcile report lists the locked periods (`closed_periods`), and
period locks survive disaster-recovery snapshots like the frozen-account
list — a restore never silently reopens a closed month.

#### Closing entries (`POST /periods/{id}/close?closing=true`)

The standard period-end close: with `?closing=true`, closing the period
first books closing entries that zero the period's profit-and-loss
accounts into a retained-earnings account, then locks the month. The P&L
set and the retained-earnings account are opt-in structural config (the
chart-of-accounts types are a separate, optional control — see "Chart of
accounts" below):

```bash
curl -s -X PUT localhost:8080/closing-accounts \
  -d '{"pl_accounts":["revenue","expenses"],"retained_earnings":"retained"}'
curl -s -X POST 'localhost:8080/periods/2026-09/close?closing=true'
# {"period":"2026-09","closed":true,"closing":true,"entries":[...]}
```

Each (P&L account, currency) with a non-zero balance books one entry —
debit-normal balances (expenses) post `{debit: retained, credit:
account}`, credit-normal balances (revenue) post `{debit: account,
credit: retained}` — dated at the next month's first instant
(`2026-10-01T00:00:00Z`), so the locked month's books stay final and the
new month opens with zeroed P&L accounts. Entry IDs are deterministic
(`2026-09/closing/revenue/USD`) and each carries the memo
`closing:2026-09`, so closes are idempotent and closing entries are
queryable via `GET /entries?memo=closing:2026-09`.

Closing entries are ordinary journal entries: they append audit-chain
links, bump the version, and are covered by `VerifyAccountingEquation`
and the reconcile report like any other posting. Two deliberate
differences from `POST`: they do not fire the opt-in journal rules
engine (derived business legs must not attach to a mechanical close),
and they are never routed into dual-control review. Every entry clears
the normal gate order (frozen → overdraft → daily limit → next-month
period gate); the first failing gate aborts the whole close and the
period **stays open** — never a silent half-close. Closing without
configured accounts fails `400` (`ErrClosingNotConfigured`); re-closing
an already-closed period is a no-op. The mapping survives
disaster-recovery snapshots, and the close is recorded in the audit
chain (`period_close` with closing details).

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

### Chart of accounts (Go API: `RegisterAccountType`)

Opt-in five-category classification — `asset`, `liability`, `equity`,
`revenue`, `expense` — with posting-direction enforcement on
`POST /entries`, `POST /entries/batch`, and their dry-run twins. The
debit leg must name an asset or expense account (a debit increases them)
and the credit leg a liability, equity, or revenue account (a credit
increases them); a reverse-direction posting is rejected with `422`
(`ErrAccountDirectionMismatch`) and books nothing — no journal row, no
chain link, no version bump.

```go
l.RegisterAccountType("cash", ledger.AccountTypeAsset)
l.RegisterAccountType("loan", ledger.AccountTypeLiability)
// debit loan / credit cash now fails: debiting a liability is the
// reverse direction.
```

Unregistered accounts skip the check entirely, so a ledger that never
registers anything posts exactly as before. Contra and clearing accounts
that legitimately book both directions — a payable credited by accruals
and debited by their reversals, an FX clearing account, suspense —
should stay unregistered or use dedicated contra accounts: this is the
strict statement-account control accounting software applies, not a
blanket double-entry rule.

Ordering: the direction check runs after the idempotency replay check
and before the frozen check — a key posted before the account was
registered still replays to its original entry, because the replay books
nothing new. Registrations are structural config like the frozen-account
list: they don't bump the journal version and they survive
disaster-recovery snapshots (a tampered type fails the import instead of
installing a classification that could never have been created). Each
trial balance carries an `account_type` field, the reconcile report
aggregates per-category books in `account_type_totals` (sorted by type
and currency — total assets, total liabilities, and so on), and
`GET /metrics` exposes `ledger_chart_direction_rejections_total`.

### Accruals (Go API: `AccrueInterest` / `AccrueFee` / `AccruedTotal` / `ReverseAccrual`)

Systematic adjusting entries for interest and fees (fintech month-end
accruals). One entry per (kind, expense account, payable account, UTC
calendar day): debit the expense account, credit the payable account.
Entry IDs are deterministic —
`accrual/<kind>/<expense>/<payable>/<YYYY-MM-DD>` — so re-running a day's
accrual is idempotent: the second call returns the original entry and
books nothing new.

```go
// Daily interest at 5%/yr on a $10,000 principal: floor(1000000*500/10000/365).
l.AccrueInterest("interest-expense", "interest-payable", 1000000, 500, asof, capCents)
// A fixed fee accrual of exactly 2500 cents.
l.AccrueFee("fee-expense", "fee-payable", 2500, asof, capCents)
// Cumulative net accrual for the triple, over a time window.
l.AccruedTotal("fee-expense", "fee-payable", ledger.AccrualKindFee, since, until)
// Reverse one accrual: books the mirror entry under "<accrual ID>/reversal".
l.ReverseAccrual(accrualID, asof)
```

`capCents` caps the cumulative net accrual: a run that would push the
total over the cap is rejected with `ErrAccrualCapExceeded` and books
nothing (zero disables capping; the cap check and the post run under the
same write lock, so concurrent accruals cannot both slip past it).
`ReverseAccrual` admits exactly one reversal per accrual — reversing
twice returns the existing reversal — and `AccruedTotal` is net of
reversals.

Accruals are ordinary journal entries — own audit-chain link, version
bump, covered by `VerifyAccountingEquation` and `Reconcile` — with two
deliberate differences, mirroring the closing-entries design: they never
fire the journal rules engine (system adjustments must not cascade
operator-configured rules) and never enter dual-control review. They
otherwise run the full `Post` gate order (validation, idempotency
replay, chart direction, frozen, overdraft, daily-limit, period). The
memo `accrual:<kind>` makes accrual rows discoverable via
`GET /entries?memo=accrual:fee`.

Note the chart interaction: accruals debit an expense and credit a
liability — the normal direction under a registered chart — but their
reversals debit the liability, which a strict chart rejects with
`ErrAccountDirectionMismatch`. Reversing against a registered chart must
route through a dedicated contra account; accounts that book reversals
stay unregistered.

### Journal rules (`PUT /rules/{id}` / `DELETE /rules/{id}` / `GET /rules`)

Opt-in automatic journaling: rules that match raw journal postings on
debit account, credit account, currency, and amount range, and auto-append
derived journal legs — the classic payment-fintech automations (tax
withholding on a revenue posting, a platform service fee on a merchant
payout). Rules are off by default; each rule must be explicitly enabled.

A rule fires only after the principal entry clears the full `Post` check
order (validation → idempotency replay → frozen → overdraft → daily-limit
→ period). Each derived leg is a full journal entry — its own audit-chain
link, deterministic ID `<entry ID>/rule/<rule ID>/<leg index>`, memo
`rule:<rule ID>:<label>` — and re-runs the risk gates in the same order,
with overdraft and daily-limit evaluated on the aggregate outflow per
credit account (principal + derived legs, like a transfer's fee leg). A
leg that fails any gate rejects the whole `Post`; the commit is atomic
across principal and derived legs, and idempotent replays never re-fire
rules. Derived legs never re-trigger rules, so rule sets cannot cascade.
Illegal rules (empty ID, no legs, ambiguous or non-positive amounts, a
rate above 100% of the principal, impossible match bounds) are rejected
at construction with `400` and stored nothing.

Rule changes are structural config: they don't bump the journal version,
they're recorded in the structured audit log's hash chain (`rule_set` /
`rule_delete` events), and they round-trip through disaster-recovery
snapshots together with the rule-set mutation counter.

```bash
curl -s -X PUT localhost:8080/rules/tax-withholding \
  -d '{"enabled":true,"match_credit_account":"revenue","match_currency":"USD",
       "legs":[{"debit_account":"revenue","credit_account":"tax-payable",
                "rate_bps":800,"label":"withholding"}]}'
# {"rule":{...},"rule_version":1}
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"cash","credit_account":"revenue","amount_cents":10000,"currency":"USD"}'
# 201 — books the principal plus an 800-cent withholding leg to tax-payable
curl -s localhost:8080/rules
# {"rules":[{...}],"rule_version":1}
curl -s -X DELETE localhost:8080/rules/tax-withholding
# {"deleted":true,"rule_version":2}
```

`GET /metrics` exposes `ledger_journal_rule_sets_total`,
`ledger_journal_rule_deletes_total`, and `ledger_journal_rule_legs_total`
(derived legs committed by the engine).

### Daily outflow limits

Per-account, per-currency cap on how much money may leave an account in
one UTC calendar day (fintech risk control — velocity limits on customer
cash-outs). Configure with the `LEDGER_DAILY_OUTFLOW_LIMITS` environment
variable or the `ledger.WithDailyLimit` / `SetDailyLimit` API. A
`POST /entries` whose credit (payer) leg would take the account's
cumulative outflow for the UTC day above the limit is rejected with
`422`; a `POST /transfers` counts the payer's total outflow (amount +
fee leg) against the same budget.

Outflow accumulates per UTC calendar day of each entry's own timestamp,
so the window rolls at UTC midnight and backdated entries count toward
their own day. Limits are opt-in per (account, currency); the check runs
last among the risk controls (after frozen and overdraft), idempotent
replays consume no budget, and a rejected posting books nothing. Setting
or clearing a limit is structural — no version bump — and visible to
audit tooling: each trial balance carries a `daily_limit_cents` field
(default currency) and the reconcile report lists every configured limit
(`daily_limits`). Rejections are counted in
`ledger_daily_limit_rejections_total`.

```bash
LEDGER_DAILY_OUTFLOW_LIMITS="cust-123:USD:100000" ./ledger-api-go
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"cash","credit_account":"cust-123","amount_cents":100000}'
# 201 on the first $1000.00 of the UTC day, then:
# 422 {"error":"ledger: posting would exceed the account's daily outflow limit"}
```

**Subtree limits (hierarchy × daily limits).** A parent's limit constrains
its whole subtree's cumulative outflow for the UTC day — every descendant's
outflow counts against the parent's budget, and the parent's own postings
do too. Limits compose: a child's own limit and each ancestor's limit are
checked independently, the tighter one wins per posting, per currency.
`SetParent` and `SetDailyLimit` validate against today's already-booked
subtree outflow: linking a subtree under a parent whose limit it would
breach, or lowering a limit below the subtree's current usage, is rejected
with `422` (the rejected link is rolled back atomically). The reconcile
report's `subtree_limit_utilization` section lists every configured limit
with its subtree's booked outflow, utilization fraction, and account
count for the report day, so risk tooling can watch velocity-limit
pressure per hierarchy.

### Monthly spend budgets

Per-account, per-currency cap on how much money an account may spend in
one UTC calendar month (fintech risk control — accounting-period spend
control, the counterpart to the daily velocity limit above). Configure
with the `LEDGER_MONTHLY_SPEND_BUDGETS` environment variable or the
`ledger.WithAccountBudget` / `SetAccountBudget` API. A `POST /entries`
whose credit (payer) leg would take the account's cumulative spend for
the UTC month above the budget is rejected with `422`; `POST /transfers`
counts the payer's total outflow (amount + fee leg, source currency for
FX transfers), `POST /entries/batch` every entry's credit leg,
`POST /sweeps` every source leg, and `POST /holds/{id}/capture` the hold
account's settlement leg.

Spend accumulates per UTC calendar month of each entry's own timestamp,
so the window rolls at the UTC month boundary and backdated entries
count toward their own month. Budgets are opt-in per (account,
currency); the check runs after the daily outflow limit among the risk
controls, idempotent replays consume no budget, and a rejected posting
books nothing. The daily limit and the monthly budget are orthogonal —
a rolling outflow window and an accounting-period control — and a posting
must clear both. Spend reserved by pending dual-control reviews counts
in the month's used figure from review creation (the freeze is atomic,
covering funds, daily limit, and budget together); approval settles
against the already-reserved budget. Setting or clearing a budget is
structural — no version bump — and visible to audit tooling: the change
lands in the structured audit log (`budget_set` / `budget_cleared`),
each trial balance carries a `budget_cents` field (default currency),
and the reconcile report's `budget_utilization` section lists every
configured budget with its limit, booked spend, and utilization fraction
for the report month. Rejections are counted in
`ledger_budget_rejections_total`. `POST /entries/dry-run`,
`/transfers/dry-run`, and `/sweeps/dry-run` preview the budget impact:
the response's `budget_impact` rows show each affected budget's used
spend before and after the simulated posting.

```bash
LEDGER_MONTHLY_SPEND_BUDGETS="cust-123:USD:500000" ./ledger-api-go
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"cash","credit_account":"cust-123","amount_cents":500000}'
# 201 on the first $5000.00 of the UTC month, then:
# 422 {"error":"ledger: posting would exceed the account's monthly spend budget"}
```

### Cost allocation rules (Go API: `SetAllocationRule` / `AllocateCost`)

Month-end cost allocation for payment fintech: a shared expense (a
platform fee, a processing bill) booked against one expense account is
split across cost centers in fixed proportions. Register a rule with
`SetAllocationRule` — a source account plus targets with basis-point
weights summing to exactly 10000 — then `AllocateCost(ruleID, amount,
currency)` posts one double-entry leg per target (debit cost center,
credit source) in a single atomic commit for the current UTC calendar
month.

Shares are `floor(amount * bps / 10000)` computed overflow-safe, and the
dust remainder lands on the first target so the legs always sum to the
full amount. An amount so small that a target's share computes to zero
cents rejects the whole allocation (`ErrAllocationDustLeg`) — unlike
journal-rule dust legs, an allocation must cover the whole amount, so a
dropped leg would break the books.

The legs walk the full `Post` gate order — validation, denylist,
chart direction, closed/frozen, overdraft, daily-limit, monthly-budget,
period gate — with intra-allocation aggregate simulation, and commit
atomically: one failing leg rejects the whole allocation and records
nothing. Two things deliberately do not happen: journal rules never
fire on allocation legs (system postings must not cascade through
operator-configured rules, like accruals), and dual-control review never
engages (review is a `PostTransfer` gate; allocations settle
immediately).

Idempotency is deterministic: one allocation per rule per UTC month,
keyed `alloc/<ruleID>/<YYYY-MM>`. A replay returns the original receipt
with `duplicate: true` and books nothing new; the receipt registry is
its own idempotency namespace expiring with the TTL, and the rules plus
the registry round-trip through disaster-recovery snapshots. Leg IDs
are deterministic (`alloc/<ruleID>/<YYYY-MM>/leg/<i>`) and carry a
memo of `alloc:<ruleID>`, so allocations are discoverable through
`GET /entries?memo=alloc:<ruleID>`.

```go
l.SetAllocationRule(ledger.AllocationRule{
    ID:     "platform-fee",
    Source: "fee-clearing",
    Targets: []ledger.AllocationTarget{
        {Account: "merchant-a", PctBps: 5000},
        {Account: "merchant-b", PctBps: 3000},
        {Account: "merchant-c", PctBps: 2000},
    },
})
receipt, dup, err := l.AllocateCost("platform-fee", 10000, "USD")
// 3 legs: merchant-a 5000 / merchant-b 3000 / merchant-c 2000,
// fee-clearing credited 10000 — atomic, or nothing.
```

### Sanctions/denylist screening (`POST /denylist`)

Opt-in sanctions/denylist: exact account IDs and prefix patterns whose
accounts may not move money (fintech compliance — sanctions screening on
every money movement). Manage with `POST /denylist` (`{"account": "<id>"}`
or `{"prefix": "<prefix>"}`), `GET /denylist`, `DELETE /denylist/{value}`
(`?prefix=true` for patterns), and `POST /denylist/import` for loading a
sanctions feed in one atomic batch (already-listed rows are skipped, a
malformed row aborts the whole import). The operator identity comes from
the `x-ledger-actor` header and is recorded on the audit event and the
entry itself, like freeze/approve/close.

`POST /entries`, `POST /entries/batch`, `POST /transfers`, and
`POST /holds/{id}/capture` screen every leg against the list. A hit is
rejected with `422 {"error":"ledger: account is on the sanctions/denylist:
..."}`. Screening runs before the idempotency replay check — a sanctions
hit rejects even a replay, and the denied request leaves no idempotency
trace — and every rejection is recorded as a `denylist_rejected` audit
event, because the compliance trail must show the attempt, not just the
absence of a posting. Reads (balance, snapshot, trial balance, reconcile)
are unaffected, so compliance tooling can keep watching a denied account.
List changes are structural (no version bump) and round-trip through
disaster-recovery snapshots — a restore never silently lifts screening.
Rejections are counted in `ledger_denylist_rejections_total`.

```bash
curl -s -X POST localhost:8080/denylist \
  -H 'x-ledger-actor: ops-chen' -d '{"prefix":"ofac-sdn-"}'
curl -s -X POST localhost:8080/entries \
  -d '{"debit_account":"ofac-sdn-123","credit_account":"equity","amount_cents":100}'
# 422 {"error":"ledger: account is on the sanctions/denylist: account \"ofac-sdn-123\" matched denylist entry \"ofac-sdn-*\""}
```

### Large-transfer auto-split (Go API: `PostTransferSplit`)

When a transfer exceeds a per-transfer cap — a downstream rail's
single-transaction limit, for example — `PostTransferSplit` splits it
into multiple chunk postings that commit atomically (fintech
payout batching). The chunk size is the tighter of the opt-in cap
(`ledger.WithMaxTransferChunkCents`, or the `LEDGER_MAX_TRANSFER_CHUNK`
environment variable in cents) and the payer's daily outflow limit in
the transfer's currency: a transfer of $2,500 with a $1,000 cap books
three chunks of $1,000 / $1,000 / $500 as `<id>/chunk-0..2`. Any leg
failure — validation, ID conflict, frozen/closed account, overdraft,
daily-limit, closed period — rolls the whole split back with zero
records; readers never observe a half-posted split.

Each chunk is checked against the day's cumulative outflow budget in
commit order, so the daily limit stays a hard cap that chunking can
never circumvent. The payer's overdraft check runs against the total
outflow (amount + fee), since the split settles at once. Fees follow
the transfer-fee convention by default — computed once on the whole
amount, one `<id>/fee` leg — or per chunk with
`ledger.WithSplitFeePerChunk` (`<id>/chunk-<i>/fee` legs; an explicit
`FeeCents` is always charged once). Chunk entries carry idempotency
keys derived per (transfer, chunk) (`<key>/chunk-<i>`), and replaying
the transfer's key returns the whole receipt (`Duplicate: true`).
Splitting never bypasses dual-control review: a transfer meeting the
payer's review threshold is rejected with
`ErrSplitTransferNeedsReview` and belongs in `PostTransfer`'s review
flow. Cross-currency splits convert each chunk at the transfer's one
rate (floored per chunk; the receipt discloses the summed conversion)
with every leg keeping its single currency. The chunking config
round-trips through disaster-recovery snapshots.

```go
l := ledger.New(ledger.WithMaxTransferChunkCents(100000)) // $1,000 cap
receipt, err := l.PostTransferSplit(ledger.Transfer{
    ID: "pay-2026-001", From: "ops", To: "vendor", AmountCents: 250000,
    IdempotencyKey: "pay-2026-001",
})
// receipt.Chunks: [{chunk_index:0, chunk_count:3, amount_cents:100000, …}, …]
```

### Balance threshold watches

Per-account, per-currency opt-in alert band for fintech operations (low
funding-source balances, merchant float monitoring, treasury overflow
detection): a low level, a high level, or both. Configure with the
`LEDGER_BALANCE_WATCHES` environment variable
(`"<account>:<currency>:<lowCents>:<highCents>,..."`, either level may be
empty), the legacy `LEDGER_LOW_BALANCE_THRESHOLDS` (low side only), or
the `ledger.WithBalanceWatch` / `SetBalanceWatch` API. When a committed
posting takes an account's balance below the low level (or above the
high level), the ledger records one structured audit event
(`balance_threshold_breached`, carrying `account`, `currency`,
`direction` (`"low"`/`"high"`), `threshold_cents`, `balance_cents`,
`version`) and bumps `ledger_balance_watch_breaches_total`.

The alert is advisory, never a risk-control rejection: postings are never
blocked for crossing a level. Evaluation runs strictly after the atomic
commit zone and reads balances through the read-only balance path, so it
can neither fail nor alter a posting. Edge-triggered, one event per
breach: further postings while the balance stays outside the band are
silent, and recovering inside the band re-arms the alert. Replays and
rejected postings (frozen, overdraft, validation, ...) never fire —
rejections book nothing, so they never evaluate. Setting or clearing a
watch is structural — no version bump, and a config change never
backfills stale alerts — it survives disaster-recovery snapshots (the
breach state is re-derived from the restored balances, and pre-LG-61
`low_balance_thresholds` snapshot entries import as low-only watches),
each trial balance carries `low_balance_threshold_cents` /
`high_balance_threshold_cents` (default currency), the reconcile report
lists every watch (`balance_watches`), and watch CRUD itself is audited
(`balance_watch_set` / `balance_watch_cleared`).

```bash
LEDGER_BALANCE_WATCHES="cust-123:USD:50000:,treasury:USD::10000000" ./ledger-api-go
# post cust-123's USD balance below $500.00, then:
curl -s localhost:8080/metrics | grep balance_watch
# ledger_balance_watch_breaches_total 1
```

### Balance-change notification hooks

Opt-in per-account balance-change callbacks for downstream fintech
plumbing — payment notifications, settlement webhooks, cache
invalidation. Register with `ledger.WithBalanceChangeHook` /
`SetBalanceChangeHook` (Go API). Every committed `Post`, `Transfer`,
`Capture`, `Sweep`, and `Merge` dispatches one `BalanceChange` event per
touched (account, currency): `account`, `currency`, `old_balance_cents`,
`new_balance_cents`, `delta_cents` (negative on the paying side),
`version` (the ledger version after the commit), and `trace`
(the entry / transfer / sweep / merge ID, or the hold ID for captures).
The payer's delta nets the full outflow (amount + fee).

Events are fully materialized after the atomic commit zone and
dispatched asynchronously — one goroutine per event — so a slow hook
can never block the commit path. Hook failures are isolated: a
panicking callback is recovered, and a callback that overruns its
per-call timeout (default 5s, configurable via
`WithBalanceChangeHookTimeout` / `SetBalanceChangeHookTimeout`) is
abandoned. Neither can alter or roll back the committed entries.
Idempotent replays and rejected operations fire nothing. With no hook
registered (the default), the commit path allocates nothing and behaves
exactly as before.

```go
l := ledger.New(ledger.WithBalanceChangeHook(func(ev ledger.BalanceChange) {
    // fan out to the payments notification service / webhook queue
    notify(ev.Account, ev.Currency, ev.DeltaCents, ev.Trace)
}))
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
same `LEDGER_IDEMPOTENCY_TTL`. A capture may carry `external_ref`
(optional, ≤ 64 UTF-8 characters), which is stamped on the settlement
journal entry; a reference already journaled on another entry rejects
the capture with `400`.

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
leg — principal and fee — in that one currency**, unless `to_currency`
names a different settlement currency: a **cross-currency transfer**
converts at the configured rate for the `(currency, to_currency)` pair
(see below). Malformed codes are rejected with `400`; both are counted in
`ledger_currency_rejections_total`.

#### FX transfers

Set `to_currency` (different from `currency`) and the ledger converts:
the payee is debited the converted amount in the target currency while
the payer is credited the original amount in the source currency, with
the FX clearing account as the counterparty of both legs. The conversion
is `floor(amount * rateNum / rateDen)` in exact 128-bit integer
arithmetic — no float64 ever touches money, and an overflowing conversion
is rejected before booking. The receipt discloses the conversion in its
`fx` field (currencies, rate, converted amount, and the ledger version at
which the rate took effect).

```bash
curl -s -X POST localhost:8080/transfers \
  -d '{"transfer_id":"fx-001","from_account":"alice","to_account":"erin","amount_cents":10000,"currency":"USD","to_currency":"EUR"}'
# 201 {...,"fx":{"from_currency":"USD","to_currency":"EUR","rate_num":108,"rate_den":100,"converted_cents":10800,...}}
```

Each currency's books stay balanced independently: the two journal
entries (`<transfer_id>` in the target currency, `<transfer_id>/fx` in
the source currency) each debit and credit within one currency, so
`VerifyAccountingEquation` and the audit chain cover FX transfers with no
special cases. The fee leg, when present, is charged in the **source**
currency on top of the amount; overdraft and daily-limit checks guard the
payer's source-currency total outflow (amount + fee) — a EUR balance can
never cover a USD outflow. A pair with no configured rate is rejected
with `422`; a transfer with no FX account (neither `fx_account` on the
request nor `LEDGER_FX_ACCOUNT` configured) is rejected with `400`.

Rates are opt-in structural config, directional (`USD→EUR` and `EUR→USD`
are independent), and settable at runtime via `SetFXRate` (Go API) or
`LEDGER_FX_RATES` at startup:

```bash
LEDGER_FX_ACCOUNT="fx-pnl" \
LEDGER_FX_RATES="USD:EUR=108/100,USD:CNY=720/100" ./ledger-api-go
#   1 USD = 1.08 EUR, 1 USD = 7.20 CNY
```

A malformed rate table fails the startup fast. Rates survive
disaster-recovery snapshots (with their effective versions) and are
listed in the `POST /reconcile` report's `fx_rates` section. Cross-currency
attempts are counted in `ledger_fx_transfers_total`.

#### Rate-table snapshot versioning

Every mutation of the rate table (`SetFXRate`, `SetFXRateRat`,
`RemoveFXRate`, and rates installed at construction) bumps a snapshot
version and persists a **full copy of the table** at the new version
(`ledger.FXRateTableSnapshot(version)`). Each FX receipt pins the
snapshot version in effect when it posted
(`fx.rate_snapshot_version`), so a historical audit reproduces exactly
the table that converted any transfer — even after the rate was replaced
or removed. Every mutation also emits an `fx_rate_change` audit event
(flat version bracket, like `freeze`). `POST /reconcile` reports the
history in `fx_rate_versions` (`current_version`, `total_snapshots`,
`continuity_ok`): a gap in versions 1..current means snapshot history was
lost and historical reproduction for the missing versions is gone. The
version counter and snapshots survive disaster-recovery snapshots;
imports of pre-versioning snapshots rebuild version 1 from the table.

#### Exact decimal rates and rate expiry (`SetFXRateRat`)

`SetFXRate(num, den)` takes an integer ratio; `SetFXRateRat(from, to,
rate *big.Rat, ttl)` takes an **exact rational** plus an optional
lifetime. Decimal rates are exact here — a rate parsed from `"1.10"` is
exactly `11/10`, while the same rate rounded through `float64` would be
`1.1000000000000000888`, a ratio no integer pair represents. Use
`ParseFXRateDecimal` to turn operator-supplied decimal strings into the
`*big.Rat` it takes; a ratio that does not fit `int64` is rejected rather
than silently truncated.

A positive `ttl` gives the rate an expiry (`ExpiresAt`): a cross-currency
transfer posted at or after the expiry is rejected with `422`
`{"error":"ledger: FX rate for currency pair has expired"}` — a stale
rate is never applied silently. Installing a fresh rate unblocks the pair;
`RemoveFXRate` works as before. The idempotency replay check runs before
the rate lookup, so replaying a key posted while the rate was live still
returns the original receipt after expiry. Expiry survives snapshots, is
visible via `FXRate()`, and expired-rate rejections are counted in
`ledger_currency_rejections_total`.

**FX rate expiry watch.** The transfer gate rejects expired rates, but
nothing told the operator a rate was *about* to expire — the first signal
was a rejected transfer. The background watch (`LEDGER_FX_RATE_WATCH_INTERVAL`,
e.g. `5m`; see [Environment](#running)) scans the rate table on a ticker,
read-only, and edge-triggers two audit events: `fx_rate_stale` when a
rate enters the opt-in warning window (`LEDGER_FX_RATE_WARNING_THRESHOLD`,
e.g. `72h`), and `fx_rate_expired` once it crosses expiry — each counted
in `ledger_fx_rate_stale_total` / `ledger_fx_rate_expired_total`. A rate
that stays stale across ticks is reported once on entering the window
and once more on crossing expiry (dedup identity: expiry + effective
version, so renewing a rate re-arms both warnings); removing a pair
drops its mark. Unset or invalid interval disables the watcher; unset
or invalid threshold disables stale warnings only — expiry alerts stay
active.

**Market FX rate provider adapter.** The rate table above is the static
leg; the watch can also poll a caller-implemented `FXRateProvider`
(`FetchRates(ctx) ([]MarketRate, error)`) on its interval and install
the quotes through the `SetFXRateRat` write path — the interface is
defined by the library and implemented by the caller (a market-data
feed client, a treasury pricing service), mirroring realtime-event-bus's
`BridgeTransport`. Opt in with `WithFXRateProvider`; nil (the default)
keeps the static-table behavior byte-for-byte. Per tick: each quote is
validated like `SetFXRateRat`; a pair under an active manual override
keeps the human price (the feed never writes it); an unchanged static
quote skips the write (no snapshot churn); a moved price is installed
and audited as `fx_rate_moved` (from/to, old and new rate, provider,
TTL). A fetch error is audited as `fx_rate_provider_failed` — never
silent — and never kills the worker; the next tick runs as scheduled.
`ledger_fx_rate_refreshes_total` / `ledger_fx_rate_moved_total` /
`ledger_fx_rate_provider_failed_total` count installs, moves, and
fetch failures.

**FX rate triangular-consistency check.** Each transfer prices its legs
off the rate table independently, so nothing on the write path asks
whether the table is *self-consistent*: if USD→EUR→JPY→USD multiplies
to something far from 1, the book has an arbitrage leak — a payer
routing through the table can mint or burn value across currencies
while every single leg looks legitimate. The background consistency
watch (`LEDGER_FX_CONSISTENCY_INTERVAL`, e.g. `5m`; see
[Environment](#running)) scans the rate table on a ticker, read-only,
and checks every directed 3-cycle with all three legs present. The
product is computed in exact `big.Rat` arithmetic (the table stores
integer num/den, so no float rounding can hide or invent a deviation);
a cycle whose |product − 1| exceeds the opt-in threshold
(`LEDGER_FX_CONSISTENCY_THRESHOLD_BPS`, integer basis points)
edge-triggers an `fx_rate_inconsistent` audit event (path, legs,
exact product, exact deviation in bps) and counts in
`ledger_fx_rate_inconsistency_total`. The check is advisory only — it
never blocks a conversion; the inconsistency is a data-quality problem
in the operator's rate feed, not a booking bug. Edge-triggered like the
expiry watch: a cycle that stays inconsistent across ticks alerts once
when it *becomes* inconsistent (dedup identity: the three legs'
num/den values, so a leg-value change re-arms; removing a leg drops the
mark). Unset or invalid interval disables the watcher; unset or invalid
threshold disables the check — the threshold is opt-in, so microscopic
feed wobble never pages by default.

**Manual FX rate overrides (dual control).** When the feed is wrong — a
stale quote, a market dislocation — an operator can propose a manual
override price for one directional pair via `SetRateOverride(from, to,
rate, ttl, reason, proposer)` (exact `*big.Rat` price; the reason is
required). Maker-checker applies, following the transfer-review
convention: the override takes effect only after a *second* operator
approves it (`ApproveRateOverride`), and the proposer can never approve
their own. Two guardrails bound the blast radius: the override may not
deviate from the live market rate by more than the configured
basis-points cap (default 10%, `WithRateOverrideMaxDeviationBps`,
checked at proposal and re-checked at approval — a market that moved in
between blocks the approval), and every override auto-expires
(per-call TTL, default one hour via `WithRateOverrideTTL`;
`ExpireRateOverrides` sweeps lapsed overrides). While active, the pair's
FX transfers convert at the override price; each conversion emits an
`fx_rate_overridden` audit event binding the transfer to the override's
provenance (proposer, approver, reason, both rates, the measured
deviation), the receipt discloses `override_applied` plus the market
rate and approver, and `ledger_fx_rate_overridden_total` counts them.
Overrides are operational state like transfer reviews — they do not
round-trip through snapshots; after a restore, operators re-propose.

#### FX revaluation (`POST /revaluations`)

Period-end remeasurement: every non-base-currency balance is revalued at
the current rate table into the reporting currency, and the unrealized FX
gain/loss since the last revaluation is booked as ordinary journal
entries against the configured revaluation P&L account
(`LEDGER_REVAL_ACCOUNT`, or `ledger.WithRevaluationAccount` /
`SetRevaluationAccount` in the Go API; unset means `POST /revaluations`
is rejected with `400`).

```bash
curl -X POST localhost:8080/revaluations \
  -d '{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}'
# 200 {"as_of":"...","base_currency":"USD","rate_version":3,
#      "items":[{"account":"trader","currency":"EUR","base_currency":"USD",
#        "foreign_balance_cents":10000,"rate_num":12,"rate_den":10,
#        "prior_base_value_cents":11000,"new_base_value_cents":12000,
#        "delta_cents":1000,"entry_id":".../trader/EUR/reval","status":"revalued"},...],
#      "entries":[{...}]}
```

Each revaluation is one base-currency entry per `(account, currency)`:
a gain books debit `account@base` / credit `reval-pnl@base`, a loss the
mirror image — so the account's base-currency row is its unrealized
carrying value and the per-currency accounting equation holds by
construction. The first revaluation books the full converted value
(initial recognition); later runs book only the delta against the stored
baseline, so an unchanged book and rate table book nothing. Entry IDs are
deterministic (`<asof unix>/<account>/<currency>/reval`), making a repeat
run for the same `asof` idempotent.

The run is **best-effort per account/currency, not atomic**: each item
reports `revalued` | `unchanged` | `skipped_duplicate` |
`skipped_no_rate` | `skipped_rate_expired` | `failed`, so one missing rate
or frozen account never blocks the rest. Entries run the `POST` risk
gates in order (frozen, overdraft, daily-limit, period — keyed on the
`asof`, so a revaluation can never book into a closed period) but never
trigger journal rules and never enter dual-control review, like
period-closing entries. `POST /reconcile` reports the unrealized position
in `revaluation_impact` (per-account carrying values and their rates) and
the P&L account in `revaluation_account`; committed entries are counted
in `ledger_revaluations_total`. Baselines and the P&L account survive
disaster-recovery snapshots.

The receipt's `fx` field now discloses both sides of the conversion —
`source_cents` (pre-conversion) and `converted_cents` (post-conversion) —
plus the rate's expiry when the rate carries one; the structured audit
log's `transfer` event seals the same provenance (`source_cents`,
`converted_cents`, `rate_num`, `rate_den`, `rate_effective_version`) in
its hash-chained `Details`, binding the journal's two legs (the `/fx` leg
books the source amount, the principal the converted amount) to the exact
rate that produced them.

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

### `GET /balance-history`

Balance time-series query: the sampled balance history of one
(account, currency) over a time window — the read side of the background
recorder (`ledger/balance_history.go`). Where `/balance-at` replays the
journal per query (O(version) each time), the recorder amortizes that
into a background time series: every tick samples all (account,
currency) balances and appends one JSON row per key to per-day JSONL
files (`balance-history-YYYY-MM-DD.jsonl`), so a history query is a
bounded file scan. Each row carries `version`, so a sample can be
cross-checked against `/balance-at` — the primitive behind
reconciliation dashboards and payment reporting ("show me this
settlement account's balance over the last 30 days").

```bash
curl -s "localhost:8080/balance-history?account=cash&currency=USD&since=2026-10-01T00:00:00Z&until=2026-10-31T00:00:00Z"
# {"account":"cash","currency":"USD","points":[
#   {"account":"cash","currency":"USD","balance_cents":12500,"version":42,"at":"2026-10-11T07:00:00.123Z"},
#   ...]}
```

- `account` is required (`400` without it); `currency` defaults to USD;
  `since`/`until` are optional RFC 3339 bounds (`400` on malformed
  input). Points come back ordered by `(at, version)`.
- With no history directory configured the endpoint answers `404`
  fail-closed — an unconfigured series is a missing feature, not an
  empty history.
- The series is append-only and the recorder holds no cursor: after a
  disaster-recovery snapshot restore, a worker pointed at the same
  directory keeps appending to the current day's file — old rows are
  never rewritten, so the series continues instead of resetting. The
  directory is process configuration, not ledger state, and is
  deliberately not part of the snapshot.
- Retention is by file modification time
  (`LEDGER_BALANCE_HISTORY_MAX_AGE_MS`, default 90 days); only the
  recorder's own files are ever pruned. Sampling is read-only: the
  recorder takes the read lock, writes nothing to the books, and never
  bumps the version. Ticks are counted in
  `ledger_balance_history_snapshots_total` (one per tick, not per row);
  failed ticks are logged and retried on the next tick.

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

### `GET /accounts/{id}/ofx` — OFX 2.x bank statement export

A bank statement for one account, generated from the journal in OFX 2.x
form (Go: `ledger.ExportOFX`) — the exchange format accounting packages
and bank uploaders import. Query parameters: `currency` (optional; default
exports every currency the account holds as separate `STMTTRNRS` segments
— currencies never mix inside a segment, matching the per-currency books),
`since` / `until` (optional RFC 3339 bounds, half-open `[since, until)`),
`after_entry_id` (optional resume checkpoint).

```bash
curl -s 'localhost:8080/accounts/cash/ofx?currency=USD' -o cash.ofx
```

Semantics, stated so importers can trust them:

- One `STMTTRN` per effective journal row where the account is either leg.
  `FITID` is the journal entry ID — stable across exports, so importers
  can dedupe on it. `DTPOSTED` is the entry's `CreatedAt` in UTC.
- `TRNTYPE` and the `TRNAMT` sign are **account-centric**: the account as
  the debit leg means money was added to it (`CREDIT`, positive amount);
  the account as the credit leg means money left it (`DEBIT`, negative
  amount). Debit always increases the account's balance in this ledger's
  bookkeeping, so `CREDIT` always adds.
- `MEMO` carries the journal memo, `REFNUM` the entry's external
  reference, both XML-escaped.
- `LEDGERBAL` is the account's true ledger state at `DTEND`: the balance
  folded from every effective journal row with `CreatedAt < until` —
  not just the exported page. Pending-review rows never touched balances
  and are excluded from the statement and the fold.
- The document is the OFX hybrid form: the classic SGML-style header
  block (`OFXHEADER:100` … `VERSION:211`) followed by the OFX 2.x XML
  body, which is what most importers parse.

Resume: the response carries the last exported entry's ID in the
`X-Ledger-OFX-Checkpoint` header. Pass it back as `after_entry_id` to
continue exactly where the previous page stopped — no duplicates, no
skips. An unknown checkpoint fails `400` rather than silently
re-exporting from the start (which would duplicate rows downstream).
`GET /metrics` counts exports as `ledger_ofx_exports_total`.

### `POST /ofx/import` — OFX 2.x statement import to pre-posting drafts

The inverse of the export above: it parses an OFX 2.x bank statement
(SGML or XML body style, with or without the SGML header block) and
returns a **pre-posting report** — every statement line as a
`DraftEntry` the operator reviews, matched against the account's
journaled rows. Go: `ledger.ParseOFX` / `ledger.ImportOFX`.

```bash
curl -s -X POST localhost:8080/ofx/import \
  -H 'Content-Type: application/json' \
  -d '{"account":"cash","ofx":"<OFX>...</OFX>"}'
# 200 {"document_sha256":"…","account":"cash",
#      "items":[{"transaction":{...,"direction":"in","amount_cents":15000,...},
#                "draft":{"id":"ofx/stmt-1","debit_account":"cash","amount_cents":15000,...},
#                "status":"matched","matched_entry_id":"e1"}, ...],
#      "matched":1,"mismatched":0,"unmatched":2,"duplicate":false}
```

Semantics, stated so operators can trust them:

- **Nothing is posted.** The import writes no journal rows, enters no
  hash chain, and bumps no version — booking external money movement is
  an operator decision; the report only says what the statement claims
  and what the ledger already has.
- Direction mirrors the export: `CREDIT` (positive) means the account
  was the debit leg — money in (`"direction":"in"`); `DEBIT` (negative)
  means it was the credit leg — money out. Amounts parse as exact
  decimals to cents, no floats; `DTPOSTED` accepts `YYYYMMDDHHMMSS` and
  `YYYYMMDD`.
- **Matching** — a journaled row is a candidate for a statement line
  when it is effective (pending-review rows are excluded, like the
  exporter), carries the same currency, has equal `|amount|`, and its
  `CreatedAt` falls within ±24h of `DTPOSTED`. Exactly one candidate is
  `matched`; several is `mismatched`; none is `unmatched`.
- **Idempotency** — the ledger keeps every import report keyed by the
  document's SHA-256. Re-importing the same document returns the
  original report with `duplicate:true`; a re-uploaded statement never
  double-counts. The registry survives disaster-recovery snapshots.
- A statement naming a different account than the import target, a
  malformed document, a missing account, or an empty document is a
  `400`. There is no special auth, like the export endpoint.
  `GET /metrics` counts imports as `ledger_ofx_imports_total`.

### `GET /accounts/{id}/statement` — JSON account statement

A bank-style account statement for one account over the half-open
window `[from, to)` (Go: `ledger.GenerateStatement`): opening and
closing balances per currency, the period's journal rows in
`(CreatedAt, ID)` order, a per-currency rollup of the transfer fee legs
the account paid or received, and a structured event summary. Query
parameters: `from` / `to` (optional RFC 3339 timestamps; omitted `from`
defaults to the first day of the current UTC month at 00:00, omitted
`to` defaults to now). A malformed timestamp or an inverted window
(`from` after `to`) is a `400`, fail-fast. Unknown accounts return a
valid empty statement, not 404.

```bash
curl -s 'localhost:8080/accounts/cash/statement?from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z'
# {"account":"cash","period":{"from":"...","to":"..."},
#  "opening_balances":[{"currency":"USD","balance_cents":0}],
#  "closing_balances":[{"currency":"USD","balance_cents":300}],
#  "entries":[...],"fee_summary":[],
#  "event_summary":{"freeze_events":[],"transfer_reviews":[],
#   "review_counts":{},"overdraft_protected":false,"currently_frozen":false},
#  "generated_at":"...","version":3}
```

Semantics, stated so consumers can trust them:

- **Balances** — opening and closing balances fold the effective
  journal prefix with `CreatedAt` before `from` / `to` (the time-domain
  twin of `balance-at`'s version-prefix fold), per currency. Currencies
  are never summed together.
- **Entries** — effective journal rows touching the account in
  `[from, to)`, sorted by `(CreatedAt, ID)`. Pending-review rows are
  journaled but not effective (they never touched balances), so the
  statement excludes them, like the OFX export does; opening balance
  plus the period rows' net always equals closing balance.
- **Fee summary** — the period's transfer fee legs (journal rows with
  ID `<transfer ID>/fee`) that touched the account, rolled up per
  currency: `paid_cents` (the account as the fee leg's credit leg),
  `received_cents` (the account as the fee account), and `entry_count`.
- **Event summary** — drawn from the ledger's structured in-memory
  sources, never fabricated: `freeze_events` (the account's
  freeze/unfreeze/auto-release transitions in the window, in
  chronological order), `transfer_reviews` (dual-control reviews
  involving the account created in the window, with per-status
  `review_counts`), `overdraft_protected` and `currently_frozen`
  (current risk-control state). Overdraft rejections themselves are
  not listed: rejected posts record nothing by design, so there is no
  structured source to draw them from.
- **Read-only** — the statement is a pure query under the read lock:
  it bumps no version, books nothing, appends no audit-chain link, and
  triggers no rules or hooks. `version` is the ledger version read at,
  so consumers can detect whether anything changed between two reads.
  There is no special auth, like the other read-only account
  endpoints. `GET /metrics` counts requests as
  `ledger_statement_queries_total`.

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

The response also carries `frozen`, `closed`, `overdraft_protected`,
`daily_limit_cents`, and `low_balance_threshold_cents` for the default
currency (`0` means the corresponding control is not configured there;
per-currency levels live in the reconcile report).

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

### Account tags and the tag-dimension rollup (`PUT /accounts/{id}/tags`, `GET /rollup?tag=k:v`)

Tags are small `(key, value)` classification labels on accounts — a
merchant's business line (`line=retail`), a regulatory segment
(`segment=cross_border`), a cost center (`cost_center=ops`) — for
report-driven rollups that cut **across** the hierarchy tree instead of
following it. Tagging is structural, like the parent link: it does not
bump the ledger version, and tag sets round-trip through the
disaster-recovery snapshot. Tag changes are recorded in the structured
audit chain (`account_tags_set`, clears marked `tags_cleared=true`).

```bash
curl -s -X PUT localhost:8080/accounts/retail-1/tags \
  -H 'Content-Type: application/json' -d '{"tags":{"line":"retail"}}'
# 200 {"account":"retail-1","tags":{"line":"retail"}}

curl -s "localhost:8080/rollup?tag=line:retail"
# {"tag_key":"line","tag_value":"retail","accounts":["retail-1","retail-2"],
#  "by_currency":[{"currency":"USD","balance_cents":3000}],"version":5}
```

- Keys and values are 1–64 characters of `[A-Za-z0-9._-]`; anything
  else → `400`. `PUT` replaces the account's whole tag set atomically;
  an empty map clears it.
- `GET /rollup?tag=k:v` aggregates every account carrying that exact
  pair, per currency. A pair matching nothing returns an empty account
  list with zero balances (never a 404 — an operator's report shows an
  empty table). A missing or malformed `tag` parameter → `400`.
- Tags carry no hierarchy inheritance: only the tagged accounts
  contribute, so the classification is explicit and auditable.
- The trial balance of a tagged account carries its `tags`, and the
  `POST /reconcile` report lists every tagged account (`account_tags`),
  so downstream reporting can group the same rows by tag dimension.

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
- `memo`: optional keyword filter — only entries whose memo note contains
  it as a case-sensitive substring are returned. It composes with the
  time window and cursor pagination (a cursor from a filtered page resumes
  the same filtered sequence), so reconciliation tooling can page through
  every entry tagged with an order ID or invoice reference.
- `ref`: optional exact-match filter — only the entry whose `external_ref`
  equals it exactly is returned. It composes with the time window, the
  `memo` filter, and cursor pagination.
- Malformed timestamps, cursors, or limits → `400` with an `{"error": ...}` body.

### `GET /entries/verify`

Recomputes the ledger's tamper-evident audit chain and reports whether it
is intact. The chain links every posted entry to the previous link's
SHA-256 hash (`SHA-256(prevHash || entry)`); idempotent replays add no link.

```bash
curl -s localhost:8080/entries/verify
# {"ok":true,"links":128,"head":"9f2c...","anchor":{"configured":true,...}}   # empty ledger: links=0, head=64 zeros
```

- `200 {"ok":true,"links":N,"head":"<hex>","anchor":{...}}` — the chain is
  intact; `head` moves if and only if a new entry was posted. The `anchor`
  section is the external-anchoring status (see below): `configured`,
  `key_id`, `checkpoints`, `latest`, `verified`, and `error` when the
  checkpoint history does not check out.
- `500 {"ok":false,"error":"..."}` — verification failed (rewritten entry,
  missing journal row, spliced/reordered chain). This is an
  operator-level integrity incident, not a client error.

### External audit-chain anchoring (`POST /entries/anchor`)

For compliance and third-party attestation, the audit-chain head can be
anchored externally: `POST /entries/anchor` signs the current head with a
configured Ed25519 key (standard library `crypto/ed25519`) and appends an
`AnchorCheckpoint{seq, head_hash, anchored_at, key_id, signature}` as one
JSON line to `checkpoints.jsonl` in the audit directory (fsync'd before
the call returns). The signature covers a domain-separated canonical
message (`"LEDGER-ANCHOR/v1:" || BE64(seq) || headHash || BE64(unixnano)`),
so it can never be transplanted into another protocol.

```bash
curl -s -X POST localhost:8080/entries/anchor
# {"seq":128,"head_hash":"9f2c...","anchored_at":"2026-10-10T...Z","key_id":"ab12...","signature":"..."}
```

- The key comes from `LEDGER_ANCHOR_KEY` (hex of a 32-byte seed or a
  64-byte private key; invalid values fail the startup fast). The key
  itself never leaves the process — snapshots, checkpoints, and the audit
  log only ever record the `key_id` (public-key prefix).
- Anchoring is operator-driven (e.g. from the day-end reconcile job);
  `Anchor()` is idempotent for an unchanged head — re-anchoring without
  new posts returns the existing checkpoint instead of appending a
  duplicate.
- `GET /entries/verify` replays every checkpoint against the live chain:
  sequence numbers must strictly increase, each `head_hash` must equal the
  chain link at its `seq`, and each signature must verify. The Reconcile
  report's `audit_chain` section carries `anchor_checkpoints`, `anchor_ok`,
  and `anchor_error` — a rewritten journal, a reordered checkpoint file,
  or a forged signature surfaces there.
- `ledger_anchors_total` counts anchors in `/metrics`.

### RFC 3161 timestamp anchoring (`POST /entries/tsa-anchor`)

Self-signed checkpoints prove *what* was anchored, not *when*. For
external, independently verifiable time, the audit-chain head can be
countersigned by an RFC 3161 timestamp authority (TSA — e.g. FreeTSA or
your own `openssl ts` service): `POST /entries/tsa-anchor` sends the
SHA-256 head digest to the TSA, parses and validates the returned CMS
`TimeStampToken` (hand-rolled DER parser — no third-party dependency),
and appends a `TimestampAnchor{seq, head_hash, tsa_url, token_der,
trusted, anchored_at}` as one JSON line to `tsa_anchors.jsonl` in the
audit directory (fsync'd before the call returns).

```bash
curl -s -X POST localhost:8080/entries/tsa-anchor
# {"seq":128,"head_hash":"9f2c...","tsa_url":"https://tsa.example.com/tsa",
#  "token_der":"MIIN...","trusted":true,"anchored_at":"2026-10-14T...Z"}
```

- The endpoint comes from `LEDGER_TSA_URL`; `LEDGER_TSA_CERT_FILE`
  (PEM certificate) optionally pins the TSA trust anchor. Without a
  pinned cert, token signatures verify against the token's own embedded
  certificate and are honestly reported with `"trusted":false` — the
  timestamp is TSA-signed either way, but not anchored to *your* TSA.
- A background worker (`LEDGER_TSA_INTERVAL`, e.g. `"1h"`) re-anchors
  the head on a cadence; the first tick happens after one full interval.
  Anchoring is idempotent for an unchanged head. TSA outages never block
  posting — each failure is degraded, not fatal: a `tsa_anchor_failed`
  audit event is recorded, `ledger_tsa_anchor_failures_total` ticks up,
  and the worker retries on the next tick.
- `GET /entries/verify` gains a `tsa` section (`configured`, `anchors`,
  `trusted`, `last_anchor`), and the Reconcile report's `audit_chain`
  section gains `tsa_anchors`, `tsa_anchor_ok`, `tsa_anchor_error`:
  every stored token is cryptographically re-verified and replayed
  against the live chain, so a journal rewritten after the TSA
  countersigned it — the wholesale-rewrite attack this feature exists to
  detect — surfaces there. An offline verifier is available for
  third-party audit: `ledger.VerifyTimestampAnchors(dir, trusted)`.
- `ledger_tsa_anchors_total` counts successful anchors in `/metrics`
  (manual requests and worker ticks alike).

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
| `merges`               | every committed account merge, sorted by merge ID: `merge_id`, `from_account`, `to_account`, the per-currency `legs`, and `created_at`. Merged sources stay frozen — cross-check `from_account` against `frozen_accounts` in the same report |
| `fx_applied`           | `true` when the report carries the opt-in base-currency summary (see below); `false` on a plain scan |
| `base_currency_summary` | the base-currency rollup, present only when `fx_applied` is `true`: `base_currency`, base-currency `total_debits_cents`/`total_credits_cents`, per-currency `conversions`, converted `discrepancies`, the `fx_snapshot` of rates used, `missing_rates`, and `fx_incomplete` |

#### Base-currency summary (opt-in)

Passing `{"base_currency":"USD"}` in the `POST /reconcile` body folds every
per-currency totals row and every discrepancy through the FX rate table into
one reporting-currency rollup — the number a settlement operator actually
wires. Each currency converts at the rate in effect at scan time
(`floor(amount * num / den)`, the same integer convention as FX transfers,
no float64 on money); the report discloses the exact rate it used per
currency (`fx_snapshot`: `currency`, `rate_num`/`rate_den`, and
`rate_asof_version` — the ledger version at which the rate took effect; the
base currency itself converts at identity `1/1` with version `0`).

Currencies with no `(currency -> base)` rate are never silently skipped:
they are listed in `missing_rates`, the converted figures cover only the
convertible currencies, and `fx_incomplete` is `true` to mark the partial
summary. An invalid `base_currency` is a `400`; an empty body keeps the
legacy report (`fx_applied: false`, no summary key) so existing cron jobs
are unaffected.

```bash
curl -s -X POST localhost:8080/reconcile \
  -d '{"base_currency":"USD"}' | tee reconcile-$(date +%F).json
```

Trimmed summary shape:

```json
"fx_applied": true,
"base_currency_summary": {
  "base_currency": "USD",
  "total_debits_cents": 972,
  "total_credits_cents": 972,
  "conversions": [{"currency": "EUR", "total_debits_cents": 972, "total_credits_cents": 972}],
  "discrepancies": [],
  "fx_snapshot": [{"currency": "EUR", "rate_num": 108, "rate_den": 100, "rate_asof_version": 1}],
  "missing_rates": [],
  "fx_incomplete": false
}
```

#### Signed reconcile export (offline-verifiable)

`POST /reconcile/export` runs the same scan as `POST /reconcile` and
returns a signed package for compliance archival: the deterministic
report plus the anchor key's Ed25519 signature over its canonical bytes,
`{"report": {...}, "signature": "<hex>", "key_id": "<hex>"}`. The request
body is the same optional `{"base_currency": "..."}` as `POST /reconcile`.
A third party holding the operator's public key re-verifies the package
offline — no trust in the ledger's storage, no callback to the server:

1. Take the `report` bytes exactly as received.
2. `message = "LEDGER-RECONCILE-EXPORT/v1:" || report bytes`.
3. Ed25519-verify `signature` against `message`; check `key_id` is the hex
   of the public key's first 8 bytes.

The domain prefix keeps export signatures untransplantable into the
LG-44 anchor protocol and vice versa. The export is read-only (no
version bump, no chain link) and emits a `reconcile_export` audit event
naming the signing key. Without a configured `LEDGER_ANCHOR_KEY` the
endpoint fails closed with `500` — signing is a deployment
precondition, not a request problem.

```bash
curl -s -X POST localhost:8080/reconcile/export | tee reconcile-$(date +%F).signed.json
```

`ledger_reconcile_export_runs_total` counts export requests in `/metrics`.

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

### `GET /settlement`

The merchant settlement view (LG-43): the end-of-day question a payments
operator asks — for each merchant account, how much money moved today, in
which currency, and does the day's net match what the payment provider's
settlement file says it should be? Aggregation runs under a single read
lock like `POST /reconcile`, so the report describes one consistent point
in time.

- **Merchant dimension**: the account. Each `cells` row aggregates one
  `(account, UTC calendar day, currency)`: `debit_cents`, `credit_cents`,
  `net_cents` (debits − credits), and the `entry_ids` behind the row for
  drill-down, sorted by `(account, currency)`. There is no tag/metadata
  dimension on journal entries, so the channel dimension (e.g. `alipay`,
  `wechat-pay`) is structural config set in the `ledger` package:
  `SetSettlementChannel(account, channel)` (empty channel clears); it does
  not bump the ledger version and survives disaster-recovery snapshots.
  Journal entries marked `PendingReview` are excluded — they are booked
  but not effective, so including them would lie about moved money.
- **Reconciliation alerts**: pass the day's expected nets from the
  provider's settlement file (`?expected=merch-a:USD:2900`, repeatable) and
  opt-in tolerances (`?tolerance=merch-a:USD:100:50` — 100 cents absolute,
  50 bps relative, repeatable). Any `(account, currency)` whose actual
  day-net differs from the expected net beyond its tolerance lands in
  `mismatches` with `difference_cents` (actual − expected) and the
  tolerance that was exceeded. An expectation with no tolerance row uses
  zero tolerance: any nonzero difference alerts. Like `POST /reconcile`,
  alerts are always `200` — the findings live in the report, not the
  status code.
- **Exports**: `?format=csv` downloads the cells view (`day,account,
  channel,currency,debit_cents,credit_cents,net_cents,entry_count,
  entry_ids` with semicolon-joined entry IDs); `?format=sql` downloads a
  **SQL text dump** (`CREATE TABLE` + `INSERT` for `settlement_cells` and
  `settlement_mismatches`). The Go standard library has no SQLite driver,
  so this is deliberately a `.sql` text file, not a `.db` binary — import
  it with a real `sqlite3`:

```bash
curl -s 'localhost:8080/settlement?day=2026-10-10&format=sql' -o settlement.sql
sqlite3 settlement.db < settlement.sql
sqlite3 settlement.db 'SELECT account, currency, net_cents FROM settlement_cells ORDER BY 1,2;'
```

```bash
# JSON report for the day, with one expectation and a tolerance
curl -s 'localhost:8080/settlement?day=2026-10-10&expected=merch-a:USD:2900&tolerance=merch-a:USD:100:0' | head -40
```

Malformed `day` (`YYYY-MM-DD` required), `format`, `expected`, or
`tolerance` values are `400`; the currency codes and tolerance bounds in
`expected`/`tolerance` are validated by the ledger when the report runs
(`abs_cents >= 0`, `0 <= bps <= 10000`).

### `GET /aging`

The AR aging view (LG-50): the collections question a payments operator
asks — for each account that owes us money, how old is the outstanding
balance, in which currency, and who is sitting on overdue receivables.
The structural complement to the settlement view: settlement aggregates
one day's *movement*, aging decomposes the outstanding *stock* by age.

- **Unsettled = current balance**: the report attributes each
  `(account, currency)` balance to age buckets — `0-30`, `31-60`,
  `61-90`, `90+` days — measured from each entry's business timestamp
  (`CreatedAt`) to the report's as-of time, so backdated entries age by
  their business date. Only positive balances appear (negative-balance
  accounts are not receivables); `?account=` forces one account's row to
  appear regardless.
- **FIFO attribution**: the outstanding balance is attributed to the
  oldest entries first, and payments retire the oldest lots before newer
  ones — a 90-day-old invoice paid last week does not read as overdue.
  Bucket sums always add up to the current balance. Over-applied credit
  parks at the payment's own age and is consumed by later debits.
  Entries marked `PendingReview` are excluded: they are booked but not
  effective, so including them would break the bucket/balance invariant.
- **Overdue alerts**: overdue is the `61-90` + `90+` buckets.
  `?alert_overdue_cents=1000` flags every reported `(account, currency)`
  whose overdue cents exceed it — one `aging_alert` structured audit
  event per breaching account and one row in the report's `alerts` list;
  `ledger_aging_alerts_total` counts the hits. The threshold is per-call,
  not stored config (like settlement tolerances), and re-fires on every
  scan while in breach — aging is a point-in-time scan, not a crossing
  event. Zero or negative disables alerting. Like `/settlement`, alerts
  are always `200`.

```bash
# Who is overdue, as of a business date, alerting past 1000 cents
curl -s 'localhost:8080/aging?asof=2026-10-10T00:00:00Z&alert_overdue_cents=1000'
# One account's full decomposition
curl -s 'localhost:8080/aging?account=cust-123'
```

Malformed `asof` (RFC3339 required) or `alert_overdue_cents` (integer
required) are `400`.

### Multilateral netting (`ledger/netting.go`)

The clearinghouse view (LG-56): at cut-off time, given a set of
participant accounts with bilateral obligations already on the books,
what is the smallest set of transfers that settles everybody? A
participant's net position in a currency is its current net balance —
positive means net creditor (to receive), negative means net debtor (to
pay) — and netting runs per currency, isolated like the accounting
equation. The greedy plan repeatedly matches the largest creditor
against the largest debtor for `min(claim, debt)`; each leg zeroes at
least one participant, so the transfer count is always ≤ N−1 (N =
account count), with any leftover residual owed to or from parties
outside the set.

`NetPositions(accounts)` computes the positions (read-only);
`PlanNetting(sessionID, accounts)` builds the read-only plan;
`PostNetting(sessionID, accounts)` books every leg in one atomic batch
through the full Post gate order (validation → idempotency replay →
chart direction → frozen → overdraft → daily-limit → period), so any
failing leg rejects the whole session with zero bookings. The session
key is deterministic (`netting:<sessionID>`): reposting returns the
original `NettingReport` — before/after positions plus the per-leg
detail (from/to/amount/currency/entry ID) — with `duplicate: true` and
books nothing new.

```go
report, err := l.PostNetting("cutoff-2026-10-11", []ledger.AccountID{"merch-a", "merch-b", "merch-c"})
// report.Transfers: [{from_account:"merch-a", to_account:"merch-b", amount_cents:..., currency:"USD", entry_id:"cutoff-2026-10-11/net/USD/0"}, ...]
// report.Before / report.After: per-(account, currency) net positions around the settlement
```

### Disaster-recovery snapshots (`ledger/snapshot.go`)

The ledger can be exported to a JSONL disaster-recovery snapshot and
rebuilt from it. The export is a library-level API (no HTTP endpoint —
backups are an operator concern, taken from the process embedding the
ledger):

```go
var buf bytes.Buffer
if err := l.ExportSnapshot(&buf); err != nil { /* ... */ }
// buf now holds one JSON object per line:
//   {"record":"meta",...}                 exactly one, always first (carries merkle_root)
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
guards, sub-account hierarchy, transfer fee policy, idempotency TTL,
settlement channel mappings) comes
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

#### Balance Merkle inclusion proofs (`ledger/merkle.go`, `GET /accounts/{id}/merkle-proof`)

Snapshots anchor the balance sheet, not just the journal. At export
time the ledger builds a deterministic Merkle tree over every
`(account, currency) -> balance` leaf and writes the root into the
snapshot header (`meta.merkle_root`):

- **Leaves** are sorted by `(account, currency)`; each leaf encodes
  `u64be(len(account)) || account || u64be(len(currency)) || currency || i64be(balance_cents)`
  (length-prefixed so `"a"+"bc"` cannot alias `"ab"+"c"`).
- **Leaf hash**: `SHA-256(0x00 || leaf encoding)`; **inner nodes**:
  `SHA-256(0x01 || left || right)` — the `0x00`/`0x01` domain
  separators are second-preimage protection, so a leaf hash can never
  be mistaken for an inner hash.
- An odd level's last node pairs with itself (documented, not
  folklore); an empty ledger commits to the all-zero "genesis" root.
- Pure standard library (`crypto/sha256`) — no third-party crypto.

`GET /accounts/cash/merkle-proof[?currency=EUR]` returns the account's
inclusion proof: the committed balance, its leaf index, the sibling
hash path, and the root — everything an offline verifier needs:

```bash
curl -s localhost:8080/accounts/cash/merkle-proof
# {"account":"cash","currency":"USD","balance_cents":1000,"version":42,
#  "merkle_root":"9f2c...","leaf_index":3,"leaf":"...","leaf_encoding":"...",
#  "siblings":["ab12...","34cd...",...]}
```

The proof is verified with `ledger.VerifyMerkleProof(root, proof)`,
a pure function — no ledger, no network. A rewritten balance, a
spliced sibling, a swapped leaf index, or a wrong root all fail
verification, so a tampered snapshot is caught even by an auditor who
never sees the live ledger. Unknown accounts 404 (no committed
balance to prove); `?currency=` defaults to USD; proofs are counted
in `ledger_merkle_proof_queries_total`.

The root also travels with the snapshot version into the sealed
audit trail: the periodic backup worker's `snapshot_backup` audit
event carries `merkle_root` alongside `version_after`, so
`GET /audit/verify` proves exactly which root was committed for which
snapshot version. This rides the audit log's own hash chain — the
per-post journal chain (`VerifyChain`) is untouched: no new journal
link type, no changed link semantics.

On import, `ImportSnapshot` recomputes the root from the refolded
balances and rejects the snapshot (`ErrSnapshotInvalid`) when the
header's `merkle_root` does not match — a header rewritten after
export cannot be promoted. Snapshots written before this feature
carry no `merkle_root` and import unchanged (the check is skipped).

#### Incremental snapshots (`ledger/incremental.go`)

For bandwidth-efficient backup and replica sync, the ledger can export
only what changed since a base version:

```go
var delta bytes.Buffer
if err := l.ExportIncrementalSnapshot(&delta, baseVersion); err != nil { /* ... */ }
// delta holds:
//   {"record":"meta","base_version":N,...}   exactly one, always first
//   {"record":"entry",...}                   journal entries with seq > N, in chain order
//   {"record":"link",...}                    their audit-chain links
//   {"record":"hold",...}                    full holds section
//   {"record":"idempotency",...}             full idempotency registries (all five namespaces)
//   {"record":"config",...}                 current operational config (incl. FX rates)
```

The journal is the volume — that is what stays incremental. Holds,
registries, and config are compact state, always carried whole, which
keeps the merge rules simple: the replica applies the delta with
`ImportIncrementalSnapshot`:

```go
if err := replica.ImportIncrementalSnapshot(bytes.NewReader(deltaBytes)); err != nil {
    log.Fatalf("delta rejected: %v", err) // replica untouched — re-sync from its actual version
}
```

Continuity is the core invariant: the delta's `base_version` must equal
the replica's current version. A delta from an older version (overlap) or
a newer version (gap) is rejected with `ErrSnapshotInvalid` — a replica
that missed a delta re-syncs from the base it actually has, never skips.
New entries must be absent from the replica; the staged links are proven
to hash onto the replica's chain head before anything is committed (a
splice, reorder, or tampered entry fails there); holds and idempotency
rows merge (identical rows merge cleanly, conflicting rows reject the
import); and the config section carries the latest operational config.
The import finishes with a full audit-chain verification over old and new
links. Every section is validated before the first mutation, so a
rejected delta leaves the replica untouched. An empty delta
(`baseVersion == current version`) is legal and syncs config/registry
state with no new journal rows.

A full `ImportSnapshot` explicitly refuses a meta carrying `base_version`,
so the two snapshot kinds can never be confused.

#### Snapshot version diff (`ledger/snapshot_diff.go`, `GET /snapshots/diff`)

Two backup versions of the same ledger can be compared without
restoring either: `GET /snapshots/diff?from=<v1>&to=<v2>` loads both
versions from the backup directory (`LEDGER_SNAPSHOT_BACKUP_DIR`) and
returns the difference in three dimensions — per-(account, currency)
**balances**, journal **entries** by ID, and **frozen** risk-control
state (including timed-freeze deadlines) — each as added / deleted /
changed rows:

```bash
curl -s 'localhost:8080/snapshots/diff?from=120&to=168'
# {"from_version":120,"to_version":168,
#  "balances":{"added":[...],"deleted":[...],"changed":[{"account":"cash","currency":"USD","from_cents":1500,"to_cents":1800,"delta_cents":300}]},
#  "entries":{"added":[...],"deleted":[],"changed":[]},
#  "frozen":{"added":[],"deleted":[{"account":"acct-f"}],"changed":[]}}
```

Version resolution reuses the backup files directly: a full snapshot at
exactly the requested version is imported, otherwise the newest full
snapshot at or below the version is loaded and incremental deltas are
applied in continuity order until the version is reached — the same
`ImportIncrementalSnapshot` gap/overlap rules apply, so a version no
backup chain can construct is a `404` (`ErrSnapshotVersionNotFound`),
never a guessed state. The diff is purely read-side: both versions are
loaded into fresh in-memory ledgers, so nothing is written to the live
ledger, the audit log, or the backup directory. Served diffs are
counted in `ledger_snapshot_diffs_total`. Unconfigured backup directory
→ `404` fail-closed; missing or non-numeric `from`/`to` → `400`.

### Multi-tenant isolation (`ledger/tenant.go`)

One process can serve many isolated tenants (merchants, business units):
every tenant gets its own ledger — accounts, journal entries, idempotency
keys, the audit hash chain, freezes, limits, holds, and all other state
are namespaced by construction, so a reference that exists only in
another tenant (an account ID, an entry ID, an idempotency key) simply
does not exist in this tenant's ledger. Cross-tenant references behave
as not-found, never as a leak.

HTTP routing selects the tenant two ways (the path prefix wins when both
are given); a request that names no tenant hits the default tenant,
which keeps single-tenant deployments working exactly as before:

```bash
# path prefix …
curl -s -X POST localhost:8080/tenants/acme/entries -d '{"debit_account":"cash",...}'
# … or header
curl -s -H 'X-Tenant: acme' localhost:8080/accounts/cash/balance
# list provisioned tenants (the default tenant reports as "")
curl -s localhost:8080/tenants
# {"tenants":["","acme","globex"]}
```

Tenants are provisioned lazily on first use, built with the same options
the default ledger started with (TTLs, fee policies, review thresholds).
The structured audit log and the domain-event outbox stay process-wide
compliance streams — every event they carry is stamped with its `tenant`
(the default tenant's events carry no tenant field, exactly as before).
Snapshots round-trip per tenant (`TenantStore.Snapshot` /
`TenantStore.Restore`, independent per tenant), `POST /reconcile` reports
per tenant, and `GET /metrics` aggregates ledger counters across tenants
plus `ledger_tenants_total`. Background workers (sweepers, snapshot
backups) keep operating on the default tenant.

Tenant IDs are 1–64 chars of `[A-Za-z0-9._-]`; anything else is a 400
before any ledger is touched.

### Structured audit log (`ledger/audit.go`)

Every mutating operation (`POST`, `POST /transfers`, `POST /sweeps`,
`POST /merges`, holds, freeze/unfreeze) and every `POST /reconcile` run
appends one JSONL event to a configured directory — the compliance trail
of who did what, when, and what changed. Disabled by default; set
`LEDGER_AUDIT_DIR` to enable.

```json
{"ts":"2026-10-09T04:36:23.592704067Z","op":"merge","actor":"PostMerge",
 "trace_id":"merge-2026-10-09","version_before":1,"version_after":2,
 "entry_ids":["merge-2026-10-09/USD"],"accounts":["old-corp","new-corp"],
 "details":{"legs":1},
 "prev_hash":"0000000000000000000000000000000000000000000000000000000000000000",
 "hash":"9f2c…"}
```

- `prev_hash` / `hash` — the tamper-evident hash chain (LG-32): every
  event is sealed at enqueue time with the previous event's hex SHA-256
  (`prev_hash`) and its own seal
  `hash = SHA-256(canonical(entry) || prev_hash)` (standard library
  only; the first entry links the all-zero genesis marker). The link is
  assigned under a mutex at the single ordered enqueue point, so the
  async writer's flush order can never break the chain; a new file's
  first entry points at the previous file's last seal, so rotation and
  gzip archiving carry the chain across files unchanged, and a restart
  recovers the head from the newest sealed entry on disk. Any rewrite,
  deletion, or reorder breaks recomputation — verify with
  `GET /audit/verify`.
- `op` — the operation (`post`, `transfer`, `sweep`, `merge`, `hold`,
  `hold_capture`, `hold_release`, `hold_expire`, `freeze`, `unfreeze`,
  `frozen_auto_released`, `reconcile`).
- `actor` — the in-process ledger entrypoint that performed it
  (`Post`, `PostTransfer`, …). HTTP handlers call through these
  entrypoints; correlate with the HTTP access log by timestamp for
  client attribution.
- `trace_id` — correlates related records: the entry ID for a post, the
  transfer/sweep/merge ID for a compound operation (all of its legs share
  it), the hold ID for hold operations, the account for a freeze, the
  reconciled version for a reconcile run.
- `version_before` / `version_after` — the ledger version bracket around
  the operation. Read-only operations (freeze, reconcile) report a flat
  bracket — the event still proves the log line was written against a
  known ledger state.
- `entry_ids` — the journal entries the operation committed, in commit
  order; empty for operations that book nothing.

Writes are asynchronous and never block the ledger: operations enqueue
into a bounded channel (default 4096, `WithAuditLogQueueSize`), a
background goroutine appends JSONL — one `Write` syscall per line, so
lines are never torn. When the queue is full the event is dropped and
counted (`ledger_audit_dropped_total` — alert on it); a dead disk can
never stall posting. Reads never touch the log, so the read path is
unaffected.

Files rotate daily (UTC, `audit-2006-01-02.jsonl`) and on the size cap
(default 100 MiB, `LEDGER_AUDIT_MAX_BYTES`); rotated files are gzipped in
the background (`audit-2006-01-02.jsonl.gz`). Filenames are never reused,
so a `.jsonl` and its `.gz` twin always carry identical events.
`ledger.ReadAuditLog(dir, "2006-01-02")` reads a day's files (plain and
gzipped, in order) and skips malformed lines — a damaged audit file never
fails the read; corrupt lines are counted and reported.

**Per-line HMAC signatures (LG-65).** The hash chain proves ordering and
continuity; per-line HMAC-SHA256 proves each line was written by someone
holding the operator key (standard library only). Set
`LEDGER_AUDIT_SIGNING_KEYS` to enable:

```
LEDGER_AUDIT_SIGNING_KEYS="k1:001122…ff,k2:aabb…cc@2026-10-01T00:00:00Z"
```

— comma-separated `id:hexkey` entries with an optional `@<retiredAt
RFC3339>` suffix for rotated-out keys. The first non-retired key signs
new lines; every sealed line carries `{"kid":"k1","mac":"…"}`, the MAC
covering the line's canonical bytes (the key id is domain-separated into
the MAC input, so a signature cannot be transplanted by swapping the kid
header). A retired key still verifies lines written inside
`LEDGER_AUDIT_SIGNING_GRACE` after its retirement (a Go duration string,
default 24h). An invalid value fails startup fast — a misconfigured
signing key never silently runs.

`ledger.VerifyAuditSignatures(dir, keys, grace)` is the offline
verification tool: it replays every audit file (all days, oldest first,
plain and `.gz`) with no ledger instance running, and reports
`{ok, files_checked, lines_checked, signed_lines, unsigned_lines,
bad_lines}` — every bad line pinpointed by file, line number, trace id,
and reason (`corrupt` / `unknown_key` / `key_retired` /
`bad_signature`). Unsigned lines are counted, not failed: without
`LEDGER_AUDIT_SIGNING_KEYS` the log runs in degraded unsigned mode and
records an `audit_signing_disabled` alert as its first trail entry, so a
later verification can tell "never signed" from "signatures stripped".

### Domain event outbox (`ledger/outbox.go`) — `GET /events`

The audit log answers "who did what, when". The outbox answers "what
domain facts changed" — an append-only JSONL stream of the ledger's state
changes, for downstream consumers (indexers, notification fan-out,
warehouse loaders). Disabled by default; set `LEDGER_OUTBOX_DIR` to
enable. The two are deliberately decoupled: the audit log records every
operation (including config changes and dry runs that commit nothing);
the outbox records only committed domain facts.

Event envelope — `{seq,type,at,account?,entry_id?,amount_cents?,currency?,details?}`:

```json
{"seq":7,"type":"transfer_posted","at":"2026-10-11T05:56:34Z",
 "account":"alice","entry_id":"tx-2026-001","amount_cents":10000,
 "currency":"USD",
 "details":{"transfer_id":"tx-2026-001","to":"bob","fee_cents":25}}
```

- `seq` — process-global monotonic sequence, assigned inside the commit
  critical section, so seq order is commit order. It is the pagination
  cursor for `GET /events`.
- `type` — the domain fact: `entry_posted` (every committed journal row:
  posts, batches, transfer/fee/FX legs, rule-derived legs, sweep/merge
  legs, reversals, closing and revaluation entries), `transfer_posted`
  (the business envelope — legs first, envelope last, in seq order),
  `hold_created`, `hold_captured`, `hold_released`, `hold_expired`,
  `account_frozen`, `account_unfrozen`, `review_approved`,
  `review_rejected`, `review_expired`, `entry_reversed`.
- `account` — the event's primary account (debit account for
  `entry_posted`, payer for transfers, hold account for holds). The
  `?account=` query filter also matches either leg of an `entry_posted`
  event.
- `details` — event-specific facts (the second leg, memos, batch IDs,
  hold IDs, reviewer names, FX rate provenance). The envelope stays
  stable; details may grow.

Crash semantics — at-least-once, stated honestly: emission is
synchronous inside the commit critical section and each event is
fsync'd before the operation returns success, so a crash can never lose
a committed event (verified by a crash-simulation test that abandons the
outbox handle without closing it). Duplicates are possible on
caller retry across a crash or idempotency-key expiry — consumers must
dedupe on `(type, entry_id)` or tolerate replays. A disk failure at emit
time is counted (`ledger_outbox_dropped_total` — alert on growth) but
never fails the posting; seq gaps on read are the visible symptom.
Unlike the audit log, the outbox carries no hash chain: it is a consumer
stream, not tamper evidence.

Emission points mirror the operation's commit: idempotent replays emit
nothing (the original commit already emitted), pending-review transfers
emit at approval, dry runs emit nothing, and snapshot imports emit
nothing (a restore is not a domain event). Files are one per UTC day
(`outbox-2006-01-02.jsonl`), no size rotation; a restart continues the
seq from the highest value on disk. Corrupt lines are skipped and
counted on read. The fsync-per-event cost applies only when the outbox
is attached; the [Benchmarks](#benchmarks) numbers were measured without
it.

```bash
curl -s "localhost:8080/events?type=transfer_posted&limit=100"
# {"events":[{...}],"next_cursor":"…","corrupt_lines":0}
# 404 {"error":"domain event outbox disabled (LEDGER_OUTBOX_DIR unset)"}
#   — no LEDGER_OUTBOX_DIR configured; fail-closed.
```

`GET /events` query parameters: `since`/`until` (RFC3339, `until`
exclusive), `type` (comma-separated event types), `account`, `limit`
(1–1000, default 100), `cursor` (opaque; the response's `next_cursor`
resumes after it, empty on the last page). Invalid cursors/limits are
`400`.

#### Audit-sink webhook forwarder (`ledger/audit_sink.go`)

An opt-in forwarder that streams the outbox's domain events to an
external endpoint as signed JSON webhooks — for SIEM ingestion,
compliance mirrors, or cross-region replication. Disabled by default;
set `LEDGER_AUDIT_SINK_URL` (and the required `LEDGER_AUDIT_SINK_SECRET`,
see [Environment](#running)) to enable. Like the other background
workers it stays on the default tenant's ledger.

```bash
LEDGER_OUTBOX_DIR=/var/lib/ledger/outbox \
LEDGER_AUDIT_SINK_URL=https://siem.example.com/ledger-hook \
LEDGER_AUDIT_SINK_SECRET='<shared secret>' \
  ./ledger-api-go
```

Delivery contract, stated honestly:

- Every event POSTs as JSON with `Content-Type: application/json`,
  `X-Ledger-Signature: sha256=<hex>` (`HMAC-SHA256(secret, body)`),
  `X-Ledger-Event-Type`, and `X-Ledger-Event-Seq`. A `2xx` advances
  the cursor.
- The sink never touches the posting path: it tails the outbox files
  on its own goroutine, so a slow or dead webhook delays the cursor,
  never a `POST`.
- **At-least-once**: a crash between delivery and the cursor write
  replays the event after restart — downstream must dedupe on
  `(type, seq)`. The cursor persists atomically after each event
  (`<outboxdir>/audit-sink.cursor` by default).
- **First start begins at the current outbox head — history is not
  replayed.** The sink forwards new domain facts; it is not a backfill
  tool.
- **Gaps are possible by design**: an event that exhausts its 10
  retries (1s backoff doubling to 60s) is dead-lettered — recorded as
  an `audit_sink_dead_letter` audit event (with seq, type, attempts,
  last status), counted, and skipped; the cursor moves past it. Alert
  on `ledger_audit_sink_dead_letter_total`.
- A URL without a secret fails startup fast: unsigned webhooks can be
  forged, so the sink refuses to run unsigned. No outbox attached, an
  unavailable outbox directory, or a corrupt cursor file likewise fails
  startup fast rather than silently losing the stream.

`GET /metrics` exposes `ledger_audit_sink_sent_total`,
`ledger_audit_sink_failed_attempts_total` (network errors and non-2xx
responses, including dead letters' final attempts), and
`ledger_audit_sink_dead_letter_total`.

### `GET /audit/verify`

Replays the audit log's hash chain over every audit file — all days,
oldest first, current file plus history including `.gz` archives — and
reports whether it is intact. Each entry's `prev_hash` must equal the
previous entry's `hash`, and each `hash` must recompute from the entry's
canonical serialization; the first entry must link the all-zero genesis
marker. Verification stops at the first break (everything after a broken
link would cascade), and rotation boundaries verify like any other link
because the chain spans files by design.

```bash
curl -s localhost:8080/audit/verify
# {"ok":true,"checked_entries":128,"skipped_lines":0,"files_checked":3,
#  "head":"9f2c…","first_break":null}
# {"ok":false,...,"first_break":{"file":"audit-2026-10-09.jsonl","line":42,
#  "entry_id":"tr-7","expected_prev":"ab…","actual_prev":"cd…",
#  "reason":"prev_mismatch"},...}
```

- `200 {"ok":true,…}` — the chain is intact; `head` is the last seal.
- `200 {"ok":false,"first_break":{…}}` — the chain is broken; `reason`
  is `hash_mismatch` (entry edited in place), `prev_mismatch` (entry
  deleted / files reordered), or `missing_hash` (seal stripped). Like
  `POST /reconcile`, an unhealthy finding is still a 200 — the findings
  live in the body. `skipped_lines` discloses corrupt lines, which are
  skipped, never breaks.
- `404 {"error":"audit log disabled"}` — no `LEDGER_AUDIT_DIR`
  configured; fail-closed.
- Counted by `ledger_audit_verify_total` /
  `ledger_audit_verify_breaks_total` — alert on any nonzero breaks.

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
- `ledger_fx_transfers_total` — `POST /transfers` requests that attempted
  a cross-currency transfer, including rejected ones.
- `ledger_period_rejections_total` — journal-writing requests rejected
  with `422` because the entry's timestamp fell in a closed accounting
  period (`POST /entries`, `POST /entries/batch`, `POST /transfers`,
  `POST /sweeps`, `POST /merges`, `POST /holds/{id}/capture`).
- `ledger_dry_runs_total` — what-if dry-run requests received
  (`POST /entries/dry-run`, `POST /transfers/dry-run`,
  `POST /sweeps/dry-run`), including would-be rejections.
- `ledger_sweeps_total` — every `POST /sweeps` request received.
- `ledger_sweep_idempotency_hits_total` — sweeps that replayed an existing
  idempotency key (returned the original receipt, booked nothing).
- `ledger_merges_total` — every `POST /merges` request received.
- `ledger_merge_idempotency_hits_total` — merges that replayed an existing
  idempotency key (returned the original receipt, booked nothing).
- `ledger_reversals_total` — reversal journal entries committed by
  `POST /entries/{id}/reverse` (idempotent replays book nothing new and are
  not counted twice).
- `ledger_revaluations_total` — revaluation journal entries committed by
  `POST /revaluations` (one per account/currency whose carrying value
  moved; no-op runs move it by zero).
- `ledger_batch_total` — every `POST /entries/batch` request received.
- `ledger_batch_idempotency_hits_total` — batches that replayed an
  existing batch idempotency key (returned the original receipt, booked
  nothing).
- `ledger_audit_events_total` — audit-log events written to disk (synced
  from the ledger's audit log on every scrape).
- `ledger_audit_dropped_total` — audit-log events dropped because the
  async queue was full or the log was closed. A growing value means the
  disk cannot keep up — alert on it.
- `ledger_outbox_events_total` — domain events durably written to the
  outbox (synced from the ledger on every scrape).
- `ledger_outbox_dropped_total` — outbox events dropped because the
  outbox was closed or a disk write/fsync failed. A growing value means
  the stream has gaps — alert on it.
- `ledger_audit_verify_total` — `GET /audit/verify` requests that
  executed a full hash-chain verification of the audit log.
- `ledger_audit_verify_breaks_total` — verifications that found a broken
  audit-log hash chain. Any nonzero value is an integrity incident —
  alert on it.
- `ledger_balance_queries_total` — `GET /accounts/{id}/balance` requests
  served. Snapshot reads are not counted.
- `ledger_balance_at_queries_total` — `GET /accounts/{id}/balance-at`
  requests served.
- `ledger_ofx_exports_total` — `GET /accounts/{id}/ofx` OFX statement
  exports served.
- `ledger_ofx_imports_total` — `POST /ofx/import` OFX statement
  pre-posting imports served, including duplicates and rejected
  payloads.
- `ledger_statement_queries_total` — `GET /accounts/{id}/statement`
  JSON account statement requests served.
- `ledger_audit_sink_sent_total` — outbox domain events the audit-sink
  webhook forwarder delivered (HTTP 2xx).
- `ledger_audit_sink_failed_attempts_total` — failed audit-sink webhook
  delivery attempts (network errors and non-2xx responses).
- `ledger_audit_sink_dead_letter_total` — events the audit sink gave up
  on after exhausting retries (each recorded as an
  `audit_sink_dead_letter` audit event).
- `ledger_account_closes_total` — `POST /accounts/{id}/close` requests
  that closed an account (idempotent replays are not counted: they close
  nothing new).
- `ledger_closed_rejections_total` — the same set of write requests as
  `ledger_frozen_rejections_total`, rejected with `403` because an
  account involved was closed.
- `ledger_verify_requests_total` — `GET /entries/verify` requests served.
- `ledger_reconcile_runs_total` — `POST /reconcile` requests served.
- `ledger_reconcile_export_runs_total` — `POST /reconcile/export`
  requests served (signed reconcile exports).
- `ledger_frozen_rejections_total` — `POST /entries`, `POST /transfers`,
  `POST /sweeps`, `POST /merges`, and hold requests rejected with `403`
  because an account was frozen.
- `ledger_overdraft_rejections_total` — `POST /entries`, `POST
  /transfers`, `POST /merges`, and capture requests rejected with `422`
  because the posting would have overdrawn an overdraft-protected account.
- `ledger_daily_limit_rejections_total` — `POST /entries` and
  `POST /transfers` requests rejected with `422` because the posting would
  have taken the account's UTC-day cumulative outflow above its configured
  daily outflow limit.
- `ledger_budget_rejections_total` — `POST /entries`,
  `POST /entries/batch`, `POST /transfers`, `POST /sweeps`, and
  `POST /holds/{id}/capture` requests rejected with `422` because the
  posting would have taken the account's UTC-calendar-month cumulative
  spend above its configured monthly spend budget.
- `ledger_low_balance_breaches_total` — low-balance alert events emitted
  (advisory; the alert never rejects a posting). Synced from the ledger on
  every scrape, like the audit-log counters.
- `ledger_period_closes_total` — every `POST /periods/{id}/close` request
  received, including rejected ones and plain closes without closing
  entries.
- `ledger_closing_entries_total` — period-closing journal entries
  committed by `POST /periods/{id}/close?closing=true`.
- `ledger_balance_health_violations_total` — balance-health violations
  found by the background health watchdog (advisory integrity signal).
  Synced from the ledger on every scrape. Any nonzero value is an
  incident — alert on it.
- `ledger_fx_rate_stale_total` — `fx_rate_stale` alerts edge-triggered
  by the background FX-rate watch (rates that entered the opt-in warning
  window). Synced from the ledger on every scrape.
- `ledger_fx_rate_expired_total` — `fx_rate_expired` alerts
  edge-triggered by the background FX-rate watch (rates whose expiry
  has passed; transfers for the pair are rejected until a fresh rate is
  installed). Synced from the ledger on every scrape.
- `ledger_fx_rate_overridden_total` — cross-currency transfers converted
  at an approved manual rate override instead of the market rate
  (`fx_rate_overridden` audit events). Synced from the ledger on every
  scrape.
- `ledger_fx_rate_refreshes_total` — market FX rate quotes installed by
  the opt-in provider adapter (`WithFXRateProvider`) through the
  `SetFXRateRat` write path. Synced from the ledger on every scrape.
- `ledger_fx_rate_moved_total` — provider refreshes that changed a
  pair's price (`fx_rate_moved` audit events). Synced from the ledger on
  every scrape.
- `ledger_fx_rate_provider_failed_total` — market FX rate provider fetch
  failures (`fx_rate_provider_failed` audit events); the watch worker
  continues on the next tick. Synced from the ledger on every scrape.
- `ledger_fx_rate_inconsistency_total` — `fx_rate_inconsistent` alerts
  edge-triggered by the background FX-consistency watch (directed
  3-cycles of the rate table whose exact product deviates from 1 beyond
  the opt-in basis-point threshold; advisory only, conversions are never
  blocked). Synced from the ledger on every scrape.
- `ledger_balance_history_snapshots_total` — balance-history snapshot
  ticks completed by the background recorder (see
  `LEDGER_BALANCE_HISTORY_DIR` below). One per tick, not per row — it
  measures that the time series is alive, not its volume.
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
- `ledger_snapshot_backups_total` — successful periodic snapshot backup
  ticks (full and incremental; see `LEDGER_SNAPSHOT_BACKUP_DIR` below).
- `ledger_snapshot_backups_failed_total` — periodic snapshot backup
  ticks that failed after retries.
- `ledger_snapshot_diffs_total` — `GET /snapshots/diff` requests served
  successfully (read-only version diffs between backup snapshots).
- `ledger_merkle_proof_queries_total` — `GET /accounts/{id}/merkle-proof`
  requests served successfully (balance Merkle inclusion proofs).
- `ledger_settlement_runs_total` — `GET /settlement` requests served
  (all formats: `json`, `csv`, `sql`).
- `ledger_settlement_alerts_total` — settlement mismatch alerts across
  served settlement reports: the day-end breaks an operator works
  through, not posting rejections.
- `ledger_aging_alerts_total` — AR aging overdue alerts across served
  `GET /aging` reports: accounts whose 61-90 + 90+ day overdue cents
  exceeded the report's alert threshold.

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
- `LEDGER_FREEZE_SWEEP_INTERVAL` — enables the background freeze-expiry
  worker, e.g. `LEDGER_FREEZE_SWEEP_INTERVAL=1m`. While enabled, the
  server releases timed account freezes whose auto-unfreeze deadline has
  passed on the interval (the same `ExpireFrozenAccounts` sweep; every
  release is a `frozen_auto_released` audit event and bumps
  `ledger_freeze_auto_releases_total`). The first tick fires after one
  full interval; SIGINT/SIGTERM stops the worker with the server. Unset
  or invalid means disabled — release stays lazy on the write path.
- `LEDGER_SCHEDULE_SWEEP_INTERVAL` — enables the background
  transfer-schedule worker, e.g. `LEDGER_SCHEDULE_SWEEP_INTERVAL=1m`.
  While enabled, the server fires due transfer schedules on the interval
  (the same `SweepDue` scan: idempotent per-run keys, failed runs
  retried next tick, missed runs skipped) and counts every fired run in
  `ledger_scheduled_transfers_total`. The first tick fires after one
  full interval; SIGINT/SIGTERM stops the worker with the server. Unset
  or invalid means disabled — plans stay registered but never fire until
  the worker is enabled.
- `LEDGER_REPORT_SWEEP_INTERVAL` — enables the background report-schedule
  worker, e.g. `LEDGER_REPORT_SWEEP_INTERVAL=1h`. While enabled, the
  server generates due report subscriptions on the interval (the same
  `SweepReportsDue` scan: read-only generation, failed deliveries
  retried next tick, missed runs skipped). The first tick fires after one
  full interval; SIGINT/SIGTERM stops the worker with the server. Unset
  or invalid means disabled — subscriptions stay registered but never
  generate until the worker is enabled.
- `LEDGER_HEALTH_WATCH_INTERVAL` — enables the background balance-health
  watchdog, e.g. `LEDGER_HEALTH_WATCH_INTERVAL=5m`. Each tick re-checks
  the invariants the write path promises — negative balances on
  overdraft-protected accounts (the only accounts the write path
  guarantees can never go negative; credit-normal accounts legitimately
  carry negative balances), an accounting-equation spot check over a
  rotating quarter of the (account, currency) keys (full coverage every
  four ticks), and audit-chain/version continuity
  (`len(chain) == version`, head seq in step). Every break is recorded
  as a structured audit event (`balance_health_violation`) and counted
  in `ledger_balance_health_violations_total`. The scan is read-only:
  the watchdog never writes to the books — it verifies the write-path
  gates held, after bugs, bad restores, or config drift. The first tick
  fires after one full interval; SIGINT/SIGTERM stops the worker with
  the server. Unset or invalid means disabled.
- `LEDGER_FX_RATE_WATCH_INTERVAL` — enables the background FX-rate
  expiry watch, e.g. `LEDGER_FX_RATE_WATCH_INTERVAL=5m`. Each tick scans
  the rate table, read-only, and edge-triggers `fx_rate_stale` when a
  rate enters the warning window and `fx_rate_expired` once it crosses
  expiry, counted in `ledger_fx_rate_stale_total` /
  `ledger_fx_rate_expired_total`. The first tick fires after one full
  interval; SIGINT/SIGTERM stops the worker with the server. Unset or
  invalid means disabled.
- `LEDGER_FX_RATE_WARNING_THRESHOLD` — the opt-in stale-warning window
  for the FX-rate watch, e.g. `LEDGER_FX_RATE_WARNING_THRESHOLD=72h`.
  Unset or invalid disables stale warnings only — expiry alerts stay
  active.
- `LEDGER_FX_CONSISTENCY_INTERVAL` — enables the background FX
  triangular-consistency watch, e.g. `LEDGER_FX_CONSISTENCY_INTERVAL=5m`.
  Each tick scans the rate table, read-only, and edge-triggers
  `fx_rate_inconsistent` for directed 3-cycles whose exact product
  deviates from 1 by more than `LEDGER_FX_CONSISTENCY_THRESHOLD_BPS`
  (integer basis points), counted in
  `ledger_fx_rate_inconsistency_total`. Advisory only — conversions are
  never blocked. The first tick fires after one full interval;
  SIGINT/SIGTERM stops the worker with the server. Unset or invalid
  interval disables the watcher; unset or invalid threshold disables the
  check (the threshold is opt-in).
- `LEDGER_SNAPSHOT_BACKUP_DIR` — enables the periodic snapshot backup
  worker (LG-39): the server exports disaster-recovery snapshots to this
  directory on `LEDGER_SNAPSHOT_BACKUP_INTERVAL` (e.g. `1h`). Every
  `LEDGER_SNAPSHOT_BACKUP_FULL_EVERY`-th backup (default 6) is a full
  snapshot, the rest are incremental deltas from the last successful
  backup; the worker keeps the newest `LEDGER_SNAPSHOT_BACKUP_KEEP_FULL`
  (default 7) full and `LEDGER_SNAPSHOT_BACKUP_KEEP_INCR` (default 24)
  incremental files and deletes older ones. Writes are atomic (temp file
  + rename), exports retry on failure, every successful backup is
  recorded in the audit log (`snapshot_backup`), and ticks are counted
  in `ledger_snapshot_backups_total` /
  `ledger_snapshot_backups_failed_total`. Unset dir or invalid interval
  means disabled.
- `LEDGER_BACKUP_ENCRYPTION_KEY` — opt-in AES-256-GCM at-rest encryption
  for snapshot backup files (full and incremental alike): 64 hex chars
  for a single key, or `kid:hex,kid:hex,...` for a rotation ring (first
  entry is the active encryption key, the rest are retired
  decrypt-only keys, so old backups keep opening after a rotation).
  Each file carries a `LEDSNAPENC1` envelope: JSON header (key id,
  random nonce, authenticated alongside the ciphertext) + raw GCM
  ciphertext; tampered files fail loudly and are refused on import.
  The envelope is content-detected, so encrypted and plaintext backups
  coexist in one directory and `GET /snapshots/diff` decrypts
  transparently with the same key ring. With no key configured the
  worker writes plaintext and emits a one-time
  `snapshot_backup_unencrypted` audit warning — fail-open, documented
  honestly — unless `LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED=1`
  (`true`/`yes` also work), which makes the tick fail instead with a
  `snapshot_backup_encryption_missing` audit event.
- `LEDGER_BALANCE_HISTORY_DIR` (+ `LEDGER_BALANCE_HISTORY_INTERVAL`,
  `LEDGER_BALANCE_HISTORY_MAX_AGE_MS`) — enables the background
  balance-history recorder, e.g. `LEDGER_BALANCE_HISTORY_INTERVAL=5m`.
  While enabled, the server samples every (account, currency) balance on
  the interval and appends one row per key to append-only per-day JSONL
  files (`balance-history-YYYY-MM-DD.jsonl`) in this directory, readable
  via `GET /balance-history`. Files older than
  `LEDGER_BALANCE_HISTORY_MAX_AGE_MS` (default 90 days, in milliseconds)
  are pruned on every tick. Sampling is read-only — the recorder holds
  the read lock only for the capture, writes nothing to the books, and
  never bumps the version; failed ticks are logged and retried next
  tick. Successful ticks are counted in
  `ledger_balance_history_snapshots_total`. The directory is process
  configuration, not ledger state: after a disaster-recovery snapshot
  restore, pointing the new process at the same directory resumes the
  series — old rows are never rewritten. The first tick fires after one
  full interval; SIGINT/SIGTERM stops the worker with the server. Unset
  dir or invalid interval means disabled.
- `LEDGER_TRANSFER_FEE` — default transfer fee policy. Flat form
  `"<rateBps>:<revenueAccount>"` (e.g. `LEDGER_TRANSFER_FEE="250:fee-revenue"`
  for 2.5%), or tiered form `"<minCents>:<rateBps>,...@<revenueAccount>"`
  (e.g. `"0:0,10000:250,1000000:100@fee-revenue"`). Applies to
  `POST /transfers` without an explicit fee unless the request sets
  `skip_fee`. Unset means no default fee; an invalid value fails startup
  fast.
- `LEDGER_DAILY_OUTFLOW_LIMITS` — per-account per-currency daily outflow
  limits, `"<account>:<currency>:<limitCents>,..."` (e.g.
  `LEDGER_DAILY_OUTFLOW_LIMITS="cust-123:USD:100000"` caps cust-123's daily
  USD outflow at $1000.00). Postings that would exceed the day's budget
  are rejected with `422`. Unset means no limits; an invalid value fails
  startup fast.
- `LEDGER_MONTHLY_SPEND_BUDGETS` — per-account per-currency monthly spend
  budgets, `"<account>:<currency>:<budgetCents>,..."` (e.g.
  `LEDGER_MONTHLY_SPEND_BUDGETS="cust-123:USD:500000"` caps cust-123's
  monthly USD spend at $5000.00). Postings that would take the account's
  UTC-calendar-month cumulative spend above its budget are rejected with
  `422`. Unset means no budgets; an invalid value fails startup fast.
- `LEDGER_LOW_BALANCE_THRESHOLDS` — per-account per-currency low-balance
  alert levels, `"<account>:<currency>:<thresholdCents>,..."` (e.g.
  `LEDGER_LOW_BALANCE_THRESHOLDS="cust-123:USD:50000"` alerts when
  cust-123's USD balance falls below $500.00). The alert is advisory: the
  first posting that crosses the level emits one `low_balance_breach`
  audit event, counted in `ledger_low_balance_breaches_total`. Unset means
  no alert levels; an invalid value fails startup fast.
- `LEDGER_REVIEW_THRESHOLD` — ledger-wide large-transfer dual-control
  threshold in cents (e.g. `LEDGER_REVIEW_THRESHOLD="100000"` for
  $1,000.00): transfers at or above it enter `pending_review` instead of
  settling, until an operator approves (`POST /transfers/{id}/approve`)
  or rejects (`POST /transfers/{id}/reject`) them. Unset or `"0"` means
  disabled; an invalid value fails startup fast.
- `LEDGER_REVIEW_THRESHOLDS` — per-account review thresholds,
  `"<account>:<thresholdCents>,..."` (e.g.
  `LEDGER_REVIEW_THRESHOLDS="treasury:1000000,ops:500000"`); a per-account
  threshold wins over `LEDGER_REVIEW_THRESHOLD` for that account's
  outbound transfers. Unset means none; an invalid value fails startup
  fast.
- `LEDGER_REVIEW_EXPIRY` — how long a pending review may wait for a
  decision, e.g. `LEDGER_REVIEW_EXPIRY=24h`. Lapsed reviews are
  auto-rejected by `POST /transfers/reviews/expire` or the background
  worker. Unset means reviews never lapse; invalid values are ignored
  with a log line.
- `LEDGER_REVIEW_SWEEP_INTERVAL` — enables the background
  transfer-review expiry worker, e.g.
  `LEDGER_REVIEW_SWEEP_INTERVAL=5m`. While enabled, the server
  auto-rejects lapsed reviews on the interval and counts every expired
  review in `ledger_review_rejected_total`. The first tick fires after one
  full interval; SIGINT/SIGTERM stops the worker with the server. Unset
  or invalid means disabled — expiry stays lazy by predicate and
  available on demand via `POST /transfers/reviews/expire`.
- `LEDGER_FX_ACCOUNT` — the ledger-wide FX clearing account, counterparty
  of cross-currency transfer legs. Unset means each cross-currency
  transfer must carry `fx_account`.
- `LEDGER_FX_RATES` — the FX rate table, `"<from>:<to>=<num>/<den>,..."`
  (e.g. `LEDGER_FX_RATES="USD:EUR=108/100,USD:CNY=720/100"` for 1 USD =
  1.08 EUR and 1 USD = 7.20 CNY). Converting `X` source cents books
  `floor(X * num / den)` target cents. Rates are directional and take
  effect at ledger version 0. Unset means no rates: any cross-currency
  transfer is rejected with `422`; an invalid value fails startup fast.
- `LEDGER_REVAL_ACCOUNT` — the P&L account receiving the gain/loss legs of
  FX revaluations (`POST /revaluations`). Unset means revaluation is
  disabled: the endpoint is rejected with `400` until configured.
- `LEDGER_AUDIT_DIR` — enables the structured audit log (see above):
  every mutating operation and every reconcile run is appended as JSONL
  to this directory, with daily rotation and a per-file size cap. Unset
  means disabled. The directory is created if needed; an unwritable
  directory fails startup fast. The log is flushed on graceful shutdown.
- `LEDGER_AUDIT_MAX_BYTES` — caps a single audit file in bytes (default
  `104857600` = 100 MiB). Past the cap the writer rotates to a new file;
  rotated files are gzipped in the background. Unset or invalid means the
  default.
- `LEDGER_ANCHOR_KEY` — the Ed25519 key for external audit-chain
  anchoring (see above): hex of a 32-byte seed or a 64-byte private key.
  Checkpoints land in `LEDGER_AUDIT_DIR` as `checkpoints.jsonl`. Unset
  means unconfigured; an invalid value fails startup fast.
- `LEDGER_OUTBOX_DIR` — enables the domain-event outbox (see above):
  every committed state change is appended as JSONL to this directory,
  one file per UTC day, fsync'd per event. Unset means disabled. The
  directory is created if needed; an unwritable directory fails startup
  fast. The outbox is closed on graceful shutdown.
- `LEDGER_AUDIT_SINK_URL` — enables the audit-sink webhook forwarder
  (see above): outbox domain events POST as HMAC-signed JSON to this
  absolute http(s) URL. Unset means disabled.
- `LEDGER_AUDIT_SINK_SECRET` — the shared HMAC-SHA256 secret signing
  every webhook delivery (`X-Ledger-Signature: sha256=<hex>`).
  Required when `LEDGER_AUDIT_SINK_URL` is set: a URL without a secret
  fails startup fast, because unsigned webhooks can be forged. Never
  logged.

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
│   ├── ledger_reconcile_base_test.go # base-currency summary: conversion, missing rates, fx_applied, race
│   ├── ledger_bench_test.go   # BenchmarkPost: throughput + p99 latency (numbers → README)
│   ├── ledger_test.go         # validation, idempotency, concurrency tests
│   ├── hierarchy.go           # sub-account parent links (cycle-guarded) + per-currency balance rollup
│   ├── tags.go                # account tags (classification labels) + tag-dimension balance rollup
│   ├── ledger_tags_test.go    # tag validation/set/clear/copy isolation, tag rollup, snapshot round-trip, audit trail
│   ├── main_tags_test.go      # PUT /accounts/{id}/tags + GET /rollup?tag=k:v endpoints
│   ├── ledger_hierarchy_test.go# parent assignment, cycle rejection, rollup aggregation, concurrency
│   ├── ledger_idempotency_ttl_test.go# TTL eviction, lazy prune, interval guard
│   ├── ledger_snapshot_test.go# versioned snapshot semantics
│   ├── snapshot.go            # disaster-recovery snapshots: JSONL export / verified import (entries + chain + holds + idempotency registries + config)
│   ├── ledger_dr_test.go      # snapshot round-trip, tamper/splice rejection, reconcile-rerun parity
│   ├── audit.go               # structured compliance audit log: async JSONL writer, daily + size rotation, gzip, corrupt-line-skipping reader, hash-chain sealing at enqueue
│   ├── audit_chain.go         # SHA-256 audit-log hash chain: canonical seal, full-chain verifier (VerifyAuditLog), restart head recovery
│   ├── ledger_audit_test.go   # audit event shapes, rotation/gzip, corrupt-line skipping, read-path silence, concurrency
│   ├── ledger_audit_chain_test.go # hash-chain sealing/linking, tamper + deletion detection, rotation/gzip continuity, corrupt-line skipping, restart recovery
│   ├── merge.go               # PostMerge: atomic account merge (all currencies → target, debt absorption) + source freeze
│   ├── ledger_merge_test.go   # merge legs/freeze/idempotency/overdraft/TTL, snapshot + incremental round-trips
│   ├── timetravel.go          # BalanceAt: point-in-time balance at a ledger version (audit-chain prefix scan)
│   ├── ledger_timetravel_test.go# time-travel correctness, currency isolation, future-version rejection, concurrent readers
│   ├── balance_history.go       # balance time-series recorder: (account,currency) snapshots → append-only per-day JSONL, retention pruning, history query
│   ├── ledger_balance_history_test.go# snapshot/query/prune/worker/snapshot-restore continuity, concurrency
│   └── ledger_list_test.go    # cursor pagination, time windows, interleaved inserts
├── main.go                    # net/http JSON API (thin assembly only)
├── metrics.go                 # Prometheus-format /metrics counters (stdlib only)
├── metrics_test.go            # /metrics exposition + counter semantics tests
├── main_test.go               # HTTP handler tests (httptest)
├── main_timetravel_test.go    # GET /accounts/{id}/balance-at handler tests (httptest)
├── main_balance_history_test.go # GET /balance-history handler + env config + /metrics exposition (httptest)
├── main_reconcile_test.go     # POST /reconcile handler tests (httptest)
├── main_graceful_test.go      # SIGTERM drain: in-flight requests complete, listener closes
└── .github/workflows/ci.yml
```

## License

MIT
