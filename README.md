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

### API versioning

The API negotiates versions two ways, with identical semantics:

- **Path prefix:** `GET /v1/entries` (preferred for servers)
- **Accept header:** `Accept: application/vnd.ledger.v1+json` (preferred for client negotiation)

A request that names no version keeps the legacy behavior exactly: it is
served the latest stable version (currently `v1`) with the legacy
bare-JSON response shape, so existing integrations keep working. A
request that names a version explicitly gets the versioned envelope
`{"api_version": "v1", "data": ...}` around every JSON response body.
Non-JSON surfaces (Prometheus exposition on `/metrics`, CSV/SQL exports,
JSONL audit streams, SSE) keep their own content types and carry the
resolved version in the `X-API-Version` header instead. Naming an unknown
version (e.g. `/v2/entries`) or conflicting signals (path says `v1`,
Accept says `v9`) is a `406` listing the supported versions.

Deprecated endpoints stay fully functional until their sunset date but
answer with `Deprecation: true`, a `Sunset` date, and a
`Link: <successor>; rel="successor-version"` header:

| Endpoint | Sunset | Successor |
|---|---|---|
| `POST /entries` | 2027-06-30 | `POST /entries/batch` (a one-entry batch carries identical semantics and gate coverage) |
| `POST /transfers/reviews/approve` | 2027-06-30 | `POST /transfers/{id}/approve` (bulk approval bypasses per-transfer review scrutiny) |

```bash
curl -s localhost:8080/v1/entries?limit=1 | head -c 120
# {"api_version":"v1","data":{"entries":[...],"next_cursor":...}}
curl -s -D - -o /dev/null -X POST localhost:8080/entries -d '{}' | grep -i -E 'deprecation|sunset|link|x-api-version'
# Deprecation: true / Sunset: Wed, 30 Jun 2027 00:00:00 GMT /
# Link: </entries/batch>; rel="successor-version" / X-API-Version: v1
```

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
- `metadata`: optional structured metadata for the entry — downstream
  reconciliation tags like `channel=alipay` or `risk_score=12`. At most 16
  pairs; keys are 1–64 characters of `[A-Za-z0-9._-]`, values at most 256
  UTF-8 characters. It is journaled with the entry, covered by the
  audit-chain hash (keys sorted, deterministic encoding; rewriting a
  journaled tag breaks the chain exactly like rewriting an amount —
  entries without metadata hash exactly as before), carried through
  snapshots and idempotent replays, queryable via
  `GET /entries?meta.<key>=<value>` (composes with `?memo=`, `?ref=`,
  the time window, and cursor pagination), and echoed by dry-run legs.
  `POST /transfers` stamps it on the principal entry (the fee leg, FX
  conversion legs, and the spread leg carry none — on cross-currency
  transfers it rides the `/fx` leg, mirroring the memo); each
  `POST /entries/batch` leg carries its own; `POST /entries/{id}/reverse`
  takes the caller's metadata (the original's is **not** inherited);
  `POST /holds/{id}/capture` stamps it on the settlement entry. Internal
  and system-derived postings (journal-rule legs, accruals, dividends)
  carry none. Anything else — more than 16 pairs, a malformed key, or an
  overlong value — is rejected with `400` before anything is recorded.

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

### `POST /transfers/batch`

Atomic bulk transfers: one request commits several transfers (a payroll
run, a merchant batch settlement) — either every transfer lands or none
do (Go: `ledger.PostBatchTransfer`). Each transfer is a full
`POST /transfers` (fee resolution, FX conversion, idempotency replay,
frozen/overdraft/daily-limit/period risk controls, dual-control review);
the batch is the unit of atomicity.

```bash
curl -s -X POST localhost:8080/transfers/batch \
  -H 'Content-Type: application/json' \
  -d '{"batch_id":"payroll-2026-10","idempotency_key":"btk-001","transfers":[
    {"transfer_id":"t-001","from_account":"payroll","to_account":"alice","amount_cents":500000,"currency":"USD"},
    {"transfer_id":"t-002","from_account":"payroll","to_account":"bob","amount_cents":450000,"currency":"USD","fee_cents":200,"fee_account":"fees"}
  ]}'
# 201 {"batch_id":"payroll-2026-10","receipts":[{"transfer_id":"t-001","entries":[...]}, ...],"duplicate":false}
```

- Atomicity is two-phase: every transfer first runs the exact
  production code path on a private deep copy of the ledger (the LG-36
  dry-run twin pattern), in batch order, under the single write lock.
  Any rejection — validation, denylist, ID conflict, frozen,
  overdraft, daily-limit, budget, period, FX — aborts the whole batch
  with zero records. Only when every transfer commits on the twin does
  the live ledger commit them. A twin-accepted transfer is
  live-accepted: there is never a half-committed batch to roll back.
- First-time batch → `201` with the receipt (per-transfer receipts with
  leg detail: principal + fee + FX legs). The server generates
  `batch_id` and per-transfer `transfer_id` values when the client
  omits them.
- Re-post with the same batch `idempotency_key` → `200` with the
  original receipt (`duplicate: true`); nothing is booked again. The
  key lives in its own namespace (`transfer-batches` in
  `GET /idempotency-keys`), expires with the idempotency TTL, and
  survives disaster-recovery snapshots (full and incremental).
- Each transfer's own `idempotency_key` shares the `POST /transfers`
  key namespace: a key already posted resolves to the originally posted
  receipt (returned in the batch receipt, booked nothing); a batch may
  freely mix fresh transfers and replays.
- Invalid batch (empty batch, bad transfer, duplicate transfer IDs in
  the batch, or a duplicated new transfer key) → `400`. A transfer
  through a **frozen** account → `403`; a transfer that would overdraw
  an **overdraft-protected** payer, breach a **daily outflow limit**, hit
  a **closed period**, or miss an **FX rate** → `422`.
- A transfer at or above its payer's review threshold journals with the
  pending-review marker, atomically with the settled ones — approve or
  reject it with the usual review endpoints.
- The batch emits one `post_transfer_batch` audit event; each
  transfer's own audit events, metrics, balance hooks, and outbox
  events fire exactly as `POST /transfers` would emit them.
- Attempts and replays are counted in `ledger_transfer_batch_total`
  and `ledger_transfer_batch_idempotency_hits_total`.

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
  its **daily outflow limit**. The payer has a **transfer beneficiary
  allowlist** installed and the payee is not on it → `403` (see
  `PUT /accounts/{id}/allowlist`). All are counted in the shared
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

#### Fee splits

Instead of one `fee_account`, a transfer can divide its fee across
several recipients with `fee_splits` — the payment-fintech revenue-share
shape (platform fee split between the marketplace and payment
partners). Each entry names a recipient and its basis-point share; the
shares must sum to **exactly 10000** bps. The split applies to the
finally resolved fee — an explicit `fee_cents` or the policy's charge —
and each recipient's share is `floor(fee * bps / 10000)` in
overflow-safe integer arithmetic, with the dust remainder landing on the
first recipient so the shares always sum to the fee exactly.

```bash
curl -s -X POST localhost:8080/transfers \
  -d '{"transfer_id":"tx-2026-003","from_account":"alice","to_account":"bob","amount_cents":10000,"fee_cents":101,
       "fee_splits":[{"account":"rev-marketplace","bps":7000},{"account":"rev-psp","bps":3000}]}'
# 201 {"transfer_id":"tx-2026-003","entries":[{...},{...},{...}],"fee_cents":101,
#      "fee_splits":[{"account":"rev-marketplace","bps":7000,"fee_cents":71},
#                    {"account":"rev-psp","bps":3000,"fee_cents":30}],"duplicate":false}
```

Each recipient gets its own fee leg (`debit recipient`, `credit
from_account`, id `<transfer_id>/fee/<i>`, memo
`fee-split:<i>/<n>:<account>:<bps>`) booked in the transfer's currency —
the fee stays in the source currency on cross-currency transfers. Every
leg walks the same gates as the single fee leg: the fee total is already
folded into the payer's total-outflow checks (amount + fee), and each
recipient gets the frozen/closed coverage the single fee account always
had. The split commits atomically with the principal — one failing leg
rejects the whole transfer and records nothing.

Rules: `fee_splits` replaces `fee_account` (the two are mutually
exclusive); it needs a fee to divide, so it is incompatible with
`skip_fee`, and a zero resolved fee with `fee_splits` set is rejected
(`400`) instead of silently dropping the recipients. Basis points must
be 1..10000 per recipient and sum to exactly 10000; a zero address or a
non-positive share is rejected. Fee splits are orthogonal to cost
allocations: allocation divides the principal, fee splits divide the
fee. They compose with `POST /transfers` (plain and cross-currency), not
with chunked split transfers.

The receipt discloses the per-recipient breakdown in `fee_splits`, and
idempotent replays return the same breakdown (rebuilt from the journaled
legs, nothing re-booked). The structured audit event for the transfer
carries the breakdown as `fee_splits` (`account`, `bps`, `fee_cents`
per recipient). Operators can query it any time:

```bash
curl -s localhost:8080/transfers/tx-2026-003/fee-split
# 200 {"transfer_id":"tx-2026-003","fee_cents":101,"splits":[...]}
```

A transfer that posted no fee split (no fee leg, or a single fee
account) returns `404` with a hint, like an unknown transfer ID.

#### Posting rules (event-driven derived legs)

Where the opt-in journal rules (`PUT /rules/{id}`) fire on raw `POST` entries, **posting rules**
fire on domain events — `transfer`, `capture`, and `sweep` — and append
derived journal legs that commit atomically with the event. The
payment-fintech automations: a channel fee on a merchant settlement
transfer, a tax withholding on a payout capture, a reserve on a treasury
sweep.

```go
l := ledger.New(ledger.WithPostingRules([]ledger.PostingRule{{
    Name:           "channel-fee",
    EventKind:      ledger.PostingEventTransfer, // "transfer" | "capture" | "sweep"
    AccountPrefix:  "merchant-",                 // payer must start with this (optional)
    MinAmountCents: 10000,                       // event amount must reach this (optional)
    Currency:       "USD",                       // event currency must equal this (optional)
    Legs: []ledger.PostingRuleLeg{{
        AccountTemplate: "channel-fee-revenue", // {payer} / {payee} interpolated from the event
        Side:            "debit",               // "debit": debit template, credit payer
        AmountExpr:      "bps:250",             // "bps:<1..10000>" or "fixed:<cents>"
    }},
}}))
```

Semantics:

- **Event context.** Transfer: payer `from_account`, payee
  `to_account`, amount the transfer amount, trigger the transfer ID.
  Capture: payer the hold account, payee the capture target, amount
  the captured cents, trigger the capture entry ID. Sweep: one
  evaluation per (source, currency) leg — payer the source, payee the
  sweep target, trigger the leg entry ID
  (`<sweep_id>/<source>/<currency>`).
- **Derived legs.** Each leg books its own journal entry: id
  `<trigger_id>/rule:<rule_name>` (`/<i>` appended when the rule
  defines several legs), `batch_id` = the trigger ID, memo
  `rule:<rule_name>`, in the event's currency. Side `debit` debits the
  resolved template account and credits the payer (the charge-on-top
  fee shape); side `credit` does the reverse.
- **Atomicity.** Rule legs are evaluated after every gate — including
  the dual-control review gate, so review-frozen transfers book no
  rule legs — and before the first mutation. A leg that fails any
  gate (frozen/closed/suspended account, overdraft on a protected
  payer, or an ID collision) rejects the whole event: no journal rows,
  no chain links, no version bump. Rule legs are excluded from the
  daily-outflow and monthly-spend accounting (the event's own gates
  already evaluated the principal outflow), and derived legs never
  re-trigger rules — one level deep, no cascades.
- **Receipt & audit.** Receipts (`transfer`, `capture`, `sweep`)
  disclose `rule_legs` (rule name, entry ID, accounts, cents);
  idempotent replays rebuild the same disclosure from the journaled
  `/rule:` IDs and book nothing new. The structured audit events
  carry `posting_rule_legs`. `POST /reconcile` lists the installed
  rules in `posting_rules`.
- **Config.** Rules are deployment config, like the fee policy:
  installed at construction, validated fail-fast (bad kind, unknown
  `{placeholder}`, `bps` outside 1..10000, duplicate names — all
  panic, mirroring the fee schedule), and round-tripped through the
  disaster-recovery snapshot config. With no rules installed the
  ledger posts exactly as before.
- **Metrics.** `ledger_posting_rule_legs_total{rule}` counts committed
  derived legs per rule on `GET /metrics`.

A zero-cent derived leg (dust under a low bps rate) books nothing,
mirroring the fee policy. A rule ID containing `/` is rejected so the
derived-leg IDs stay unambiguous.

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
  reserved outflow, expiry — plus each review's named approve/reject votes
  under a quorum policy) and every per-account threshold
  (`review_thresholds`), and discloses the active quorum policy
  (`review_quorum`); reviews survive disaster-recovery snapshots with
  their statuses and votes, and `GET /metrics` exposes
  `ledger_review_pending_total` / `ledger_review_approved_total` /
  `ledger_review_rejected_total` plus
  `ledger_review_quorum_votes_total{approver,vote,outcome}`.
- **Batch approve:** `POST /transfers/reviews/approve` approves many
  reviews in one operator call (`{"reviewer": "op-alice", "review_ids":
  ["t1", "t2"]}`) — every review runs through the same approve path as
  the single-review endpoint, with full per-item failure isolation
  (unknown IDs, non-pending reviews, frozen accounts, overdraft,
  closed periods fail only their own item) and per-item idempotency.
  The response carries each item's outcome (`settled` / `duplicate` /
  `vote_recorded` / `failed` with the reason) in input order; every
  batch also emits one sealed-audit envelope event
  (`transfer_reviews_batch_approved`) listing all item outcomes.
  `GET /metrics` exposes `ledger_review_batch_requests_total`;
  settled items additionally count in
  `ledger_review_approved_total`, recorded quorum/chain votes in
  `ledger_review_quorum_votes_total`.
- Configure with `LEDGER_REVIEW_THRESHOLD` (global, cents, e.g. `"100000"`
  for $1,000.00) and/or `LEDGER_REVIEW_THRESHOLDS`
  (`"<account>:<cents>,..."`, e.g. `"treasury:1000000,ops:500000"` — a
  per-account threshold wins over the global one for that account's
  outbound transfers). Unset means no review; invalid values fail startup
  fast.
- **N-of-M quorum (four-eyes, hardened):** `SetReviewQuorumPolicy` installs
  an opt-in approver set (M named approvers) plus an approval threshold N
  — a single stolen operator credential can no longer move money alone.
  The same `approve`/`reject` endpoints become named-vote entry points:
  an approve call records one vote (returning `200` with
  `review_status: "pending_review"` and the live `quorum_tally`) until N
  distinct approvers have voted, and the Nth vote settles through the
  normal approval path. A reject vote is decisive at once when the policy
  is `RejectDecisive`; otherwise it is recorded as dissent and the review
  stays pending (approval or expiry still terminates it). Only policy
  members may vote (`403` otherwise); re-voting the same way is idempotent
  and voting the opposite way returns `400`. Every vote — with the
  approver identity, the tally, and the outcome
  (`recorded`/`settled`/`decisive`/`duplicate`) — lands in the structured
  audit chain and on
  `ledger_review_quorum_votes_total{approver,vote,outcome}`. The policy is
  structural config (no version bump) and round-trips through
  disaster-recovery snapshots.
- **Sequential approval chain (tiered maker → checker → controller):**
  `SetApprovalChain` installs an opt-in ordered list of stages, each
  stage its own approver set (M) and threshold (N). The review advances
  stage by stage — the same `approve`/`reject` endpoints become
  per-stage vote entry points: an approve vote that reaches the stage
  threshold advances the review (`200`, `review_status:
  "pending_review"`, the live `chain_tally` with outcome `advanced`),
  and the last stage's threshold settles through the normal approval
  path. A **reject vote is decisive at every stage**: it closes the
  review immediately through the normal rejection path (the recorded
  vote stays on the review). Only current-stage members may vote
  (`403`); re-voting the same way is idempotent, voting the opposite
  way returns `400`. Every vote lands in the structured audit chain
  with its stage number (`transfer_review_chain_vote`), and each
  review's stage plus per-stage votes ride the review record, the
  `Reconcile` report (`approval_chain`), and disaster-recovery
  snapshots. Stages may carry an opt-in `Timeout`: a stage that waits
  longer auto-rejects the review (lazy by predicate, swept by the same
  expiry worker), so funds can never strand on a silent approver.
  While a chain is configured it supersedes the flat N-of-M policy.
- **Approver vote delegation (vote by proxy):** `DelegateVote` grants a
  delegatee the right to cast votes in the granter's place — on the
  flat review path, the quorum path, or the chain path (`scope:
  "review" | "quorum" | "chain"`), optionally narrowed to one review
  and/or bounded by an expiry (`RevokeDelegation` ends it early). A
  delegated vote counts exactly as the principal's own (the
  one-vote-per-approver rule applies to the principal), but the audit
  trail attributes both hands: the standard decision events carry
  `delegate` + `principal`, and a dedicated `vote_by_delegate` event
  records the delegation exercise. Delegation chains are allowed but
  cycles are rejected at grant time (`A→B→A` fails) so the "whose vote
  was it" question always has an answer. Delegations are structural
  config (no version bump) and round-trip through disaster-recovery
  snapshots — including the ever-granted counter behind
  `ledger_review_delegations_total` — with `delegation_granted` /
  `delegation_revoked` in the audit chain. The fintech use case: an
  approver on leave delegates their four-eyes vote to a colleague
  instead of the payment stalling in dual-control limbo.
- **N-of-M Ed25519 multi-signature approval (cryptographic quorum):**
  `RegisterMultisigApprover` binds an approver ID to an Ed25519 public
  key and `SetMultisigPolicy` installs a per-operation-type (`transfer`
  / `payout` / `reversal`) N-of-M policy. `SubmitMultisigOperation`
  parks a large operation in the `pending_multisig` queue and freezes
  its funds with a hold (available drops, journal untouched); approvers
  vote with `VoteMultisig`, each vote an Ed25519 signature over the
  operation's canonical bytes (domain-separated, binding op ID, amount,
  payee, expiry, and vote direction — a signature cannot be transplanted
  to another operation or replayed for the opposite vote). N distinct
  approve signatures execute the operation through the standard write
  paths (`PostTransfer` / `CreatePayout` / `ReverseEntry`) under an
  op-derived idempotency key, so retries never double-book; one reject
  signature rejects the op at once and releases the freeze. Expired ops
  are swept by `ExpireMultisigOperations` (lazy on votes too). Every
  submission, vote, approval, rejection, expiry, and execution lands in
  the structured audit chain, and `GET /metrics` exposes
  `ledger_multisig_submitted_total` / `ledger_multisig_approved_total` /
  `ledger_multisig_rejected_total` / `ledger_multisig_expired_total`.
  Complements the named-vote quorum above: quorum trusts the
  caller-supplied approver identity, multisig verifies the key.

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

With `"business_day_cadence": true` on the create request, the plan
advances T+N business days after each firing (N = the interval in whole
days; a sub-day interval advances one business day) instead of a fixed
duration — payroll and settlement runs never land on a holiday. Missed
nominals are still skipped (counted as `skipped_runs`), and the flag
survives snapshots.

A plan can be rehearsed before the sweeper touches it: `GET
/transfer-schedules/{id}/preview?horizon=<duration>` (e.g. `?horizon=72h`;
default `24h`, max `8760h`) lists the nominal firing times inside the
horizon — same recurrence arithmetic the sweeper uses (fixed `interval`
cadence from `next_run_at`, `ends_at` truncation, no backfill of missed
runs; paused/cancelled/completed plans preview zero firings) — and
dry-runs each firing's transfer on a private copy of the ledger through
the full `PostTransfer` admission chain (frozen, overdraft, daily-limit,
budget, period). Each firing reports `admitted`, a stable failure
`code` (e.g. `account_frozen`, `overdraft_not_allowed`,
`daily_limit_exceeded`) with the exact rejection `reason`, and the
simulated per-leg balance movement plus budget impact when admitted;
firings are evaluated in nominal order so an admitted run's movement is
visible to the later ones. The preview is strictly read-only: no journal
writes, no version bump, no audit events, no idempotency keys consumed,
and the schedule's `next_run_at`/`run_seq`/status are never mutated —
the sweeper never sees a preview run.

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

#### Cross-currency sweep (`POST /sweeps/fx`)

The FX twin of `POST /sweeps`: every source's positive per-currency
balance is converted into one target currency at the ledger's configured
FX rate table and moved to the target account — one FX leg-pair per
`(source, currency)`, all under the sweep ID, atomically. A `(source,
currency)` already in the target currency sweeps same-currency (no rate
lookup, one entry). The shape is a regional treasury sweeping
multi-currency sub-merchant balances into one home-currency master
account.

```bash
curl -X POST localhost:8080/sweeps/fx -d '{
  "sweep_id": "fxsweep-2026-10-20",
  "from_accounts": ["sub-merchant-1", "sub-merchant-2"],
  "to_account": "treasury",
  "to_currency": "EUR",
  "idempotency_key": "fxsweep-day-2026-10-20"
}'
# 201 {"sweep_id":"fxsweep-2026-10-20","to_currency":"EUR","fx_account":"fx-clearing",
#      "legs":[{"from_account":"sub-merchant-1","source_currency":"USD","amount_cents":10000,
#               "converted_cents":10800,"principal_entry_id":"fxsweep-2026-10-20/sub-merchant-1/USD",
#               "fx_entry_id":"fxsweep-2026-10-20/sub-merchant-1/USD/fx"}, ...],
#      "fx":[{"from_currency":"USD","to_currency":"EUR","source_cents":10000,
#             "converted_cents":10800,"rate_num":108,"rate_den":100,"direction":"sell",...}],
#      "entries":[{...}],"duplicate":false}
```

Each converted leg reuses transfer-FX semantics (see the `FX transfers`
section below): the principal books the converted
amount in the target currency (debit target, credit the FX clearing
account) and the `/fx` leg books the source amount in the source currency
(debit FX clearing account, credit source). Pricing is the transfer
default — the sweep "sells" its foreign balances at the pair's bid (a
mid-only rate deals at mid). Unlike a transfer, no spread leg is booked:
the target receives the full dealing-price conversion; the spread is
disclosed on the receipt, never taken. A missing rate (or an expired
one) for a `(source, target)` pair rejects the whole sweep with `422` —
nothing is recorded. The FX clearing account defaults to the
ledger-wide one (`LEDGER_FX_ACCOUNT` / `WithFXAccount`) and may be
overridden per sweep with `fx_account`; it must differ from every source
and the target. The receipt's `fx` array discloses the conversion behind
each leg (rate, effective version, dealing side, settled amounts on both
sides) — idempotent replays omit it, because the rate table may have
moved since (the replayed journal entries are the authoritative record).
Sweep reserve floors, the monthly spend budget, the period gate, posting
rules, balance watches, and the idempotency/snapshot/audit chains all
behave exactly like the plain sweep.

#### Sweep reserve floors (`PUT/GET/DELETE /sweep-reserves`)

An opt-in per-`(account, currency)` minimum balance that sweeps leave
behind: only the excess over the floor moves to the target. The shape is
a merchant's sub-merchant keeping its settlement float for next-day
refunds while the surplus still rolls up at day end. A floor that meets
or exceeds the balance drops the leg entirely — the sweep still succeeds
with an empty receipt.

```bash
curl -X PUT localhost:8080/sweep-reserves -d '{
  "account": "sub-merchant-1", "currency": "USD", "floor_cents": 300000
}'
# 200 {"account":"sub-merchant-1","currency":"USD","floor_cents":300000}

curl localhost:8080/sweep-reserves
# 200 {"sweep_reserves":[{"account":"sub-merchant-1","currency":"USD","floor_cents":300000}]}

curl -X DELETE 'localhost:8080/sweep-reserves?account=sub-merchant-1&currency=USD'
# 200 {"status":"cleared"}
```

The floor must be positive (use `DELETE` to clear; a zero or negative
floor is `400`); reserve changes are structural — they never bump the
journal version — and are audited as `sweep_reserve_set` /
`sweep_reserve_changed` / `sweep_reserve_cleared`. Reserves survive
disaster-recovery snapshots and are disclosed on the reconcile report
(`sweep_reserves`).

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

### `POST /accounts/{id}/split`

Account split: the inverse of a merge — atomically moves **specified**
amounts out of the source account (`{id}`) into the target account, one
journal entry per leg, `<split ID>/<currency>`, while the source keeps
operating on the remainder. The shape is a merchant spinning a product
line into its own sub-account: the source is **not** frozen or closed —
splitting is a reorganization, not a decommission. Each leg names either
a fixed `amount_cents` or a `ratio_bps` share (1–10000 basis points,
floored) of the source's balance in that currency at split time; exactly
one sizing mode per leg, one leg per currency.

```bash
curl -X POST localhost:8080/accounts/parent-corp/split -d '{
  "split_id": "split-2026-10-20",
  "to_account": "child-corp",
  "legs": [{"currency":"USD","amount_cents":4000},{"currency":"EUR","ratio_bps":5000}],
  "idempotency_key": "split-parent-corp-2026-10-20"
}'
# 201 {"split_id":"split-2026-10-20","from_account":"parent-corp","to_account":"child-corp",
#      "legs":[{"from_account":"parent-corp","to_account":"child-corp","currency":"USD",
#               "amount_cents":4000,"entry_id":"split-2026-10-20/USD"}, ...],
#      "entries":[{...}],"duplicate":false}
```

Only positive balances are splittable: a leg's effective amount must be
positive and must not exceed the source's balance in its currency — a
dust ratio that floors to zero or a fixed amount above the balance is
rejected, never clamped. Check order is validation, then the idempotency
replay check, then the split-ID conflict check, then the
closed/frozen/suspended checks on **both** accounts, then the overdraft
check on the target (a documented no-op: legs move positive money into
the target, which can never take it below zero), then the
accounting-period gate — a backdated split landing in a closed period
returns `422`. A duplicate idempotency key returns `200` with the
original receipt. Split keys live in their own namespace and honor
`LEDGER_IDEMPOTENCY_TTL`; the split registry itself survives pruning and
round-trips through full and incremental snapshots. Like merges, holds
are advisory and daily outflow limits do not apply. `POST /reconcile`
reports every committed split in its `splits` list.

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
and, formally, via the auto-declared `reversal_of` entry link
(`GET /entries/{id}/links` — the tamper-evident replacement for memo-text
matching, see "Entry links" below); both are counted by
`ledger_reversals_total{kind="full"}`.

### `POST /entries/{id}/reverse-partial` (partial reversal / 部分冲正)

Corrects an entry in smaller slices: appends a new entry reversing
`amount_cents` of the original amount, legs swapped, with the
deterministic ID `<entry ID>/reversal/<n>` (`n` the 1-based index of
this entry's n-th partial reversal) and memo
`reversal_of:<entry ID>/partial/<n>`. The net outstanding correction —
genuine reversals minus their own reversal-of-reversal corrections —
can never exceed the original amount: anything over the cap returns
`409`, as does a full reversal while any correction is outstanding (it
would over-correct — reverse the partials individually first, which
re-opens the full path). A *new* idempotency key on an over-cap entry
also returns `409`; repeating with the same key returns `200` with the
existing partial. Keyless requests always book the next index, so pass
an `idempotency_key` for retry safety.

```bash
curl -X POST localhost:8080/entries/9f2c.../reverse-partial \
  -d '{"amount_cents":300}'
# 201 {"id":"9f2c.../reversal/1","debit_account":"bank","credit_account":"alice",
#      "amount_cents":300,"currency":"USD","memo":"reversal_of:9f2c.../partial/1","duplicate":false}
```

Partial reversals run the same full `POST` risk gate order as full
reversals (idempotency replay → closed/frozen/suspended `403` →
overdraft/daily-limit/period `422`), never trigger journal rules or
enter dual-control review, and are counted by
`ledger_reversals_total{kind="partial"}`. An inherited `external_ref`
becomes `reversal_of:<original ref>/partial/<n>` (distinct per index,
so partials never collide in the reference index).

### Entry links (`GET /entries/{id}/links`)

Explicit directed associations between journal entries — the
tamper-evident replacement for fragile memo-text matching
(`reversal_of:<id>` in a memo tells you two entries are related; a link
*proves* it). A link says "entry A is the `reversal_of` / `adjustment_of`
/ `fee_of` / `refund_of` entry B", declared through
`ledger.DeclareEntryLink(from, to, linkType)` (both endpoints must already
be journaled; unknown link types, empty IDs, and self-links are rejected).

`POST /entries/{id}/reverse` and `POST /entries/{id}/reverse-partial`
declare their `reversal_of` link automatically, atomically with the
reversal commit — the memo stays searchable, the link stays verifiable.
`adjustment_of` / `fee_of` / `refund_of` links are declared explicitly
after the operation (e.g. a refund transfer declares `refund_of` against
the original payment). Declaring the identical link twice is a silent
no-op, and declarations never bump the journal version — a link is
structural config, not money movement.

```bash
curl -s localhost:8080/entries/<reversal-id>/links
# 200 {"entry_id":"<reversal-id>","links":[
#   {"from_entry_id":"<reversal-id>","to_entry_id":"<original-id>",
#    "link_type":"reversal_of","declared_at":"2026-10-18T07:31:00Z",
#    "direction":"outgoing"}]}
```

The query is bidirectional: from the reversal the link reads `outgoing`,
from the original the same link reads `incoming`. An entry with no links
returns an empty list (`404` only for an unknown entry ID — entry IDs
containing `/`, like `<id>/reversal`, must be URL-escaped). Every
declaration appends to a dedicated tamper-evident link chain, verified by
`GET /entries/verify` alongside the entry chain: rewriting a declared
link breaks verification exactly like rewriting a journaled amount. The
`Reconcile` report discloses every link as `entry_links` plus the
per-root `link_chains` expansion of the `reversal_of`/`adjustment_of`
correction history, and declarations are counted by
`ledger_entry_links_total`. `mandate_of` links (see "Direct-debit
mandates" below) are disclosed flat in `entry_links` like the other
associations — they never enter `link_chains`, which is reserved for
corrections.

### Direct-debit mandates (`POST /mandates`, 代扣协议)

Standing pull authorizations — the SEPA-direct-debit / autopay agreement
behind subscription billing. `POST /mandates` installs a mandate: the
payer (customer) and payee (merchant), the per-debit cap
(`max_amount_cents`), one currency, a rate cap (`frequency`:
`per-day` / `per-week` / `per-month` — at most one debit per UTC day,
ISO week, or calendar month), and the validity window (`valid_from` /
`valid_until`, RFC3339).

```bash
curl -X POST localhost:8080/mandates -d '{
  "id":"sub-acme","payer":"cust-42","payee":"acme",
  "max_amount_cents":5000,"currency":"USD","frequency":"per-month",
  "valid_from":"2026-10-01T00:00:00Z","valid_until":"2027-10-01T00:00:00Z"}'
# 201 {"id":"sub-acme","status":"active",...}
```

`POST /mandates/{id}/debits` pulls under the mandate — the only way to
debit against it. The mandate gates run first (the mandate must exist
and be active, the debit's booking timestamp must fall in its validity
window, the amount must not exceed the cap, the currency must match, the
frequency window must be free — violations are `404`/`422`), then the
debit posts through the **full** `POST` validation sequence (frozen,
overdraft, daily limit, budget, period, journal rules), so a mandate
debit can never dodge a booking control the operator's own `POST` would
hit. The legs come from the mandate itself — the entry debits the payee
and credits the payer — so a caller can never pull the wrong pair of
accounts. Every debit is annotated on two channels, like reversals: the
memo is stamped `mandate_of:<id>` (appended after any caller memo,
searchable via `GET /entries?memo=mandate_of:`) and a `mandate_of` entry
link is declared from the debit entry to the mandate's reference record.

On the link's `To` endpoint, an honest-design note: mandates are
structural config, not journal rows, so `mandate_of` is the one link type
whose `To` endpoint is the mandate's reference record (the mandate ID)
rather than a journal entry — journaling a zero-amount marker entry at
mandate creation just to give the link an entry-shaped endpoint would
fabricate money movement. The declaration is validated against the
mandates registry and hashed into the link chain exactly like the
entry→entry links.

Lifecycle: `active → suspended | cancelled | expired`
(`POST /mandates/{id}/suspend`, `POST /mandates/{id}/cancel`; expiry is
lazy by predicate plus `POST /mandates/expire` for observability).
`GET /mandates/{id}` reads a mandate with its effective status;
`GET /mandates/{id}/debits` is the per-mandate reconciliation query —
the debits in debit order, paged by `limit`/`cursor`. Repeating a debit
`idempotency_key` returns the original receipt (`200`,
`duplicate: true`) without re-evaluating mandate state, because a replay
books nothing new. The `Reconcile` report discloses every mandate as
`mandates` (payer/payee, cap, currency, frequency, window, effective
status, total and in-window debit counts), mandates round-trip through
disaster-recovery snapshots, and `GET /metrics` exposes
`ledger_mandate_debits_total`.

**Automatic retry for failed debits (autopay recovery):**
`SetMandateRetryPolicy` installs an opt-in retry policy per mandate —
`max_retries` plus a `backoff` table. When a debit fails with a
*retryable* error — insufficient funds (`ErrAccountOverdraft` /
`ErrInsufficientAvailableFunds`), the daily outflow limit, or the
monthly spend budget — the failure is recorded as a pending retry
(`next_retry_at` from the backoff table) instead of dropped; the error
is still returned to the caller, so the failure is never silent.
`SweepMandateRetries(now)` is the single entry point that fires due
retries (the `SweepDue` convention: snapshot under the lock, fire
without it, per-retry firing flag). Every attempt `n ≥ 1` fires under
an independent idempotency key — the original key (or the debit ID)
suffixed `#attempt-<n>` — so attempts never replay each other and a
crash between commit and finalization replays safely. The full
mandate-gate and Post validation sequence runs on every attempt.
Validation and risk-control rejections (sanctions, frozen/suspended
accounts, mandate gates) are terminal and never retry. Attempt
outcomes — scheduled, rescheduled, succeeded, exhausted, terminal —
enter the structured audit chain; when the attempts run out, a
`mandate_retry_exhausted` outbox event fires so downstream consumers
can wire it into dunning escalation. Retry policies and pending
retries are in-memory operational state (like sweeper flags): they do
not round-trip through snapshots.

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

### `POST /accounts/{id}/suspend` and `POST /accounts/{id}/resume`

Reversible operational pause for an account (fintech investigation hold /
maintenance window). A suspended account rejects every write path that
would book a new journal entry through it — posts, batches, transfers
(principal, fee, and split legs), FX transfers, reversals, holds and
captures, sweeps, merges, closing and revaluation entries, term-deposit
opens, allocations, rule-derived legs, and the system postings
(accruals, dividends, dormancy fees) — with `403`, while balance,
snapshot, trial-balance, per-account entries, chain verification, and
the end-of-day reconcile keep working. Suspending is idempotent (a
repeat returns the original record with `already_suspended: true` and
emits nothing new), never bumps the ledger version, and records a
`suspend`/`resume` structured audit event plus an
`account_suspended`/`account_resumed` outbox event.

Suspension is orthogonal to the frozen risk-control stop and the
terminal close: the three states can stack, and the gate order is
closed → frozen → suspended, so a frozen-and-suspended account still
reports the frozen `403` it reported before. New suspensions are
counted by `ledger_account_suspends_total` on `GET /metrics`;
suspended rejections by `ledger_suspended_rejections_total`; the
reconcile report lists them under `suspended_accounts`. Suspension
records round-trip through snapshots.

```bash
curl -s -X POST localhost:8080/accounts/cash/suspend
# {"account":"cash","suspended":true,"already_suspended":false,"suspended_at":"2026-10-18T06:55:00Z"}
curl -s -X POST localhost:8080/accounts/cash/resume
# {"account":"cash","suspended":false,"resumed":true}
```

**Operator attribution (`x-ledger-actor`).** Every operator action —
freeze/unfreeze, suspend/resume, transfer-review approve/reject, account close, period
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

### Dormant accounts (Go API: `DetectDormant` / `ReactivateAccount` / `IsDormant` / `ChargeDormancyFee`)

The inactive-account lifecycle marking (fintech compliance). Dormancy is
a *state marking*, not fund control — it is orthogonal to the freeze
above: freezing is a reversible risk-control stop on fund movement,
dormancy marks that the account itself has gone quiet. Marking never
freezes funds, never touches balances, totals, the ledger version, or the
audit chain; it is structural config (like the frozen-account list) and
round-trips through snapshots.

```go
// Mark every account with no journal activity in the last 30 days.
report, err := l.DetectDormant(30)
// report.Marked: the newly marked accounts, sorted, each with
// {account, marked_at, last_activity}. Already-dormant accounts are
// left alone; accounts with no journal history are never marked.
l.IsDormant("old-customer") // true
// Lift the stop (audited as dormant_reactivated).
l.ReactivateAccount("old-customer")
```

A dormant account rejects operator `Post` and `Transfer` bookings with
`ledger.ErrAccountDormant` until reactivated. The gate sits after the
idempotency replay check (a replay of a pre-dormancy key still returns
its original entry) and before the frozen check — lifecycle stops
(closed, dormant) precede risk-control stops. Reads are unaffected.
Each marking and reactivation is a structured audit event
(`dormant_marked` / `dormant_reactivated`); only transfers' own legs
(From/To) are gated, not system-routed fee legs.

The opt-in monthly inactivity fee follows the accrual convention:

```go
// Deterministic ID "dormant-fee/<account>/<YYYY-MM>": re-charging the
// same month replays (duplicate == true). capCents caps the cumulative
// net fee (zero disables); reversible via the generic ReverseEntry.
l.ChargeDormancyFee("old-customer", "fee-income", 500, asof, capCents)
l.DormancyFeeTotal("old-customer", "fee-income", since, until)
```

The fee is an ordinary journal entry (own chain link, version bump) that
runs the full `Post` gate order but never fires journal rules nor
enters dual-control review. It deliberately skips the dormant-account
stop itself — the fee exists to charge dormant accounts — posting
through the same system path as accruals and dividends, which likewise
keep posting through dormant accounts.

#### Escheatment (Go API: `EscheatDormant` / `ReverseEscheatment`)

The compliance end of the dormancy lifecycle: unclaimed-property sweep.
`EscheatDormant(thresholdDays, escheatAccount)` scans the dormant set
(`DetectDormant` must have run first — unmarked accounts are never
swept) and, for every dormant account whose last activity is strictly
older than `thresholdDays` days, books one entry per (account, currency)
with a positive balance — credit the dormant account, debit the escheat
account — for the full balance, then freezes the swept account so no
further writes land on it.

```go
// Sweep every account dormant 365+ days into the unclaimed-property account.
report, err := l.EscheatDormant(365, "unclaimed-property")
// report.Escheated: [{account, currency, amount_cents, entry_id, ...}]
// report.Skipped: positive balances that could not be swept, with reasons
// (frozen account, gate rejection). Zero/negative balances are out of scope.
```

Entry IDs are deterministic —
`escheat/<account>/<currency>/<YYYY-MM-DD>` — so the idempotency key is
(account, currency, day). Each entry is a full journal row (own
audit-chain link, version bump, memo `escheat`), each swept account gets
an `escheat` audit event plus the `freeze` audit, and `Reconcile`
discloses every sweep in `escheats` (recovered from the journaled rows,
with a `reversed` flag). A mistaken sweep is undone with
`ReverseEscheatment(escheatID, asof)`: it books the mirror entry under
`<escheat ID>/reversal` and unfreezes the account — the original entry
is never deleted, the audit chain stays append-only, and the dormant
mark stays (the money is back but the account is still quiet).

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

#### Close preflight (`POST /periods/{id}/preflight`, `?preflight_clean=true`)

The period lock is cheap and idempotent — which made it easy to
discover, *after* locking, that the month still had a pending
dual-control review, an unexpired hold, an unsettled FX forward, or a
receivable at dunning level L3. The preflight scan answers "what would
block a clean close?" before anything locks:

- **Blockers** (refuse a gated close): `pending_reviews`,
  `active_holds`, `booked_forwards`, `dunning_level3`. Lapsed forwards
  do not count — expiry *is* cancellation.
- **Warnings** (smells, not refusals): `empty_period` (no journal rows
  dated in the month), `no_closing_accounts` (a `?closing=true` close
  would fail).

The scan is pure read — one RLock, no version bump, no journal rows, no
audit events — so it is safe to run as a readiness probe. With
`?preflight_clean=true` on the close, any blocker refuses the close with
`422` (`ErrPeriodPreflightBlocked`): the period stays open and nothing
is recorded, and the refusal is audit-logged (`period_preflight_blocked`).
The gate is off by default — `POST /periods/{id}/close` behaves exactly
as before — and it also applies to the `?closing=true` path (refused
before booking anything). `ledger_period_preflight_runs_total` counts
the probe calls.

```bash
curl -s -X POST localhost:8080/periods/2026-09/preflight
# {"period":"2026-09","blockers":[...],"warnings":[...],"clean":false}
curl -s -X POST 'localhost:8080/periods/2026-09/close?preflight_clean=true'
# 422 {"error":"ledger: period close blocked by preflight findings: 1 blocker(s), first: pending_reviews"}
```

#### Period auto-close plans (`POST /periods/{id}/auto-close`, `.../auto-close/cancel`, `GET /periods/auto-close`)

Month-end compliance without an operator at the terminal at midnight: a
plan registers a period to close at a scheduled UTC instant, and the
background worker fires it through the same close path as `POST
/periods/{id}/close` — identical gates, identical audit events:

```bash
curl -s -X POST localhost:8080/periods/2026-09/auto-close \
  -d '{"at":"2026-10-01T00:00:00Z","closing":true}'
# 201 {"period":"2026-09","close_at":"2026-10-01T00:00:00Z","with_closing":true,"status":"pending"}
curl -s -X POST localhost:8080/periods/2026-09/auto-close/cancel
# 200 {"period":"2026-09","status":"cancelled"}
curl -s localhost:8080/periods/2026-09/auto-close   # one plan
curl -s localhost:8080/periods/auto-close            # all plans, sorted
```

With `"closing":true` the fired close books the LG-48 closing entries;
without it the plan just locks the period. The close instant must be in
the future; only one pending plan per period is allowed (a duplicate is
`409`, a missing plan is `404`). A failed fire — the same gates that
fail a manual close (frozen account, overdraft, unconfigured closing
accounts, next month closed) — marks the plan `failed` with the reason,
audits `period_autoclose_failed`, and **keeps the period open**: a failed
plan is never retried by the sweeper, so the operator closes the month
by hand after fixing the gate. Every transition (`scheduled`,
`cancelled`, `executed`, `failed`) lands in the structured audit chain,
and plans round-trip through disaster-recovery snapshots, so a restore
never silently cancels a month-end close. The worker is opt-in via
`LEDGER_PERIOD_AUTOCLOSE_INTERVAL` (see [Environment](#running));
`ledger_period_autocloses_total` counts fired plans, executed and
failed.

### Idempotency key registry query (`GET /idempotency-keys`)

Every journal-writing operation keeps its own idempotency namespace, and
an operator chasing "why did my retry return a duplicate?" currently has
to guess which one the key landed in. This endpoint is the read-only
cross-namespace listing: the key, its namespace, what it maps to (an
operation ID — never a payload or an amount), and when it expires under
the idempotency TTL:

```bash
curl -s 'localhost:8080/idempotency-keys?namespace=transfers&limit=50'
# {"keys":[{"namespace":"transfers","key":"op-2026-10-001",
#           "maps_to":"tx-2026-001","expires_at":"2026-10-21T13:00:00Z"},...],
#  "count":1}
```

`?prefix=` filters keys by prefix, `?namespace=` restricts to one
namespace (unknown names fail `400` and the response names the valid
ones), `?limit=` bounds the result (default 100, capped at 1000). The
listing is operator tooling: it requires the operator API-key scope even
though it is a `GET` (`read:ledger` keys are refused when API-key auth
is enabled), and it never exposes business payloads — amounts,
accounts, and memo text are not in the response.

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

### KYC/尽调状态门控 (Go API: `SetKyc` / `GetKyc` / `ExpireKyc` / `SetKycThreshold`)

Per-account KYC (know-your-customer / 尽调) profiles gate the posting
paths — a fintech compliance control for counterparty due diligence.
Each profile carries a status (`unverified` / `pending` / `verified` /
`expired`), an optional risk tier, and optional verification/expiry
timestamps.

Gate policy: an account whose profile is **expired** blocks *all* new
`POST /entries` / `POST /transfers` through it (`422`), whatever the
amount — a lapsed KYC means the ledger no longer knows the
counterparty, so the account is stopped until re-verified. An account
with **no profile** (or an `unverified` / `pending` profile) is rejected
only when the posting amount exceeds the ledger's opt-in KYC threshold
(`SetKycThreshold`, cents; `0` disables the threshold gate, the
default) — small-value activity may proceed while a review is pending,
but material amounts need a verified counterparty.

Check order inside `Post`: field validation → idempotency replay →
frozen (`403`) → overdraft (`422`) → **KYC (`422`)** → daily limit →
budget → period. Like the other risk controls, a KYC rejection books
nothing, and idempotent replays return the original entry instead of
failing (the replay books nothing new). Expiry is lazy: every write
path sweeps lapsed profiles before the gate, and `POST /kyc/expire`
runs the same sweep explicitly (the scheduled "到期复核").

```bash
curl -s -X POST localhost:8080/accounts/acme/kyc \
  -d '{"status":"verified","risk_tier":"low","expires_at":"2027-01-01T00:00:00Z"}'
curl -s localhost:8080/accounts/acme/kyc          # query the profile
curl -s -X POST localhost:8080/kyc/expire         # {"expired": N}
```

The reconcile report discloses the KYC position (`kyc`: threshold,
profile counts by state, expired accounts), profiles and the threshold
round-trip through disaster-recovery snapshots, and rejections are
counted on `ledger_kyc_rejections_total`.

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

#### Tiered deposit rates (Go API: `SetInterestTiers` / `AccrueInterestTiered`)

Real deposit products price balance bands differently — the first
$10,000 at 5.00%/yr, the next at 3.00%/yr. `SetInterestTiers` installs
that ladder (first band must start at 0, minima strictly ascending,
rates 1–10000 bps); `AccrueInterestTiered` then books one day's
interest as the per-band sum of
`floor(band_principal * rateBps / 10000 / 365)` — overflow-safe integer
math, no floats anywhere.

```go
l.SetInterestTiers([]ledger.InterestTier{
    {MinCents: 0, RateBps: 500},         // 5.00%/yr on the first $10,000
    {MinCents: 1_000_000, RateBps: 300}, // 3.00%/yr above that
})
// $15,000 principal: 136 + 41 = 177 cents for the day.
l.AccrueInterestTiered("interest-expense", "interest-payable", 1_500_000, asof, capCents)
```

Tiered accruals are their own accrual kind (`interest-tier`): entry IDs
`accrual/interest-tier/<expense>/<payable>/<date>`, memo
`accrual:interest-tier`, queryable via
`AccruedTotal(..., ledger.AccrualKindInterestTiered)` and reversible
via `ReverseAccrual`. The table is structural config — setting it does
not bump the version, every change is audited as `interest_tiers_set`
/ `interest_tiers_cleared`, and it round-trips through snapshots.
Clearing the table (empty slice) returns tiered accrual to "not
configured". Negative principals are rejected — overdraft balances
price through `AccrueOverdraftInterest`, not deposit bands.

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

#### Interest capitalization (Go API: `CapitalizeInterest`)

The compound-interest step: moves the outstanding accrued interest for
an `(interest, expense, payable)` triple into the principal account —
debit the interest payable, credit the principal — so the next accrual
round prices off the larger principal. The entry is an ordinary journal
row (own audit-chain link, version bump, full `Post` gates, no rules
cascade, no dual-control — like accruals) and emits an
`interest_capitalized` audit event. The entry ID is deterministic —
`accrual-capitalization/interest/<expense>/<payable>/<principal>/<YYYY-MM-DD>` —
so re-running the day's capitalization is idempotent. `AccruedTotal`
nets out capitalized amounts: after capitalization the triple's
outstanding total is zero, and capitalizing frees cap room for new
accruals. Reversing a capitalization (`ReverseAccrual` accepts
capitalization IDs) restores the payable and the total. Capitalizing a
triple with zero outstanding accrual is rejected with
`ErrNothingToCapitalize`; unlike the daily accruals, capitalization is
not subject to the business-day gate — it is a period-end adjustment.

```go
// Move 3 days of accrued interest into the loan principal.
l.CapitalizeInterest("interest-expense", "interest-payable", "loan-principal", asof)
```

#### Overdraft interest (Go API: `AccrueOverdraftInterest`)

Prices a negative balance: one entry per (account, currency, UTC calendar
day) — debit the interest expense account, credit the interest payable
account — for `floor(principal * rateBps / 10000 / 365)` cents, where
`principal` is the day's overdrawn amount. The charged account is not a
journal leg (accrual convention); its negative balance is only read to
size the charge. The entry ID is deterministic —
`accrual/overdraft-interest/<account>/<currency>/<YYYY-MM-DD>` — so the
idempotency key is (account, currency, day): re-running the day returns
the original entry.

```go
// 10%/yr on a -$10,000 USD balance: floor(1000000*1000/10000/365) = 273c.
// compound=true adds previously accrued (unreversed) interest to the
// principal: end-of-day compounding without a capitalization entry.
l.AccrueOverdraftInterest("borrower", "od-interest-expense", "od-interest-payable", "USD", 1000, false, asof)
```

A non-negative balance is rejected with `ErrAccountNotOverdrawn`; a dust
charge that rounds to zero books nothing (`ErrNonPositiveAmount`).
Overdraft protection does not block the charge — protection guards the
account's own postings, and this entry never touches it. The entry is an
ordinary journal row (own audit-chain link, version bump, full `Post`
gates, no rules cascade, no dual-control, dormant stop skipped) with the
memo `accrual:overdraft-interest`, reversible via `ReverseAccrual`.

#### Funds transfer pricing (Go API: `SetFtpCurve` / `FtpRateBps` / `FtpPreview` / `ChargeFtp`)

Internal funding priced like a market: business units with a funding
shortfall pay the treasury pool for funding; units with surplus funding
get paid by the pool. One annualized rate curve per currency —
`(tenorMonths, rateBps)` buckets, e.g. `{{1, 100}, {12, 500}}` —
with linear interpolation between buckets and flat clamping outside the
curve. `ChargeFtp` books one entry per (account, currency, tenor, UTC
day): debit the FTP expense / credit the pool for a gap (positive
`gapCents`), debit the pool / credit the FTP revenue for a surplus
(negative) — the entry ID `ftp/<currency>/<account>/<tenor>/<YYYY-MM-DD>`
makes the day's charge idempotent. `FtpPreview` prices with zero side
effects. Curves are versioned (every `SetFtpCurve` bumps the version,
stamps `SetAt`, and emits an `ftp_curve_set` audit event — the version
on each charge names the curve that priced it), and curves plus the
internal counterparties (`WithFtpAccounts`) round-trip through
snapshots. Charges are system adjusting entries: full `Post` gates, own
audit-chain link, no rules cascade, no dual-control. The charge is
`floor(|gap| * rateBps / 10000 * tenorMonths / 12)` cents, overflow-safe;
a dust charge that rounds to zero books nothing. `Reconcile` discloses
per-currency `ftp_totals` (cost/revenue/charges, journal-derived so the
section can never disagree with the books) and `ftp_curve_versions`.

```go
l := ledger.New(ledger.WithFtpAccounts(ledger.FtpAccounts{
    PoolAccount: "treasury-pool", ExpenseAccount: "ftp-expense", RevenueAccount: "ftp-revenue",
}))
l.SetFtpCurve("USD", []ledger.FtpTenorPoint{{TenorMonths: 1, RateBps: 100}, {TenorMonths: 12, RateBps: 500}})
l.FtpPreview("bu-lending", "USD", 12, 1_200_000) // read-only price
l.ChargeFtp("bu-lending", "USD", 12, 1_200_000, asof) // books 60000c: debit ftp-expense / credit treasury-pool
```

### Fixed-asset depreciation (Go API: `RegisterFixedAsset` / `Depreciate` / `AccumulatedDepreciation`)

Systematic monthly amortization of capitalized assets — the complement
of accruals (accruals pre-accrue expenses; depreciation amortizes
capitalized assets). One entry per (asset, UTC calendar month): debit
the depreciation expense account, credit the accumulated depreciation
(contra-asset) account. Entry IDs are deterministic —
`depr/<assetID>/<YYYY-MM>` — so re-running a month's depreciation is
idempotent: the second call returns the original entry and books nothing
new.

```go
l.RegisterFixedAsset(ledger.FixedAsset{
    ID: "srv-001", Account: "fixed-assets:servers",
    CostCents: 1_200_000, Currency: "USD", SalvageCents: 0,
    UsefulLifeMonths: 36, Method: ledger.DepreciationStraightLine,
})
// Books 33333c for March 2026: debit depr-expense / credit accum-depr.
l.Depreciate("srv-001", "depr-expense", "accum-depr", asof)
// Net depreciation booked to date (reversals net out).
l.AccumulatedDepreciation("srv-001")
```

Two methods: `straight-line` (`floor((cost-salvage)/months)` per month,
trued up in the final month so the lifetime total is exact) and
`double-declining` (`floor(bookValue*2/months)` against the declining
book value). The charge is capped at the remaining depreciable base —
depreciation never takes the book value below salvage — and a fully
depreciated asset books nothing (empty entry, nil error). Depreciation
entries are ordinary journal rows (own audit-chain link, version bump,
full `Post` gates, covered by `VerifyAccountingEquation` and
`Reconcile`) with two deliberate exceptions, mirroring accruals: they
never fire the journal rules engine and never enter dual-control review.
The memo `depreciation:<method>` makes rows discoverable via
`GET /entries?memo=depreciation:straight-line`. Reversing a month with
`ReverseEntry` nets against it in `AccumulatedDepreciation`, so the
month can be corrected and re-run. The asset register is in-memory
structural config — re-register assets after a disaster-recovery
restore (the booked entries themselves round-trip as ordinary rows).

### Business-day calendar (LG-95)

A configurable holiday calendar for interest accrual and settlement
(fintech). Weekend days (default Saturday/Sunday — e.g. Friday/Saturday
in some regions) plus a custom holiday table of UTC dates:

```bash
LEDGER_BUSINESS_DAY_WEEKEND="Fri,Sat" LEDGER_BUSINESS_DAY_HOLIDAYS="2026-01-01,2026-10-01"
```

The calendar drives three opt-in behaviors:

- **Accruals skip non-business days** — `LEDGER_ACCRUAL_BUSINESS_DAYS_ONLY=1`
  (or `WithAccrualBusinessDaysOnly(true)` / `SetAccrualBusinessDaysOnly`):
  `AccrueInterest`, `AccrueFee`, and `AccrueTermInterest` reject a
  non-business-day `asof` with `ErrNotBusinessDay` instead of booking.
  Interest accrues per business day, not per calendar day. Off by
  default (backward compatible).
- **Schedules fire on a business-day cadence** — `"business_day_cadence":
  true` on `POST /transfer-schedules`: after firing, `next_run_at`
  advances T+N business days (N = interval in whole days) instead of a
  fixed duration, so payroll/settlement runs never land on a holiday.
- **Term deposits settle maturity on business days** —
  `"business_day_maturity": true` on `POST /term-deposits`: a `MaturesAt`
  landing on a non-business day moves to the next business day at open
  time; the stored date is the contractual one.

The calendar carries a version counter bumped on every change
(`SetBusinessDayCalendar` / `AddHoliday` / `RemoveHoliday`; changes are
audited), and both the table and the version round-trip through
disaster-recovery snapshots. Go API: `IsBusinessDay`, `NextBusinessDay`,
`PrevBusinessDay`, `AddBusinessDays`, `BusinessDayCalendar`,
`BusinessDayCalendarVersion`.

### Dividend declaration (Go API: `DeclareDividend` / `Dividends`)

Corporate-action dividend bookings (fintech corporate actions). One
double-entry row per shareholder: debit the retained-earnings pool,
credit the holder, for `floor(total * sharesBps / 10000)` cents each —
the dust remainder lands on the first holder, so the declared total is
always fully distributed and the books stay balanced. Shares must sum to
exactly 10000 bps.

```go
r, err := l.DeclareDividend("retained-earnings", 1000000, "USD",
    []ledger.DividendHolder{
        {Account: "holder-a", SharesBps: 6000},
        {Account: "holder-b", SharesBps: 4000},
    }, recordDate)
// r.DividendID == "dividend/retained-earnings/2026-10-15"
// r.Payouts[i].EntryID == "dividend/retained-earnings/2026-10-15/<i>"
// List every declared dividend, sorted by dividend ID:
l.Dividends()
```

The declaration is idempotent by construction: the dividend ID is
deterministic — `dividend/<pool>/<YYYY-MM-DD>` (recordDate, UTC) — and
each holder's row carries `<dividendID>/<index>`. Re-running a
declaration replays the original rows (`Duplicate == true`) instead of
double-booking; a declaration interrupted by a failed leg is resumable —
the retry skips the rows already journaled and books the rest. The
record-date shareholding snapshot is tamper-evident: the holder table
(account, sharesBps) is journaled (the share rides in the memo
`dividend:<bps>bps`) and the audit-chain hash covers every journaled
field, so a later-changed holder table cannot silently replace a
declared dividend.

Each leg runs the full `Post` gate order (validation, idempotency
replay, chart direction, frozen, overdraft, daily-limit, budget,
period) but, like accruals, never fires the journal rules engine and
never enters dual-control review: dividends are system-generated
corporate actions. Dividend legs post through the system path, so the
dormant-account stop does not apply — paying a declared dividend to a
dormant holder is the corporation's action, not operator activity.
Every leg is an ordinary journal entry — own
audit-chain link, version bump, entry_posted outbox event — and
`Reconcile` discloses every declared dividend (pool, record date,
total, per-holder payouts) in its `dividends` section.

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

### Rolling velocity limits

Per-account, per-currency cap on money movement inside a sliding window
(fintech/AML risk control — burst detection on customer cash-outs). Where
the daily outflow limit above uses UTC-calendar-day buckets, velocity
limits watch a rolling window (e.g. `10m`, `24h`): within any window
ending at the posting's timestamp, the account's inflow and outflow are
each bounded by a posting count and a moved-amount cap. Either dimension
rejects with `422` (`ErrVelocityLimitExceeded`) before anything is booked.

Configure with the `LEDGER_VELOCITY_LIMITS` environment variable
(`"<account>:<currency>:<window>:<maxCount>:<maxAmountCents>[,...]"`,
e.g. `"cust-123:USD:10m:50:100000"`), the `ledger.WithVelocityLimit` /
`SetVelocityLimit` API, or `PUT /velocity-limits`. A non-positive
`max_count` / `max_amount_cents` disables that dimension; at least one
must be positive. Windows are capped at 30 days.

The check runs after the monthly spend budget and before the period gate
— the last of the account risk controls — on `POST /entries` (debit leg
counts toward inflow, credit leg toward outflow), `POST /transfers`
(payer's total outflow incl. fee, payee's inflow), and reversals. Every
committed journal row feeds the windows (batch, sweep, hold capture, FX
legs, and system postings included); idempotent replays consume no
budget. The three controls are orthogonal and compose: a posting must
clear the daily limit (UTC-day outflow), the velocity limit (sliding
window, count + amount, both directions), and the API-key rate limiter
(request rates).

Windows start when the limit is configured — history before that is not
backfilled, and only accounts with a configured limit accumulate events,
so memory stays bounded by the configured windows. The configuration
(not the windows) round-trips through disaster-recovery snapshots;
`GET /velocity-limits` lists limits, `GET /velocity-limits/usage`
reports the current window usage, and the reconcile report lists every
configured limit (`velocity_limits`). Breaches land in the structured
audit log as `velocity_limit_breach` and are counted in
`ledger_velocity_limit_rejections_total`.

```bash
LEDGER_VELOCITY_LIMITS="cust-123:USD:10m:50:100000" ./ledger-api-go
curl -s -X PUT localhost:8080/velocity-limits \
  -d '{"account":"cust-123","currency":"USD","window":"10m","max_count":50,"max_amount_cents":100000}'
# 200 {"account":"cust-123","currency":"USD","window":"10m0s","max_count":50,"max_amount_cents":100000}
curl -s localhost:8080/velocity-limits/usage?account=cust-123\&direction=outflow
# {"account":"cust-123","currency":"USD","direction":"outflow","window":"10m0s",...,"count":0,"amount_cents":0}
```

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

Budget pace prediction (`ledger.BudgetPace`) linearly extrapolates each
budget's booked month-to-date spend over the elapsed share of the UTC
month and reports the projected month-end spend with a pace-alert flag.
The alert is advisory — it never blocks a posting — and fires once per
(account, currency, month) as a `budget_pace_alert` audit event, counted
in `ledger_budget_pace_alerts_total`; the alert ratio is configurable via
`SetBudgetPaceAlertRatio` (default 1.0, i.e. alert when the trajectory
would spend the full budget). The reconcile report's `budget_pace`
section discloses every budget's pace without firing alerts (Reconcile is
read-only; the explicit `BudgetPace` call is the alert entry point).

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

### Transfer beneficiary allowlists (`PUT /accounts/{id}/allowlist`)

Opt-in per-payer gate on the beneficiary of a transfer: while a payer has
a list installed, `POST /transfers` only books transfers whose payee is on
the payer's list. The payment-domain shape is merchant payout control — a
marketplace's settlement account pays out only to its registered
sub-merchants, so a compromised payout service cannot exfiltrate funds to
an attacker's account. Orthogonal to the denylist above: the denylist
screens accounts money may never move through in either direction
(compliance), the allowlist limits who one payer may pay (risk control).

Manage with `PUT /accounts/{id}/allowlist`
(`{"beneficiaries": ["sub-merchant-a", "sub-merchant-b"]}`) and
`GET /accounts/{id}/allowlist`. An empty set disables the gate — a payer
with no list installed may pay anyone, exactly as before allowlists
existed. The gate runs between the frozen and overdraft checks of the
transfer check order (field validation → idempotency replay → ID conflict
→ frozen → allowlist → overdraft → commit), so an idempotent replay still
returns the original receipt and the frozen gate always runs first. Only
the principal leg's payee is screened — the fee leg's recipient is the
operator's own revenue account, not a business beneficiary, and is exempt.

A transfer to an unlisted payee is rejected with
`403 {"error":"ledger: transfer beneficiary is not on the payer's transfer
allowlist: payer \"...\" is not allowlisted to pay \"...\""}` and records
nothing: no journal rows, no chain links, no version bump. List changes are
structural (no version bump) and round-trip through disaster-recovery
snapshots — a restore never silently re-enables payouts to unlisted
beneficiaries. Rejections are counted in
`ledger_allowlist_rejections_total`.

```bash
curl -s -X PUT localhost:8080/accounts/settle-corp/allowlist \
  -H 'x-ledger-actor: ops-chen' -d '{"beneficiaries":["sub-merchant-a","sub-merchant-b"]}'
curl -s localhost:8080/accounts/settle-corp/allowlist
# 200 {"account":"settle-corp","allowlist_enabled":true,"beneficiaries":["sub-merchant-a","sub-merchant-b"]}
curl -s -X POST localhost:8080/transfers \
  -d '{"transfer_id":"payout-1","from_account":"settle-corp","to_account":"attacker","amount_cents":100}'
# 403 {"error":"ledger: transfer beneficiary is not on the payer's transfer allowlist: ..."}
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

Every breach is also emitted to the domain-event outbox as
`balance_threshold_breached` (same facts as the audit event, plus the
breach's `trace_id` and `actor`), so registered webhook endpoints whose
event filter includes `balance_threshold_breached` receive an
HMAC-SHA256-signed notification through the outbound webhook dispatcher
(see the webhooks section) — the audit log stays the compliance record,
the webhook is the live channel. Without `LEDGER_OUTBOX_DIR` the event
is simply not emitted; filtering is a webhook concern, so a filtered-out
endpoint misses the push but the event remains in the outbox stream for
`GET /events` consumers.

```bash
LEDGER_BALANCE_WATCHES="cust-123:USD:50000:,treasury:USD::10000000" ./ledger-api-go
# post cust-123's USD balance below $500.00, then:
curl -s localhost:8080/metrics | grep balance_watch
# ledger_balance_watch_breaches_total 1
```

### Intraday liquidity positions (`GET /liquidity/intraday`)

Per-(account, currency) read-only forecast of one UTC day's net cash
position, for treasury / liquidity operations: the day's journaled
inflows and outflows (debit/credit legs), the outflow reserved by
authorization holds still outstanding at the end of the day, the nominal
transfer-schedule runs firing that day (an overdue schedule contributes
its one catch-up run, never a backfill of the missed gap), and the
principal plus accrued interest of term deposits maturing that day. The
report is strictly read-only — no journal rows, no version bump, no
balance changes. Query with `?date=YYYY-MM-DD` (defaults to today); the
`POST /reconcile` report carries the same table as `intraday_liquidity`
for the report's day.

A per-(account, currency) low-liquidity floor is configured with the
`ledger.SetLiquidityAlertThreshold` / `ClearLiquidityAlertThreshold` Go
API (structural config, like balance watches: no version bump, snapshot
round-tripped, CRUD audited). `ledger.EvaluateLiquidityAlerts` compares
each configured floor against the day's net position and emits one
structured `low_liquidity_alert` audit event per breach through the same
alert channel as the balance watches — advisory, never a rejection,
edge-triggered with silent re-arm on recovery — and bumps
`ledger_low_liquidity_alerts_total`.

```bash
curl -s "localhost:8080/liquidity/intraday?date=2026-10-15" | head -c 400
# {"date":"2026-10-15","generated_at":"...","positions":[{"account":"cust", ...
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

#### Partial captures

E-commerce and hotel pre-authorizations settle in pieces — a $500 hold
settles as a $320 room charge on day 3 and a $95 minibar charge on day
5. `POST /holds/{id}/capture/partial` is that flow: it settles
`amount_cents` (bounded by the hold's *remaining* cents) as an ordinary
double-entry posting from the held account to the payee, and the hold
stays active with its remainder still reserving available funds until
the cumulative captures reach the original hold amount.

```bash
curl -s -X POST localhost:8080/holds/<id>/capture/partial \
  -d '{"capture_seq":1,"to_account":"hotel","amount_cents":32000}'
# 201 {"capture_id":"<id>/capture/1","hold_id":"<id>","capture_seq":1,
#      "captured_cents":32000,"total_captured_cents":32000,
#      "remaining_cents":18000,...}
curl -s -X POST localhost:8080/holds/<id>/capture/partial \
  -d '{"capture_seq":2,"to_account":"hotel","amount_cents":9500}'
# 201 {"remaining_cents":8500,...}  # hold still active
```

The `(holdID, captureSeq)` pair is the operation's identity:
`capture_seq` is caller-allocated (1-based, need not be consecutive),
the journal entry is booked under the deterministic
`<holdID>/capture/<seq>` ID, and replaying the same pair returns the
original receipt with `duplicate: true` — the same logical partial
capture can never double-book. Each partial capture runs the same risk
checks as a full capture (denylist, frozen, overdraft, budget, period)
and bumps the version with one audit-chain link; the hold's
`CapturedCents` accumulates on the hold row, `Available` counts only
the un-settled remainder, and `Reconcile` reports per-hold
`partial_captures` (held / captured / remaining cents, capture count,
status). Every `POST /holds/{id}/capture/partial` is counted by
`ledger_partial_captures_total`.

### Transfer disputes (chargeback-style, 转账争议)

Payer-initiated chargeback-style claims against a posted same-currency
transfer. Raising a dispute freezes the disputed amount on the **payee's**
available balance through an authorization hold — no journal entry is
booked, so a dispute can never double-charge. Resolving a dispute is
terminal and one-way: `open` → `resolved`, never back.

```
┌──────┐  POST /disputes                ┌──────┐
│ open ├───────────────────────────────►│resolved│
└──┬───┘  (hold "dispute/<id>" freezes   └──┬───┘
   │     the disputed amount on payee)      │
   │                                        │
   │          POST /disputes/{id}/resolve   │
   │          {"uphold": true} → refund the payer via a partial
   │          reversal of the transfer's principal entry, then
   │          release the hold
   │          {"uphold": false} → release the hold only;
   │          the payee keeps the funds
   ▼
  disputed amount reserved from payee's available balance
  while open ("争议期间资金不可动") — plain hold semantics,
  no extra gating: new holds on the payee fail with
  insufficient available funds while the freeze is active
```

Endpoints:

- `POST /disputes` — raise. Body: `dispute_id`, `transfer_id`,
  `reason_code` (`unauthorized` / `not_received` / `duplicate` /
  `fraud`), `amount_cents` (positive, at most the transfer's amount —
  partial disputes are allowed), optional `currency` (defaults to the
  transfer's currency and must match it when given), optional
  `idempotency_key`. The disputed transfer must be a posted
  same-currency transfer principal; cross-currency transfers cannot be
  disputed and land on 404 like unknown IDs. Returns `201` with the
  dispute, `200` with the original on idempotent replay (repeated
  dispute ID or idempotency key).
- `POST /disputes/{id}/resolve` — resolve. Body: `{"uphold": bool}`.
  Returns `200` with the resolved dispute; `404` on an unknown ID;
  `409` when the dispute is already resolved (resolution is terminal).
- `GET /disputes/{id}` — read the dispute with its current status.

Error mapping follows the ledger's conventions: `400` for malformed
requests (empty IDs, non-positive amount, unknown reason code, amount
above the transfer's, currency mismatch), `404` for unknown dispute or
transfer, `409` for terminal-state conflicts (double resolution, a
transfer pending dual-control review), `403` when the payee's account is
frozen/closed/suspended, `422` when the payee's available funds cannot
cover the freeze.

```bash
# Raise: freeze 6000 of the payee's available balance on transfer t1.
curl -s -X POST localhost:8080/disputes \
  -d '{"dispute_id":"d1","transfer_id":"t1","reason_code":"unauthorized","amount_cents":6000}'
# 201 {"id":"d1","transfer_id":"t1","status":"open","hold_id":"dispute/d1",...}
# GET /accounts/payee/balance now reports balance_cents 10000,
# available_cents 4000 — the 6000 is frozen, not moved.

# The payer wins: refund the 6000 via a partial reversal of t1's
# principal entry (t1/reversal/1), then release the hold.
curl -s -X POST localhost:8080/disputes/d1/resolve -d '{"uphold":true}'
# 200 {"id":"d1","status":"resolved","upheld":true,...}

# The claim is rejected: release the hold only, the payee keeps the funds.
curl -s -X POST localhost:8080/disputes/d2/resolve -d '{"uphold":false}'
```

Disputes are distinct from dunning: dunning is *merchant-initiated*
collections escalation on overdue receivables and never moves money;
disputes are *payer-initiated* claims that freeze the payee's funds and
can force a refund.

Observability: every raise emits a `dispute_raised` audit event and a
`dispute_raised` outbox event; every resolution emits
`dispute_resolved` (carrying the uphold decision and the reason) plus
the outbox twin. `GET /metrics` exposes
`ledger_disputes_raised_total` and `ledger_disputes_resolved_total`.

### Term deposits (定存)

A bank-style time deposit: `POST /term-deposits` locks a principal for a
fixed term at an agreed rate. Like holds, the principal locks
**available** funds off-journal (`available = balance − active holds −
active term deposits`) without moving money through the journal — the
interest accruals are the only journal rows, booked one per day per
deposit via `POST /term-deposits/{id}/accrue` (optional `?asof=`,
defaults to now). Interest for a day is
`⌊principal × rate_bps / 10000 / 365⌋`, journaled debit the depositor /
credit the interest-expense account, with a deterministic entry ID
`term-interest/<deposit-id>/<YYYY-MM-DD>` so same-day replays return
`200` without double-booking. Sub-dust accruals (a day's interest of 0)
are rejected with `400` instead of booking a zero row.

Lifecycle: `active → matured | withdrawn_early`. `POST
/term-deposits/{id}/mature` unlocks the principal once `now ≥ matures_at`
(`422` otherwise, surfacing `ErrTermNotMatured`); `POST
/term-deposits/sweep` runs the batch job over every deposit: accrue the
day, then mature anything due. `POST /term-deposits/{id}/withdraw`
closes a deposit early with an optional `penalty_bps` (0–10000, default
0); the penalty posts debit the fee-revenue account / credit the
depositor and the principal unlocks — previously accrued interest rows
are kept, never clawed back.

Two fail-closed rules: accruals require the interest-expense account
(`LEDGER_TD_INTEREST_EXPENSE`, `500` on a misconfigured server), and a
positive early-withdrawal penalty requires the fee-revenue account
(`LEDGER_TRANSFER_FEE`) or the withdrawal is rejected. Frozen accounts
cannot open deposits (`403`); the frozen gate runs after the idempotency
replay check, so replays of keys booked before the freeze still succeed.
IDs must be slash-free (deterministic accrual IDs contain slashes) and
unique — a second open under a claimed ID returns `409` — while
idempotency keys live in their own namespace per LG convention and honor
`LEDGER_IDEMPOTENCY_TTL`. Deposits are off-journal and never bump the
ledger version; both the reconcile report and full snapshots disclose
them (`term_deposits` section, `snapshotNS term_deposit`), and the
dry-run twin copies deposit state so what-if postings respect the locks.

**Rollover (automatic renewal):** pass `"rollover":{"mode":"principal"|"principal_plus_interest","term_days":30,"max_rollovers":3}`
on `POST /term-deposits` to renew automatically. When the maturity sweep
matures the deposit it opens the next term immediately — same account and
rate, fresh tenor from the policy — under the deterministic ID
`<deposit-id>/roll/<n>` (`n` 1-based; chained renewals nest:
`td-1/roll/1/roll/2`). `principal` re-locks the original principal;
`principal_plus_interest` compounds by adding the interest the journal
booked for the maturing deposit. The renewal runs the full open commit
path (closed/frozen/suspended gates + available-funds check), so it can
fail while the original still matures — maturing is never held hostage
by the renewal; successes audit as `term_deposit_rolled_over`, failures
as `term_deposit_rollover_skipped` with the reason. Only the sweep rolls
(`POST /term-deposits/sweep`); maturing one deposit by hand stays a plain
release. Rollover IDs contain slashes (system-generated; operator IDs
stay slash-free) — URL-encode them as `%2F` in HTTP paths — and interest
rows attribute by exact deposit-ID segment, so a child's accruals never
leak into the parent's totals.

**Tiered rates (LG-145):** pass `"rate_tiers":[{"min_cents":0,"rate_bps":500},{"min_cents":1000000,"rate_bps":300}]`
instead of `rate_bps` (the two are mutually exclusive — a deposit with
`rate_tiers` leaves `rate_bps` at 0). Each principal band
`[min_cents, next_min_cents)` earns its own annual rate: one day's
interest is the sum over bands of `⌊band × rate_bps / 10000 / 365⌋`
(per-band floor, overflow-safe), so a ¥15,000 deposit on the ladder
above books `⌊1000000×500/10000/365⌋ + ⌊500000×300/10000/365⌋ = 136 + 41 = 177`
cents/day. The ladder reuses the `InterestTier` shape from the tiered
accrual engine (LG-114): first band must start at 0, minima strictly
ascending, rates 1–10000 bps, validated at open. Rollover keeps the
ladder — a tiered deposit renews as a tiered deposit, never silently
flattening. The reconcile report (`term_deposits` section) and the open
audit event disclose the ladder; the deterministic interest entry IDs
(`term-interest/<id>/<date>`) are unchanged.

```bash
curl -s -X POST localhost:8080/term-deposits \
  -d '{"id":"td-1","account":"saver","amount_cents":400000,"term_days":30,"rate_bps":500,"idempotency_key":"k1"}'
# 201 {"id":"td-1","status":"active","matures_at":"...","principal_cents":400000,...}
curl -s localhost:8080/accounts/saver/balance
# {"account":"saver","balance_cents":1000000,"available_cents":600000,"frozen":false}
curl -s -X POST localhost:8080/term-deposits/td-1/accrue?asof=2026-03-10T12:00:00Z
# 201 {"id":"term-interest/td-1/2026-03-10",...,"amount_cents":54}
curl -s -X POST localhost:8080/term-deposits/td-1/withdraw -d '{"penalty_bps":100}'
# 201 {"deposit":{"status":"withdrawn_early",...},"penalty_cents":4000}
```

Status codes: `201` first booking / `200` idempotent replay or
re-withdraw; `400` malformed request, bad currency/rate/penalty, or
dust accrual; `403` frozen account; `404` unknown deposit; `409` ID
conflict or duplicate open; `422` insufficient available funds,
maturing before `matures_at`, withdrawing a matured deposit, or
accruing after maturity; `500` interest-expense/penalty account not
configured.

### Payout instructions (出款)

The operator lifecycle of money leaving the ledger to an external
destination: `draft → approved → submitted → settled | failed`, with
`cancelled` reachable from draft/approved. Drafting (`POST /payouts`)
is off-journal — it locks nothing; the instruction carries the payee
account, amount, currency, an external destination reference
(LG-122 style, e.g. `"FPS:6222000012345678"`), the maker, and an
optional idempotency key (own namespace, honors
`LEDGER_IDEMPOTENCY_TTL`).

Approval (`POST /payouts/{id}/approve`) runs maker-checker against the
LG-42 dual-control review threshold: at or above the payee's threshold
the approver must differ from the maker (`403` otherwise); below it any
approver may sign, and the approver is always recorded.

Submit (`POST /payouts/{id}/submit`) earmarks the funds by posting
**debit payout-clearing / credit payee** through the full Post gate
order — denylist, replay, VAN, chart direction, closed, dormant,
frozen, suspended, overdraft, daily-limit, period — so the instruction
cannot route around the ledger's risk controls. The legs mirror a
transfer `payee → clearing` under the ledger's transfer convention
(debit = balance up): the payee's balance drops, the clearing account
holds the earmark. The clearing account is fail-closed
(`LEDGER_PAYOUT_CLEARING_ACCOUNT`, `500` when unconfigured). The entry
ID is the deterministic `payout/<id>/clearing`.

Settle (`POST /payouts/{id}/settle`) requires bank-statement evidence:
the supplied statement line must be an outflow that matches the
clearing entry through the statement-matching machinery (LG-107) —
exactly one candidate, and it must be the clearing row itself, else
`422`. On settle the suspense reverses — debit payee / credit clearing
("冲待清算", `payout/<id>/settle`) — so the clearing account nets to
zero; the economic outflow is evidenced by the matched statement line,
not by a second journal leg. `POST /payouts/{id}/fail` does the same
reversal for a bank rejection (`payout/<id>/fail`) but keeps the failed
status with the reason, so the books never show money as left when it
did not. Every transition audits (`payout_open/approved/submitted/
settled/failed/cancelled`), emits an outbox event, and is idempotent;
the reconcile report discloses every instruction (`payouts` section).
Instructions are in-memory only (like clearing batches) — the journaled
legs survive snapshots, and the fail-closed clearing account
round-trips.

```bash
curl -s -X POST localhost:8080/payouts \
  -d '{"id":"po-1","payee_account":"saver","amount_cents":250000,"destination":"FPS:6222000012345678","maker":"alice"}'
# 201 {"id":"po-1","status":"draft",...}
curl -s -X POST localhost:8080/payouts/po-1/approve -d '{"approver":"bob"}'
curl -s -X POST localhost:8080/payouts/po-1/submit
# 200 {"status":"submitted","clearing_entry_id":"payout/po-1/clearing",...}
curl -s -X POST localhost:8080/payouts/po-1/settle \
  -d '{"statement":{"id":"stmt-1","account":"saver","currency":"USD","direction":"out","amount_cents":250000,"posted_at":"2026-10-19T12:00:00Z"}}'
# 200 {"status":"settled","statement_line_id":"stmt-1",...}
```

Status codes: `201` first draft / `200` replay or idempotent
transition; `400` validation errors; `403` maker-checker violation or
frozen/closed/suspended on the submit/settle legs; `404` unknown
payout; `409` ID conflict or invalid state transition; `422`
statement mismatch or overdraft/daily-limit/period rejection;
`500` payout clearing account not configured.

### ACH returns (dishonored entries / 退票拒付)

The other side of an ACH origination: the bank answers the NACHA
file with a return file when a receiver's bank dishonors an entry
(`R01` insufficient funds, `R02` account closed, … `R85`). The
return file is the same 94-character NACHA layout, with each
dishonored Entry Detail (6) followed by an Addenda Type 98 record
carrying the return reason code, the original entry's trace number,
and the original receiving DFI.

`POST /ach/returns` books a bank return file (JSON body
`{"file": "<94-char records joined by \n>"}`). The file parses with
`ledger.ParseNACHAReturn` — a malformed line or a reason code
outside `R01`–`R85` refuses the whole file with `400` (fail-loud: a
return that goes unbooked would leave the books claiming money that
never moved). Entries with no 98 addenda are not returns and are
skipped. Each return is then matched to its payout by the entry's
individual ID (stamped by `ledger.PayoutToNACHAEntry`) and applied
through `ledger.ProcessACHReturns`:

- the payout must be submitted (its clearing entry journaled), else
  a per-item rejection;
- the return amount must equal the clearing entry's amount (a
  mismatch is a per-item rejection — the return is evidence about a
  different entry);
- the clearing entry is reversed through `ReverseEntry` with the
  deterministic idempotency key
  `ach-return/<trace>/<reason>/<payout>` — replays return
  `duplicate: true` and book nothing new;
- a **return of a return** (二次退回) is explicitly rejected per
  item: the clearing entry is already a reversal, or already
  carries one — NACHA forbids returning a returned entry, and the
  ledger cannot correct twice.

Processing is per-item: one bad return is recorded in its item's
`error`, the others still book. Every first-time return emits an
`ach_return` audit event (reason code, trace, payout ID, reversal
ID — the original clearing row, the reversal row, and the
`reversal_of` entry link all stay in the journal, so the dishonored
payout leaves a fully reconcilable trail) and bumps
`ledger_ach_returns_total{reason_code}` on `GET /metrics`. Go ledger
API plus this operator endpoint (the `x-ledger-actor` header lands
in the hash-chained audit events).

```bash
curl -s -X POST localhost:8080/ach/returns \
  -H 'x-ledger-actor: ops' \
  -d '{"file": "<the bank return file>"}'
# 200 {"results":[{"payout_id":"po-1","reason_code":"R01",
#      "trace_number":"121042880000001",
#      "reversal_id":"payout/po-1/clearing/reversal",
#      "amount_cents":250000,"duplicate":false}]}
```

Status codes: `200` with the per-item results (a rejected return
carries its error in the item, not in the status); `400` malformed
return file, invalid reason code, or a file with no returns.

### FedNow inbound instant-payment receipts (pacs.008 / 入账通知)

The other side of an instant payment: FedNow settles in seconds and
the receiving bank learns about it from a `pacs.008.001.08`
(`FIToFICstmrCdtTrf`) message — one `CdtTrfTxInf` per credit
transfer, carrying the `EndToEndId`, the settlement amount, and the
debtor/creditor accounts.

`POST /fednow/pacs008` books a pacs.008 notification (JSON body
`{"pacs008": "<the XML document>"}`). The document parses with
`ledger.ParsePacs008` — namespace-tolerant across
`pacs.008.001.xx` revisions, one item per `CdtTrfTxInf`; a malformed
document refuses the whole request with `400` (fail-loud: an
unbooked receipt would leave the books understating what the bank
received). Each item is then applied with
`ledger.ProcessPacs008`:

- the item's `EndToEndId` is associated with the originating
  transfer/payout via its idempotency key (payout by ID, then payout
  by idempotency key, then transfer by idempotency key, then transfer
  by ID — the same convention the pain.001 export uses for
  `EndToEndId`);
- when an original is found, the notification amount/currency must
  equal the original's and the payee must be one of the original's
  accounts, else an explicit per-item rejection;
- without an association, the payee must already be a known ledger
  account (an inbound payment cannot create a ledger identity out
  of thin air);
- the receipt books a payee-inflow entry
  (`fednow-pacs008/<endtoendid>`, idempotent across replays)
  against the configured FedNow settlement account
  (`ledger.SetFedNowSettlementAccount` — unset is fail-closed,
  like the ECL and webhook booking accounts);
- the entry goes through the full `Post` gate (balance rules,
  velocity checks, posting rules), and the operator identity from
  `x-ledger-actor` lands in the hash-chained
  `fednow_pacs008_posted` / `fednow_pacs008_rejected` audit events.

Processing is per-item: one bad notification is recorded in its
item's error, the others still book. Receipts bump
`ledger_fednow_postings_total`; rejections bump
`ledger_fednow_rejections_total{reason}` on `GET /metrics`. Go ledger
API plus this operator endpoint.

```bash
curl -s -X POST localhost:8080/fednow/pacs008 \
  -H 'x-ledger-actor: ops' \
  -d '{"pacs008": "<the pacs.008 document>"}'
# 200 {"results":[{"end_to_end_id":"E2E-1","entry_id":
#      "fednow-pacs008/E2E-1","amount_cents":25000,
#      "currency":"USD","payee_account":"acct-payee",
#      "duplicate":false}]}
```

Status codes: `200` with the per-item results (a rejected item
carries its error in the item, not in the status); `400` malformed
document or empty document.

### Installment plans (分期)

A BNPL-style scheduled receivable: `OpenInstallmentPlan` splits a
principal into `NumPeriods` equal collections from a payer to a payee on
a `monthly` / `weekly` / `daily` billing rule anchored at `FirstDueAt`.
Each period collects `⌊principal/periods⌋`, the final period absorbing
the division remainder so the periods sum to exactly the principal — no
dust created or lost. `InstallmentsDue(now)` is the sweeper: it books
every period whose due date has arrived through the existing atomic
`Post` path, one journal row per period as of its due date (a late
sweep backdates honestly — the obligation existed at the due date).
Collections follow the transfer convention (payee debited, payer
credited).

Lifecycle: `active → overdue → active → completed`, plus
`active|overdue → settled_early` via `SettleInstallmentEarly`, which
closes the plan in one row for the remaining principal. A rejected
period posting (frozen account, overdraft, …) moves the plan to
`overdue` and stops that plan's sweep — later periods wait rather than
skipping the queue; a later sweep that posts every outstanding period
recovers it to `active`. Every transition emits an audit event
(`installment_overdue`, `installment_recovered`, `installment_completed`,
`installment_settled_early`, `installment_plan_open`).

Idempotency: period rows carry the deterministic entry ID and
idempotency key `installment/<plan-id>/<seq>` (early-settle:
`installment/<plan-id>/early-settle`), so a blind scheduled sweeper
replays original rows instead of double-booking; plan opens carry their
own key namespace honoring `LEDGER_IDEMPOTENCY_TTL`. Plans are
in-memory only and do not round-trip through disaster-recovery
snapshots — the journaled period rows do, so the money is safe.

### Installment late fees (LG-150)

Overdue money is free money for the payer — unless the plan opts in:
`SetInstallmentLateFee(planID, InstallmentLateFeeRule)` arms one plan
with a late-fee rule (`{fee_account, daily_rate_bps, fixed_cents_per_day,
max_total_cents}`), and `AccrueInstallmentLateFees(asof)` books one
day's fee per overdue-but-unposted period on every sweep:

```go
l.SetInstallmentLateFee("ip-2026-001", ledger.InstallmentLateFeeRule{
    FeeAccount:       "fee-income",
    DailyRateBps:     2400,  // 24% annual, pro-rata per 365-day year
    FixedCentsPerDay: 50,    // flat penalty on top, plan currency cents
    MaxTotalCents:    5000,  // per-period cap, net of reversals; 0 = uncapped
})
```

Each fee is an ordinary journal row — debit `fee_account`, credit the
plan's payer (the fee-leg direction used everywhere) — with the
deterministic ID `installment-latefee/<plan-id>/<seq>/<YYYY-MM-DD>`
(UTC), so a day's sweep is idempotent: replays return the same rows
with `duplicate: true`. Fees post through the full Post gate order
(field validation, idempotency replay, chart direction, frozen,
overdraft, daily-limit, period) via `postSystemLocked`, never through
posting rules or dual-control review — a frozen payer, overdraft, or
closed period blocks the fee like any other booking, and a failed fee
is reported on the result while the sweep continues with the next
period. The rule is validated at set time (plain account name, rate in
`[0,10000]` bps, non-negative fixed/cap, and it must actually charge
something); `ClearInstallmentLateFee` removes it (already-booked rows
are real history and stay), and `InstallmentLateFee` reads it back.
Set/clear emit `installment_late_fee_set` /
`installment_late_fee_cleared` audit events; the rule is structural
configuration, like the other operator knobs — no version bump, no
snapshot round-trip.

Late fees are bookkeeping, not collections: the LG-118 dunning
escalations (reminders, suspension threats, legal holds) are the
operational counterpart and stay untouched.

### Credit lines (授信)

`POST /credit-lines` grants a per-account, per-currency credit line
(`limit_cents`, `rate_bps`) — the overdraft-floor counterpart to term
deposits. For an overdraft-protected account the balance floor drops
from `0` to `−limit` in the line's currency: postings are rejected only
when `balance < amount + floor`, i.e. the balance may rest exactly at
`−limit` but never below. Breaching the floor returns the dedicated
`ErrCreditLimitExceeded` (`422`) rather than the plain
`ErrAccountOverdraft`, so clients can distinguish "overdrawn" from
"past the credit line"; accounts without a line keep the zero floor
unchanged. The floor is strictly per-currency (a USD line does not cover
EUR postings), unprotected accounts ignore the line for posting but still
track utilization, and a frozen account stays fully blocked — the line
never un-freezes anything.

Grants are create-only: a second grant for the same account+currency
returns `409` (`ErrCreditLineExists`); adjust with `POST
/credit-lines/reduce`, which lowers the limit (rejecting any limit below
current utilization with `422`, `ErrCreditLineBelowUtilization`) and
revokes the line entirely at `0` (`GET` then returns `404`). `GET
/credit-lines?account=&currency=` reports `limit_cents`, `used_cents`
(the negative-balance portion in that currency), and
`available_cents`. `POST /credit-lines/accrue-interest` books one day
of interest on the utilization —
`⌊used × rate_bps / 10000 / 365⌋`, debit the interest-income account /
credit the borrower, deterministic ID
`credit-interest/<account>/<currency>/<YYYY-MM-DD>` — and is
fail-closed: it requires the income account
(`LEDGER_CREDIT_INTEREST_INCOME`, `500` unconfigured) and rejects an
interest charge that would itself breach the floor with
`ErrCreditLimitExceeded` instead of posting it. Lines and the income
account round-trip through snapshots and are disclosed in the reconcile
report's `credit_lines` section.

```bash
curl -s -X POST localhost:8080/credit-lines \
  -d '{"account":"trader","limit_cents":50000,"currency":"USD","rate_bps":800}'
# 201 {"account":"trader","currency":"USD","limit_cents":50000,"rate_bps":800,...}
curl -s "localhost:8080/credit-lines?account=trader&currency=USD"
# {"account":"trader","limit_cents":50000,"used_cents":40000,"available_cents":10000}
curl -s -X POST localhost:8080/credit-lines/reduce \
  -d '{"account":"trader","currency":"USD","limit_cents":0}'   # revoke
# 200 {"revoked":true,...}
```

Status codes: `201` first grant / `200` reduce; `400` malformed request
or bad account/currency/rate; `404` unknown line; `409` duplicate grant;
`422` reducing below utilization, interest breaching the floor, or
posting past the credit limit.

### ECL provisions (预期信用损失)

`POST /ecl/exposures` registers a manual credit exposure for an obligor
without a credit line (`exposure_cents`, `days_past_due`) — the
standalone source. Where a credit line exists, its drawn utilization is
the exposure instead (no registration needed); a manual exposure on the
same obligor takes precedence. `POST /ecl/exposures/dpd` updates the
days-past-due tracking on either source.

`POST /ecl/provisions` books one day's IFRS 9 expected credit loss:
the exposure is staged 1/2/3 by days past due under the configurable
`ECLStageConfig` (default 30/90-day buckets, 1%/10%/50% provision
rates; `POST /ecl/config` replaces it after validation), and the
provision is `⌊exposure × stage_rate_bps / 10000⌋`, debit the
credit-loss expense account / credit the loss-allowance account, under
the deterministic ID `ecl/<account>/<currency>/<YYYY-MM-DD>` — a day's
run is idempotent, replays return the original row with `200`. Both
booking accounts are fail-closed (`LEDGER_ECL_EXPENSE`,
`LEDGER_ECL_ALLOWANCE`; `500` unconfigured), dust provisions book
nothing (`400`), and stage changes emit `ecl_stage_migration` audit
events. `GET /ecl/exposures` lists the tracked exposures; the
reconcile report discloses every exposure's stage/exposure/provisioned
figures plus the per-currency provision-coverage ratio
(`provisioned/exposure`).

```bash
curl -s -X POST localhost:8080/ecl/exposures \
  -d '{"account":"borrower","currency":"USD","exposure_cents":100000,"days_past_due":45}'
# 201 {"account":"borrower","stage":2,...}
curl -s -X POST "localhost:8080/ecl/provisions?asof=2026-04-02T09:00:00Z" \
  -d '{"account":"borrower","currency":"USD"}'
# 201 {"id":"ecl/borrower/USD/2026-04-02","amount_cents":10000,...}
```

Status codes: `201` first provision / `200` replay; `400` malformed
request, invalid stage config, or dust provision; `404` no tracked
exposure; `500` unconfigured expense/allowance account.

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

The conversion price is **locked at request admission**: `POST /transfers`
snapshots the effective rate before taking the write lock, and the
transfer settles at that admitted price even if the rate table moves
before commit (a `SetFXRate` between admission and commit is a real
window — both mutate under the same lock). The drift between the live
rate and the admitted quote is measured in basis points (exact `big.Rat`
math): the receipt discloses `fx.slippage_bps` plus the admitted quote
(`fx.admitted_rate_num/den`, `fx.admitted_effective_version`,
`fx.admitted_at`), and the `transfer` audit event seals the full detail
(`admitted_rate`, `committed_rate`, `slippage_bps`, and the caller's
`max_slippage_bps` when set). The opt-in `max_slippage_bps` request field caps the
drift: a transfer whose live rate moved past the cap is rejected with
`422` and records nothing. A negative cap is rejected with `400`. A
conversion that settled at an approved manual rate override keeps the
override's full provenance (`proposed_by`, `approved_by`, `reason`) in
the `fx_rate_overridden` audit event.

#### Two-sided dealing prices and spread P&L

A rate can carry two-sided dealing prices on top of the reference mid:
`SetFXBidAskRate(from, to, bidNum, bidDen, askNum, askDen)` installs a
bid and an ask with `0 < bid ≤ mid ≤ ask` (exact rational checks — a
spread that doesn't bracket the mid is rejected), or
`TwoSidedFromMidSpread(midNum, midDen, spreadBps)` derives an exact
symmetric quote from a mid plus a spread width in basis points. A rate
without bid/ask is mid-only and behaves exactly as before.

A cross-currency transfer names its dealing side with `fx_side`:
`"sell"` (the default) converts at the **bid**, `"buy"` at the
**ask**. Anything else is a `400`. The spread — `floor(amount·mid) −
floor(amount·bid)` on a sell, `floor(amount·ask) − floor(amount·mid)`
on a buy — books as its own double-entry leg `<transfer_id>/spread`:
debit the FX account, credit the FX spread-income account (the
transfer's `fx_income_account`, or the ledger-wide account), in the
target currency. Each currency's books stay balanced independently, and
the dealer's revenue lands in a real income account instead of hiding
in the clearing account. A positive spread with no usable income
account is rejected with `400` before anything is recorded
(fail-closed); a zero spread (mid-only rate, active override, or dust)
books no leg.

The receipt's `fx` field discloses `direction`, the mid, the full
two-sided quote, `spread_bps` (exact `big.Rat`), `spread_cents`, and
`fx_income_account`; the `transfer` audit event seals the same detail.
`POST /reconcile` reports the realized per-currency spread P&L in
`fx_spread_pnl` (settled legs, net of direct reversals), and
`ledger_fx_spread_bookings_total` / `ledger_fx_spread_cents_total`
count the bookings. The admission quote is side-correct (the slippage
gate measures drift on the dealing price), provider feed quotes stay
mid-only, and the triangular-consistency scan keeps checking the mids —
a mid-consistent table can't offer the customer a triangular arbitrage.

```bash
curl -s -X POST localhost:8080/transfers \
  -d '{"transfer_id":"fx-002","from_account":"alice","to_account":"erin","amount_cents":10000,"currency":"USD","to_currency":"EUR","fx_side":"sell"}'
# 201 {...,"fx":{"direction":"sell","bid_num":108,"bid_den":100,"ask_num":112,"ask_den":100,"spread_bps":363,"spread_cents":200,...}}
```

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

`{id}` is an account reference: a registered alias resolves to the
account it names, anything else is used as the account ID verbatim (see
Account aliases below). When the account has an alias, the response
echoes it as `alias` (omitted when the account has none):

```bash
curl -s localhost:8080/accounts/ops_cash/balance
# {"account":"cash","balance_cents":1000,"available_cents":1000,"frozen":false,"alias":"ops_cash"}
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

Like `/balance`, `{id}` accepts an alias, and the response echoes the
account's alias as `alias` when one is bound (omitted otherwise).

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
- `ACCTID` names the account's external identifier when one is mapped in
  the `ofx` system (see External account mappings below) — the identifier
  the importing bank or accounting package expects for the account. An
  unmapped account exports with its internal account ID, as before.
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

### `GET /exports/pain001` — ISO 20022 pain.001 payment instruction export

A `pain.001.001.03` customer credit transfer initiation generated from
the journaled transfer principals (Go: `ledger.ExportPain001`) — the
payment instruction a corporate banking channel uploads. Query
parameters: `from` / `to` (optional RFC 3339 time bounds, half-open
`[from, to)`, the OFX export convention), `debtor` / `creditor`
(optional payer/payee account filters), `currency` (optional ISO 4217
code).

```bash
curl -s 'localhost:8080/exports/pain001?currency=USD' -o payments.xml
```

Semantics, stated so downstream banks and importers can trust them:

- One `CdtTrfTxInf` per transfer; `EndToEndId` is the transfer ID —
  stable across exports, so importers can dedupe on it. The debtor is
  the transfer's payer (`From`), the creditor the payee (`To`).
- Debtor/creditor identifiers are the **registered IBANs** (see
  `ledger.SetAccountIBAN`): ISO 13616 mod-97 validated at registration,
  optional ISO 9362 BICs ride `DbtrAgt` / `CdtrAgt`. Fail-closed: a
  payment whose debtor or creditor has no registered IBAN refuses the
  whole export (`422`) rather than emitting a forged identifier.
- Amounts are the ledger's integer cents rendered as exact decimals —
  no floats anywhere. `CtrlSum` is the overflow-checked sum of every
  exported instruction; payments group into one `PmtInf` per (debtor,
  currency), each with its own `NbOfTxs` / `CtrlSum`.
- `MsgId` is deterministic in (window, filters, content): re-exporting
  the same window renders byte-identical output, and any content change
  changes the ID.
- Pending-review transfers are excluded — a payment instruction must
  never be generated from a frozen transfer. Re-export after approval
  includes it. Split-transfer chunks export as individual instructions;
  cross-currency transfers settle through the FX clearing account as two
  legs (not one direct debtor-to-creditor payment) and are excluded.
- The export is read-only: it takes the read lock, bumps no version,
  appends no chain link, and consumes no idempotency key. Like
  `Reconcile`, it still emits one structured audit event
  (`pain001_exported`, details `msg_id` / `tx_count` /
  `ctrl_sum_cents` / `currency`) so the compliance trail records every
  generated instruction.

A malformed timestamp, an inverted window, or an invalid currency is a
`400`, fail-fast. `GET /metrics` counts exports as
`ledger_pain001_exports_total`.

### pain.001 付款指令状态机（LG-99）

每次 `GET /exports/pain001` 导出都会在账本内注册一条付款指令
（`PainInstruction`），以确定性的 `MsgId` 为键。同一参数、同一内容
的重复导出是幂等的：返回已有记录，不产生新的注册、不写审计事件。

**指令级状态**：`registered` → `exported` → `accepted` / `rejected` →
`settled`。其中 `rejected` 与 `settled` 为终态——被银行拒绝或已结算
的指令不会复用同一 `MsgId` 重发，修正原因后重新导出会得到新的
`MsgId`。

**逐笔（per-tx）状态**（合法转移表）：

| 当前状态 | 银行回执 / 操作 | 新状态 |
|---|---|---|
| `exported` | `accepted` | `accepted` |
| `exported` | `rejected`（可带 `reason`，原文保存） | `rejected` |
| `accepted` | 对账确认（`POST .../settle`） | `settled` |

- 同一状态的回执重放是 no-op（幂等），不产生转移、不写审计事件。
- 非法转移（如 `accepted` → `rejected`、`settled` 之后再收回执）整批
  拒绝，返回 `422`，记录保持原样（回执按整批原子应用）。
- 指令级状态由逐笔状态汇总得出：全部 `settled` → `settled`；全部
  `accepted`/`settled` → `accepted`；全部 `rejected` → `rejected`；
  其余（有未回执或混合）保持 `exported`。
- `POST .../settle` 只把银行已 `accepted` 的笔推进到 `settled`，
  **绝不替银行接受**：未回执（`exported`）的笔保持不动，被拒绝的笔
  保持拒绝。这是刻意设计——结算是操作员的确认动作（attestation），
  而不是自动对账扫描的副作用。

**新增端点**：

- `GET /exports/pain001/{msgid}/status` — 指令记录：`msg_id`、`state`、
  `exported_at`、`tx_count`、`ctrl_sum_cents`、`currency` 与逐笔状态。
  未知 `msg_id` → `404`。
- `POST /exports/pain001/{msgid}/receipts` — 录入银行回执（pain.002
  风格），body 为 `{"receipts":[{"tx_id":"…","status":"accepted|rejected","reason":"…"}]}`。
  空批量 → `400`；未知 `msg_id` → `404`；未知 `tx_id`、未知状态、非法
  转移 → `422`。操作员身份来自 `x-ledger-actor` 请求头。
- `POST /exports/pain001/{msgid}/settle` — 操作员对账确认，把已接受
  的笔推进到 `settled`。未知 `msg_id` → `404`。

**审计**：每次状态转移（逐笔与指令级）都会写一条哈希链审计事件，
`op` 为 `pain001_status`，`details` 包含 `msg_id` / `tx_id` / `from` /
`to`（银行给出的 `reason` 一并记录），`trace_id` 为 `msg_id`。

**对账报告**：`POST /reconcile` 的报告新增 `pain001_pending` 小节，
列出所有未达到 `settled` 终态的指令（含被拒绝的——它的钱从未结算，
操作员需要看到它去修正重发），即操作员的对账确认工作清单。

**快照**：指令注册表随快照配置 round-trip（按 `msg_id` 确定性排序），
恢复后的账本保留每条指令的回执/结算位置，可继续转移；损坏的快照
记录（未知状态、重复 `msg_id`/`tx_id`）在导入时直接拒绝。

`GET /metrics` 新增 `ledger_pain001_status_queries_total`、
`ledger_pain001_receipts_total`、`ledger_pain001_settlements_total`。

```bash
# 查状态
curl -s localhost:8080/exports/pain001/<msgid>/status | jq .state
# 录银行回执
curl -s -X POST localhost:8080/exports/pain001/<msgid>/receipts \
  -H 'Content-Type: application/json' \
  -d '{"receipts":[{"tx_id":"st1","status":"accepted"},{"tx_id":"st2","status":"rejected","reason":"AC03"}]}'
# 对账确认（结算）
curl -s -X POST localhost:8080/exports/pain001/<msgid>/settle \
  -H 'x-ledger-actor: night-ops'
```

### pain.002 银行状态回盘（LG-162）

`POST /exports/pain002/apply` 是 pain.001 出站状态报告的逆操作：解析银行返回的
ISO 20022 pain.002.001.03（`CstmrPmtStsRpt`），按 `OrgnlMsgId` 找到对应的
pain.001 付款指令，把每笔 `TxInfAndSts` 的银行 verdict 推进转账状态机。
`OrgnlEndToEndId` 即导出时的 transfer ID（EndToEndId），天然关联原指令的幂等键。

状态映射：

| TxSts | 含义 | ledger 状态 |
|---|---|---|
| `ACSC` | AcceptedSettlementCompleted（已结算） | `accepted` |
| `ACCP` | AcceptedCustomerProfile（已受理） | `accepted` |
| `RJCT` | Rejected（拒绝） | `rejected`，`StsRsnInf/Rsn/Cd` + `AddtlInf` 原文进审计事件 reason |
| `PDNG` | Pending（银行未定论） | 跳过，保持 `exported`，不记事件 |

语义：
- 解析 fail-loud：XML 非法、`OrgnlMsgNmId` 非 `pain.001.001.03`、`TxSts` 非法 → 400，整份拒绝。
- 应用复用 `RecordPain001Receipts`：整批先校验后应用、重放幂等（same-state no-op）、每笔转移记哈希链 `pain001_status` 审计事件 + outbox。
- 全 `PDNG` 的回盘是显式 no-op：返回指令不变，不记事件——pending 是信息，不是 verdict。
- 刻意**不**驱动 `PayoutInstruction`：`SettlePayout`/`FailPayout` 需要银行对账单行证据并记账冲销，是操作员 attestation，read-only 的回盘不能代行；`GrpSts` 仅作参考，与逐笔 verdict 不一致时以逐笔为准。
- `404` 未知 `OrgnlMsgId`；`422` 未知转账/非法转移。`x-ledger-actor` 取操作员身份。计数器 `ledger_pain002_applies_total`。

```bash
curl -s -X POST localhost:8080/exports/pain002/apply \
  -H 'Content-Type: application/xml' -H 'x-ledger-actor: night-ops' \
  --data-binary @bank-status-pain002.xml | jq '{state, txs: [.txs[] | {tx_id, state}]}'
# 200 {"state":"exported","txs":[{"tx_id":"st1","state":"accepted"},{"tx_id":"st2","state":"rejected"}]}
```

### SWIFT gpi UETR 跨境追踪（`GET /gpi/track/{uetr}`）

每笔 payout 指令都是跨境支付发起：`POST /payouts` 起草时自动分配
UETR（Unique End-to-end Transaction Reference，RFC 4122 UUID v4，
`crypto/rand` 生成），并打开一条 gpi 跟踪记录，状态 `tracked`。
记录镜像该笔跨境支付在代理行链路中的旅程：

| 状态 | 含义 |
|---|---|
| `tracked` | 已跟踪（初始） |
| `ACSP` | AcceptedSettlementInProcess（银行已受理） |
| `ACSC` | AcceptedSettlementCompleted（已结算） |
| `ACCP` | AcceptedCustomerProfile（已入账受益人，终态） |
| `RJCT` | Rejected（拒绝，终态，需原因码） |

语义：
- 状态机只向前：允许跳步（如 `tracked`→`ACSC`），永不回退；
  同状态推进是幂等 no-op；`ACCP`/`RJCT` 为终态。
- `RJCT` 必须带原因码（fail-closed），原因码原文进审计——拒绝必须说明原因。
- 两条推进路径：操作员经 `POST /gpi/track/{uetr}/advance`（`{"status","reason_code?"}`，
  `x-ledger-actor` 记身份）；银行经 pain.002 回盘自动推进——`POST /gpi/link`
  把 transfer（其 EndToEndId）关联到 UETR 后，`POST /exports/pain002/apply`
  的每笔 verdict 自动镜像到 gpi 状态（`ACSC`→`ACSC`、`ACCP`→`ACCP`、
  `RJCT`→`RJCT` 带银行原因码；`PDNG` 不动）。EndToEndId 仍是 pain.001
  的幂等键；gpi 只做镜像，且迟到的回盘永不把已 `ACCP` 的记录往回拨。
- 一笔 transfer 只关联一个 UETR（冲突 409）；关联是操作员 attestation，
  进哈希链审计（`gpi_linked` / `gpi_status`）。
- 跟踪记录与 payout 指令一样是内存态（in-memory）：持久记录是哈希链
  `gpi_status` 审计事件。计数器 `ledger_gpi_tracked_total` /
  `ledger_gpi_settled_total` / `ledger_gpi_rejected_total` 进 `/metrics`。

```bash
# 起草 payout 即得 UETR
curl -s -X POST localhost:8080/payouts \
  -d '{"id":"po-x","payee_account":"saver","amount_cents":250000,"destination":"FPS:6222000012345678","maker":"alice"}' | jq .uetr
# "9f2c8e1a-3b4d-4f5a-8c6d-7e8f9a0b1c2d"

# 查询跟踪记录（含完整 transition history）
curl -s localhost:8080/gpi/track/9f2c8e1a-3b4d-4f5a-8c6d-7e8f9a0b1c2d | jq '{status, history}'

# 操作员推进（银行已受理）
curl -s -X POST localhost:8080/gpi/track/<uetr>/advance \
  -H 'x-ledger-actor: night-ops' -d '{"status":"ACSP"}' | jq .status
# "ACSP"

# 关联 transfer，后续 pain.002 回盘自动推进 gpi
curl -s -X POST localhost:8080/gpi/link \
  -H 'x-ledger-actor: night-ops' -d '{"uetr":"<uetr>","transfer_id":"st1"}' | jq .transfer_id
# "st1"
```

Go: `ledger.AdvanceGPI` / `ledger.LinkGPITransfer` / `ledger.GetGPITrack` /
`ledger.GPICounts`。

### `GET /exports/pain008` — ISO 20022 pain.008 直接扣款指令导出

`pain.001` 的反向：`pain.001` 是付款人主动贷记（credit transfer），
`pain.008.001.02`（`CstmrDrctDbtInitn`）是债权人发起的直接扣款——
merchant 凭 debtor 的授权（mandate）从 debtor 银行扣款。`ExportPain008`
把一条 mandate（代扣协议，LG-125）下已记账的扣款渲染成发给银行的
pain.008 指令文件；与 pain.001 正交（借记 vs 贷记），与 pain.002 正交
（pain.002 只回答 pain.001，`OrgnlMsgNmId` 非 `pain.001.001.03` 的回盘
会被拒绝）。

SeqTp（sequence type）按 mandate 自身扣款历史自动判定——账本是
mandate 使用情况的 system of record：

| SeqTp | 含义 | 判定 |
|---|---|---|
| `FRST` | 首次扣款 | 该 mandate 下无历史扣款 |
| `RCUR` | 周期性扣款 | 有历史扣款，且还有后续窗口 |
| `FNAL` | 末次扣款 | 本笔落在 mandate 最后一个可扣款窗口 |
| `OOFF` | 一次性扣款 | mandate 整个有效期落在单个扣款窗口内（至多只能扣一次） |

语义（SEPA 分组规则：同一 creditor、同一 `ReqdColltnDt`、同一 SeqTp
为一组 `PmtInf`，每组携带自己的 SeqTp）：
- 每笔已记账扣款一条 `DrctDbtTxInf`；`EndToEndId` 即 journal entry ID，
  跨导出稳定，银行可据此去重。
- debtor = mandate payer（客户），creditor = mandate payee（商户）；
  账户标识为注册 IBAN（BIC 走 `DbtrAgt`/`CdtrAgt`）。
- `MndtId` = mandate ID；`DtOfSgntr` = mandate `ValidFrom` 日期（债务人
  授权日）；`ReqdColltnDt` = 扣款记账日。
- `CdtrSchmeId` 必填（SEPA 强制，账本不存 creditor identifier，无
  默认值可编——缺失 fail-closed）。
- fail-closed：未知/暂停/取消/过期的 mandate 拒绝导出；缺 creditor
  scheme、缺 debtor/creditor IBAN 拒绝整个导出（不伪造标识）。
- `MsgId` 确定性（同 mandate+scheme+窗口+内容重渲字节一致）；
  `CtrlSum` 溢出检查（`int64` 上溢则拒绝导出）。
- 只读：读锁、无 version bump、无链追加、无幂等键消耗；照例记一条
  `pain008_exported` 审计事件。计数器 `ledger_pain008_exports_total`。

```bash
curl -s 'localhost:8080/exports/pain008?mandate=m-sub&creditor_scheme_id=DE98ZZZ09999999999' -o collections.xml
# 200 pain.008.001.02: <CstmrDrctDbtInitn>/<PmtInf>/<SeqTp>FRST</SeqTp>/...
```

Go: `ledger.ExportPain008`。

### `GET /exports/pacs002` — ISO 20022 pacs.002 银行间状态报告导出

gpi 跟踪的 FI-to-FI 视角：`pacs.002.001.03`（`FIToFIPaymentStatusReport`）
是银行间状态报告——与客户级的 pain.002（LG-162）正交：pain.002 回答
pain.001 给客户看，pacs.002 报告底层 FI-to-FI 转账的结算状态给代理行链路。
`ExportPacs002` 把一条 gpi 跟踪记录（LG-165）渲染成 pacs.002 报文：

状态映射（gpi 镜像，不做新 verdict）：

| gpi 状态 | TxSts |
|---|---|
| `tracked`（初始，无银行 verdict） | `ACSP`（在链路中） |
| `ACSP` | `ACSP` |
| `ACSC` | `ACSC` |
| `ACCP` | `ACCP` |
| `RJCT` | `RJCT`，`StsRsnInf/Rsn/Prtry` 携带原因码原文 |

语义：
- `TxInfAndSts/OrgnlUETR` 携带原始 UETR——UETR 是整条 pacs 链的
  end-to-end key；`OrgnlEndToEndId` 为已关联的 transfer ID（未关联时为
  payout ID）；`OrgnlGrpInfAndSts` 把原指令建模为 `pacs.008.001.02`
 （payout 结算的 FI-to-FI 客户贷记转账）。
- `MsgId` 确定性：`<uetr>/<seq>`，seq = 已记录的 gpi 转移数——同状态
  重复导出 MsgId 不变，每次推进产生新 MsgId。
- 只读：读锁快照、无 version bump、无记账、无审计链追加——与
  `ExportCAMT053` / `GenerateStatement` 同契约；跟踪旅程的持久记录
  仍是哈希链 `gpi_status` 审计事件。
- 刻意**不**驱动 payout 指令：状态报告是事实陈述，不是操作员
  attestation（与 pain.002 回盘同理）。
- `400` 缺 `uetr`；`404` 未知 UETR。计数器
  `ledger_pacs002_exports_total`。

```bash
curl -s 'localhost:8080/exports/pacs002?uetr=9f2c8e1a-…' -o status.xml
# 200 pacs.002.001.03: <FIToFIPmtStsRpt>/.../<OrgnlUETR>9f2c8e1a-…</OrgnlUETR>/<TxSts>ACSP</TxSts>
```

Go: `ledger.ExportPacs002`。

### camt.056 支付撤销 + camt.029 决议（`POST /cancellations`）

已发出但未结算的 payout 可用 ISO 20022 `camt.056.001.02`
（`FIToFICancellationRequest`）向银行发起撤销：`POST /cancellations`
`{"uetr","reason_code"}` 按 UETR 定位原指令并附撤销原因码（CxRsn），
打开一条撤销记录（状态 `requested`）；`GET /exports/camt056?uetr=`
渲染出真正发给银行的 camt.056 报文。对手方以 `camt.029.001.03`
（`FIToFICancellationStatusReport`）决议：`POST /exports/camt029/apply`
送入对手方回盘。

撤销状态机：

| 状态 | 含义 |
|---|---|
| `requested` | 已请求，待对手方决议 |
| `confirmed` | 对手方接受（camt.029 `ACCR`）：payout 标记 `cancelled` |
| `rejected` | 对手方拒绝（camt.029 `RJCR`）：payout 保持 `submitted`，原因码进审计 |

语义：
- fail-closed：未知 UETR、已结算（gpi `ACCP`）/终态（`RJCT`）的跟踪、
  已撤销的 payout、未发出（draft/approved，用 `CancelPayout`）或已失败
  的 payout、无原因码的请求一律拒绝；已结算资金的撤销请走冲正
  （reversal）——camt.056 只召回在途指令。
- 幂等：`requested` 状态下重复请求返回原记录（200）；确认后重请求
  409；拒绝后可发起新一轮（新 seq、新 `MsgId`）。
- `confirmed` 时 payout `submitted`→`cancelled`，记 `payout_cancelled`
  审计事件 + outbox 事件（`CancelPayout` 惯例）——钱从未离账，不记
  冲销分录；gpi 跟踪**不**动（撤销不是银行的结算 verdict）。
- camt.029 解析 fail-loud：XML 非法、无 `RslvdCxl`、空 `OrgnlUETR`、
  `CxlSts` 非 `ACCR`/`RJCR` → 400；决议重放幂等（同决议 no-op），
  冲突决议 422；无 open 请求的决议 404（有请求才有决议）。
- 记录为内存态（payout/gpi 惯例）：持久记录是哈希链
  `cancellation_requested` / `cancellation_resolved` 审计事件。
- 计数器 `ledger_camt056_exports_total` /
  `ledger_camt029_applies_total`。

```bash
curl -s -X POST localhost:8080/cancellations \
  -H 'Content-Type: application/json' -H 'x-ledger-actor: night-ops' \
  -d '{"uetr":"9f2c8e1a-…","reason_code":"DUPL"}'
# 201 {"uetr":"9f2c8e1a-…","status":"requested","msg_id":"9f2c8e1a-…/cxl/1",...}
curl -s 'localhost:8080/exports/camt056?uetr=9f2c8e1a-…' -o cancel.xml
# 200 camt.056.001.02: <FIToFICxlReq>/.../<CxlId>9f2c8e1a-…/cxl/1</CxlId>/<OrgnlUETR>9f2c8e1a-…</OrgnlUETR>
curl -s -X POST localhost:8080/exports/camt029/apply \
  -H 'Content-Type: application/xml' --data-binary @bank-camt029.xml
# 200 {"uetr":"9f2c8e1a-…","status":"confirmed",...}  # payout -> cancelled
```

Go: `ledger.RequestPayoutCancellation` /
`ledger.ExportCancellationRequest` / `ledger.ParseCAMT029` /
`ledger.ApplyCAMT029Report` / `ledger.ResolvePayoutCancellation`。

### `GET /exports/pacs004` — ISO 20022 pacs.004 跨境支付退汇导出

已结算的 payout 可以用 ISO 20022 `pacs.004.001.02`
（`PaymentReturn`）渲染银行间退汇报文：资金已离账，对手方（或
银行）把钱退回。`GET /exports/pacs004?uetr=&reason=` 按 UETR 定位
原支付，`OrgnlUETR` 关联原支付，`RtrdIntrBkSttlmAmt` 携带退回金额
（币种 + 金额），`RtrRsnInf/Rsn/Prtry` 携带退汇原因码（RtrRsn）。

退汇原因码（银行间真实词汇，闭集校验）：

| 代码 | 含义 |
|---|---|
| `DUPL` | 重复支付：已贷记的支付又来一笔重复 |
| `AM09` | 金额错误：结算金额与指令金额不符 |
| `AC03` | 收款账户无效：受益人账户关闭/误填，资金退回发起方 |
| `CUST` | 受益人要求退回：收款方拒收（如不愿保留超额汇款） |

语义：
- 与 camt.056（LG-168）正交：camt.056 是**结算前**召回在途指令
  （钱从未离账），pacs.004 是**结算后**退汇（钱已离账又回来）；
  两条报文互补不重叠——未结算（draft/approved/submitted/
  cancelled/failed）的 payout 一律拒绝，未知 UETR、空原因码、
  不在闭集内的原因码同样拒绝（fail-closed，不做新 verdict）。
- `OrgnlGrpInf` 把原指令建模为 `pacs.008.001.02`（payout 结算的
  FI-to-FI 客户贷记转账，pacs.002 惯例）；`MsgId` 确定性：
  `<uetr>/rtr`——导出不开记录，同支付同原因重复导出 MsgId 不变。
- 只读：读锁快照、无 version bump、无记账、无审计链追加——与
  `ExportPacs002` / `ExportCAMT053` 同契约；刻意**不**驱动 payout
  指令（退汇报告是事实陈述，不是操作员 attestation）。
- `400` 缺 `uetr`/`reason` 或原因码不在闭集；`404` 未知 UETR；
  `422` 支付未结算。计数器 `ledger_pacs004_exports_total`。

```bash
curl -s 'localhost:8080/exports/pacs004?uetr=9f2c8e1a-…&reason=DUPL' -o return.xml
# 200 pacs.004.001.02: <PmtRtr>/.../<OrgnlUETR>9f2c8e1a-…</OrgnlUETR>/.../<Prtry>DUPL</Prtry>
```

Go: `ledger.ExportPacs004`。

### `GET /exports/camt052` — ISO 20022 camt.052 日间账户报告导出

camt.053（LG-158）是日终对账单，camt.052（LG-170）是它的日间
兄弟：同一个账户在 intraday 切片 `[since, until)` 内的分录渲染为
`camt.052.001.02`（`BankToCustomerAccountReport`）——`Ntry` 行是
窗口内的 journal 分录（pending-review 行除外：它们从未触碰余额，
camt.053 同规则），外加每币种两条余额快照：`OPBD`（窗口开启前
一刻的 booked balance）和 `CLBD`（窗口关闭前一刻）。

语义：
- 与 camt.053 导入/导出（LG-103/LG-158）正交：camt.053 是日终
  statement 往返（导入解析 ↔ 导出渲染），camt.052 只回答"现在
  账上多少钱"，不做 round-trip 解析。
- 恒等式：`CLBD == OPBD + 窗口内分录净额`（每币种），测试钉死；
  自转账净零、负余额渲染为 `DBIT` + 绝对金额。
- `account`/`rpt_id`/`since`/`until` 必填（RFC 3339），`currency`
  可选过滤单币种，`msg_id` 缺省回退 `rpt_id`；账户级外部映射
  （camt053 IBAN 映射）同样适用。未知账户导出合法空报告
  （银行也会发空的日间报告）。
- 只读：读锁快照、无 version bump、无记账、无审计链追加——与
  `ExportCAMT053` 同契约。计数器 `ledger_camt052_exports_total`。

```bash
curl -s 'localhost:8080/exports/camt052?account=ops&rpt_id=RPT-1&since=2026-10-22T09:00:00Z&until=2026-10-23T09:00:00Z' -o intraday.xml
# 200 camt.052.001.02: <BkToCstmrAcctRpt>/<Rpt>/.../<Cd>OPBD</Cd>/<Amt Ccy="USD">150.00</Amt>
```

Go: `ledger.ExportCAMT052` / `ledger.RenderCAMT052Report`。

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

### `POST /camt053/import` — ISO 20022 camt.053 statement import

The camt.053 counterpart of the OFX import above: it parses an ISO
20022 `BankToCustomerStatement` document (any `camt.053.001.xx`
namespace version) and returns the same kind of **pre-posting
report** — every `Ntry` entry as a `DraftEntry` the operator reviews,
matched against the account's journaled rows with the same rule (one
matcher serves both formats). Go: `ledger.ParseCAMT053` /
`ledger.ImportCAMT053`.

```bash
curl -s -X POST localhost:8080/camt053/import \
  -H 'Content-Type: application/json' \
  -d '{"account":"DE89370400440532013000","camt053":"<Document>...</Document>"}'
# 200 {"document_sha256":"…","account":"DE89370400440532013000",
#      "items":[{"transaction":{...,"direction":"in","amount_cents":25000,...},
#                "draft":{"id":"camt053/STMT-2026-10/1","debit_account":"DE89370400440532013000",...},
#                "status":"matched","matched_entry_id":"e1"}, ...],
#      "matched":1,"mismatched":0,"unmatched":2,"duplicate":false}
```

Semantics:

- **Nothing is posted.** Same draft-only contract as the OFX import —
  no journal rows, no hash chain, no version bump.
- **Direction** comes from `CdtDbtInd`: `CRDT` means the account was the
  debit leg — money in (`"direction":"in"`); `DBIT` means it was the
  credit leg — money out. Amounts parse as exact decimals to cents, no
  floats; the booking date reads `BookgDt/Dt` (else `BookgDt/DtTm`,
  else the value date), normalized to UTC. Multi-currency statements
  match per entry currency — the matcher isolates currencies, it never
  converts.
- **Matching** — same rule as OFX: effective rows only, same currency,
  equal `|amount|`, `CreatedAt` within ±24h of the booking date.
- **Idempotency** — the ledger keeps every import report keyed by the
  document's SHA-256; a re-upload returns the original report with
  `duplicate:true`. The registry survives disaster-recovery snapshots.
- A statement naming a different account than the import target, an
  entry missing its amount/currency/booking date, a malformed
  document, a missing account, or an empty document is a `400`. There
  is no special auth, like the export endpoint.
  `GET /metrics` counts imports as `ledger_camt053_imports_total`.

### `POST /camt054/import` — ISO 20022 camt.054 debit/credit notification import

The camt.053 import matches statements against journal rows; camt.054
matches *notifications* against outstanding **transfers and payouts**
— orthogonal by design. A `BkToCstmrDbtCdtNtfctn` document carries
individual `Ntry` debit/credit notifications (what the bank just
did), and this endpoint answers the operator's question: *which of my
outstanding items did this notification confirm?*

`POST /camt054/import` (JSON body
`{"account": "<the notification account>", "camt054": "<Document>...</Document>"}`)
parses with `ledger.ParseCAMT054` — namespace-tolerant across
`camt.054.001.xx` revisions, one notification per `Ntry`; a malformed
document, a missing account, or a document naming a different account
is a `400`. Each notification is then triple-matched with
`ledger.ImportCAMT054`:

- **Candidates** are named by the notification's `EndToEndId` via
  the shared idempotency-key convention (payout by ID, payout by
  idempotency key, transfer by idempotency key, transfer by ID — the
  same `EndToEndId` the pain.001 export stamps);
- a candidate **matches** when its amount equals the notification's
  (exact cents, no conversion), its currency equals, and its
  creation is within the match window (default ±24h, configurable
  per import) of the notification's booking date;
- several outstanding items under one `EndToEndId` is
  `mismatched` with reason `amount_mismatch`, `currency_mismatch`,
  or `date_out_of_window`; no candidate is `unmatched`.

A matched notification advances its outstanding item's
reconciliation status from outstanding to reconciled (recorded in
the match registry and the hash-chained `camt054_reconciled` audit
event). Deliberately nothing else: no money moves and no payout
lifecycle transition happens — the operator settles via
`POST /payouts/{id}/settle`. The ledger keeps every import report
keyed by the document's SHA-256; a re-upload returns the original
report with `duplicate:true`.

```bash
curl -s -X POST localhost:8080/camt054/import \
  -H 'Content-Type: application/json' \
  -d '{"account":"acct-notify","camt054":"<Document>...</Document>"}'
# 200 {"document_sha256":"…","account":"acct-notify",
#      "items":[{"notification":{...,"direction":"in","amount_cents":50000,...},
#                "status":"matched","matched_kind":"transfer",
#                "matched_id":"xfer-1"}, ...],
#      "matched":1,"mismatched":0,"unmatched":0,"duplicate":false}
```

`GET /metrics` counts imports as `ledger_camt054_imports_total`.
Go: `ledger.ParseCAMT054` / `ledger.ImportCAMT054`.

### camt.053 statement export (Go: `ledger.ExportCAMT053`)

The inverse of the import above: it renders the account's effective
journal rows as an ISO 20022 `camt.053.001.02`
`BankToCustomerStatement` document — one `Stmt` block, one `Ntry` per
entry — for the bank, the auditor, or any camt.053 reader. Go-only
(like the NACHA builder below): `ledger.ExportCAMT053` /
`ledger.RenderCAMT053Statement`.

```go
doc, err := l.ExportCAMT053(ledger.CAMT053ExportOptions{
    Account: "DE89370400440532013000", StmtID: "STMT-2026-10",
    Since: start, Until: end, // half-open [Since, Until); Currency optional
})
// doc is a namespaced camt.053.001.02 XML document.
```

Semantics:

- **Round-trip** — the document carries exactly what
  `ledger.ParseCAMT053` reads back: `Stmt/Id`, `Acct/Id/Othr/Id`,
  `Amt`+`Ccy`, `CdtDbtInd`, the booking date as `BookgDt/DtTm` (UTC,
  full RFC 3339 so the instant survives), `Refs/EndToEndId`, and the
  remittance text. `ParseCAMT053(ExportCAMT053(...))` returns the same
  transactions.
- **Direction** — account-centric like the importer: the account as
  the debit leg reads `CRDT` (money in), as the credit leg `DBIT`
  (money out); amounts are absolute, the sign lives in the indicator.
- **Entry refs carry the idempotency key** — `Refs/EndToEndId` is the
  entry's idempotency key (the entry ID when the row has none), so the
  bank's references trace back to ledger rows.
- **Read-only** — same contract as `GenerateStatement`/`ExportOFX`:
  read lock held throughout, no journal rows, no hash chain, no
  version bump. Pending-review rows are excluded (they never touched
  balances); re-export after approval.
- **Account naming** — `Stmt/Acct` names the external identifier
  mapped under the `camt053` system when one exists (usually the IBAN
  the bank expects; see `SetExternalMapping`), else the internal
  account ID.
- An account with no rows in the window exports a valid empty
  statement. Asymmetry, documented: `ParseCAMT053` rejects documents
  with zero entries, so an empty export does not re-parse — the
  exporter is honest about the window, the parser is strict about
  importing only real statements.

### NACHA ACH file generation (Go: `ledger.GenerateNACHAFile`)

From payout instructions (or any caller-built entry list) the ledger
renders a NACHA origination file — File Header (1), per-batch Batch
Header (5) / Entry Detail (6) / optional Addenda (7) / Batch Control
(8), File Control (9). Every record is exactly 94 characters; the file
is padded with `9` records to a 10-record block. Go-only (no HTTP
endpoint): the operator builds the file from reviewed payouts and
hands it to the bank's uploader.

```go
entry, _ := ledger.PayoutToNACHAEntry(payout, ledger.NACHAReceiver{
    Routing: "021000089", AccountNumber: "987654321",
    IndividualName: "ACME SUPPLIER", // payouts are outflows -> credit (22/32)
})
doc, err := ledger.GenerateNACHAFile(ledger.NACHAFileConfig{
    ImmediateDestination: "021000089", ImmediateOrigin: "121042882",
    DestinationName: "FEDERAL RESERVE", OriginName: "FIRST NATIONAL",
}, []ledger.NACHABatch{{Config: ledger.NACHABatchConfig{
    ServiceClassCode: "220", CompanyName: "ACME CORP", CompanyID: "1122334455",
    EntryClassCode: "CCD", CompanyEntryDescription: "VENDOR PAY",
    EffectiveEntryDate: time.Now(), OriginatingDFI: "12104288", BatchNumber: 1,
}, Entries: []ledger.NACHAEntry{entry}}})
```

Semantics:

- **Routing check digits** — every receiving DFI and both immediate
  destination/origin routings are 9-digit validated with the ABA 3-7-1
  checksum (`ledger.NACHACheckDigit`); a bad digit fails the whole
  file, never ships.
- **Direction checks** — transaction codes `22/32` (credit) and
  `27/37` (debit); a debit entry in a `220` credits-only batch (or a
  credit in `225`) is rejected, and batch/file control debit/credit
  totals are split by that same direction, so totals always reconcile
  with the entry list. Zero-dollar prenotes (`23/28/33/38`) are
  allowed only with a zero amount.
- **Fail-loud fields** — a text field that exceeds its fixed width or
  carries non-printable-ASCII is an error; the builder never silently
  truncates names or account numbers. Uppercase is recommended by
  NACHA but not forced.
- **Trace numbers** — ODFI transit (8) + per-batch 7-digit sequence
  from 1; entry hash sums the receiving DFIs (last 10 digits kept).
- Scope: CCD/PPD origination only — same-day windows and IAT are
  out of scope, documented not faked. Return (dishonored-entry)
  handling is covered by the ACH returns section above
  (`ledger.ParseNACHAReturn`, `POST /ach/returns`). Lines are
  joined with `\n` (no trailing newline); convert to CRLF for the
  bank if required.

### SWIFT MT940 statement export (Go: `ledger.ExportMT940`)

The tag block a bank's statement downloader would produce — `:20:`
(transaction reference), `:25:` (account identification), `:28C:`
(statement/sequence number), `:60F:` (opening balance), one `:61:`
(+ optional `:86:` supplementary) per statement line, `:62F:`
(closing balance), `:64:` (available balance) — from the account's
effective journal rows. Go-only (like NACHA and camt.053 export): the
operator builds the file and hands it to the bank's statement
processor.

```go
doc, err := l.ExportMT940(ledger.MT940ExportOptions{
    Account: "DE89370400440532013000", StatementID: "2026-10",
    Since: start, Until: end, // half-open [Since, Until); Currency required
})
// doc is the MT940 customer statement message, lines joined with "\n".
```

Semantics:

- **Reconciliation is structural** — the closing balance is computed
  as opening + signed sum of the lines, and `ledger.RenderMT940Statement`
  re-derives it and fails loud if the two disagree: a statement that
  does not foot is never rendered.
- **Direction** — account-centric like camt.053/OFX: the account as
  the debit leg reads `C` (money in), as the credit leg `D` (money
  out); amounts are absolute (`1500,00` with a comma decimal
  separator), the sign lives in the `C`/`D` mark.
- **Fail-loud lines** — a statement line missing its value date,
  amount, or reference is an error, never a silently degraded tag.
  The `:61:` reference is the entry's idempotency key (the journal
  entry ID when the row carries none), truncated to the `16x` field
  width — stable across exports, so the bank dedupes on it.
- **Read-only** — same contract as `GenerateStatement`/`ExportOFX`/
  `ExportCAMT053`: the read lock is held throughout, no version bump,
  nothing booked, no audit-chain link. Pending-review rows are
  excluded (they never touched balances).
- **Account naming** — `:25:` names the external identifier mapped
  under the `mt940` system when one exists (usually the IBAN; see
  `SetExternalMapping`), else the internal account ID.
- `:64:` repeats `:62F:` exactly: the ledger has no intraday
  credit-line concept, so the available balance is the honest
  closing balance.

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
per-currency levels live in the reconcile report). Like `/balance` and
`/snapshot`, `{id}` accepts an alias, and the response echoes the
account's alias as `alias` (omitted when the account has none).

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

### Virtual accounts (`POST /virtual-accounts`, `GET /virtual-accounts`, `DELETE /virtual-accounts/{van}`)

**Virtual account numbers (VANs)** for collection: map a customer-facing
account number to a real parent account, and every receipt naming the VAN
is booked to the parent instead — the VAN itself never holds a balance
(zero-balance pass-through). The classic shape is one VAN per customer
or invoice, all attributing to the merchant's operating account.

```bash
curl -s -X POST localhost:8080/virtual-accounts \
  -H 'Content-Type: application/json' -d '{"van":"van-cust-1","parent":"merchant"}'
# 200 {"van":"van-cust-1","parent":"merchant","version":1}
```

- Receipts crediting the VAN (the receiving leg) book to the parent:
  `POST /entries`, `POST /transfers` (principal and fee legs),
  split-transfer chunk principals, `POST /entries/batch`, and
  cross-currency transfers all attribute. The journal row carries a
  `van-attribution:<van>` memo marker (following the `rule:<id>` /
  `dividend:<bps>` marker conventions; fee-split legs keep their
  machine-readable memo and disclose via audit instead), and every
  attribution emits a `virtual_account_attribution` audit event with
  `van` / `parent` / `amount_cents` / `currency`.
- A posting that would **debit** the VAN (pay from it) is rejected with
  `422` (`ErrVANNoDebit`) — there is no balance to pay from.
- The idempotency replay check runs before VAN validation: replaying a
  key posted before the VAN was registered returns the original entry,
  not an attribution or a rejection. Every account risk gate (frozen,
  overdraft, daily-limit, period) sees the parent, so freezing the
  parent stops VAN receipts.
- Registration is structural, not bookkeeping: like freeze/unfreeze, it
  does not bump the ledger version. Every mapping change bumps the
  mapping's own version counter (`version` in the responses,
  `virtual_account_version` in `POST /reconcile`); re-registering an
  identical mapping and unregistering an unknown VAN are idempotent
  no-ops. Validation is fail-closed: empty VAN/parent, self-mapping, and
  chained mappings (a parent that is itself a VAN, or a VAN that already
  parents another VAN — attribution is exactly one hop) are rejected.
- The mapping table round-trips through snapshots, and `POST /reconcile`
  discloses the mappings plus the version. `GET /metrics` counts
  attributions as `ledger_van_attributions_total`.

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

### Account aliases (`PUT /accounts/{id}/alias`, `GET /accounts/{id}/alias`)

Aliases are human-readable, ledger-unique labels for accounts —
`ops_cash`, `stripe.payouts`, `acme-corp` — so operators, dashboards,
and payment ops tooling can address accounts without pasting opaque
account IDs. One alias names exactly one account, and one account
carries at most one alias. Aliasing is structural, like tags: it does
not bump the ledger version, bindings round-trip through the
disaster-recovery snapshot, and every change is recorded in the
structured audit log (`alias_set` / `alias_changed` / `alias_removed`,
with `account`, `old_alias`, and `new_alias` — aliases are
operator-chosen labels and carry no sensitive information).

```bash
curl -s -X PUT localhost:8080/accounts/cash/alias \
  -H 'Content-Type: application/json' -d '{"alias":"ops_cash"}'
# 200 {"account":"cash","alias":"ops_cash"}

curl -s localhost:8080/accounts/ops_cash/balance
# {"account":"cash","balance_cents":1000,"available_cents":1000,"frozen":false,"alias":"ops_cash"}
```

- Aliases are 1–64 characters of `[A-Za-z0-9._-]` (URL-safe by
  construction); anything else → `400`. An empty or missing alias in
  the `PUT` body clears the account's alias.
- Binding an alias that already names a *different* account → `409`
  (`ledger: alias already bound to another account`); the failed bind
  changes nothing. Rebinding an account's own alias unbinds the old one
  atomically. Setting the account's current alias again is a silent
  no-op (no audit event), so idempotent retries stay quiet in the
  compliance trail.
- Binding an alias identical to the account ID is allowed but a no-op
  in effect — it resolves exactly like the plain ID.
- `GET /accounts/{id}/alias` returns the account's current alias; an
  account with no alias → `404`.
- Resolution rule on the account-scope **read** endpoints (`balance`,
  `snapshot`, `trial-balance`, `balance-at`, `entries`, `statement`,
  `merkle-proof`, `rollup`, `ofx`, `balance-history?account=`, and the
  alias endpoints themselves): a registered alias wins over a literal
  account ID; anything else is used as the account ID verbatim, so
  unknown accounts keep their historical zero-balance behavior. The
  `balance`, `snapshot`, and `trial-balance` responses echo the
  account's alias as `alias` (omitted when the account has none), and
  `account` always carries the canonical account ID.
- Write endpoints (`parent`, `tags`, `close`, `freeze`, …) keep taking
  canonical account IDs: writes are audited against the canonical ID,
  and resolving aliases on the write path would let a rebound alias
  silently retarget in-flight automation.
- `GET /metrics` counts successful alias writes as
  `ledger_alias_sets_total` (bindings, rebinds, and clears).

### External account mappings (`POST` / `GET` / `DELETE /accounts/{id}/external-mappings`)

External account mappings are the explicit links between one internal
ledger account and the identifiers external systems know it by — a
core-banking account number, a PSP merchant number. One internal account
maps to **many** external identifiers: the mapping is keyed by the
(`account`, `system`, `external_id`) triple, so the same account can be
known to `bank` under a settlement account number and to `psp` under a
merchant number, and one system can hold several identifiers for it.
System names are 1–64 characters of `[A-Za-z0-9._-]` (the alias/tag
charset, URL-safe by construction). Well-known systems: `bank`
(core-banking / settlement account number), `psp` (PSP merchant number),
`ofx` (the identifier OFX statement exports name in `<ACCTID>`).

Mapping is structural, like aliases: it does not bump the ledger
version, rows round-trip through the disaster-recovery snapshot
(including the incremental config line), and every change is recorded in
the structured audit log (`external_mapping_set` /
`external_mapping_removed`, with `system`, `external_id`, and `note` —
mappings are operator-chosen routing data and carry no sensitive
information).

```bash
curl -s -X POST localhost:8080/accounts/cash/external-mappings \
  -H 'Content-Type: application/json' \
  -d '{"system":"bank","external_id":"DE89370400440532013000","note":"primary settlement"}'
# 200 {"account":"cash","system":"bank","external_id":"DE89370400440532013000","note":"primary settlement"}

curl -s localhost:8080/accounts/cash/external-mappings
# 200 {"account":"cash","mappings":[{"account":"cash","system":"bank","external_id":"...","note":"..."}]}

curl -s -X DELETE 'localhost:8080/accounts/cash/external-mappings?system=bank&external_id=DE89370400440532013000'
# 200 {"account":"cash","system":"bank","external_id":"...","removed":true}
```

- Declaring a row that already exists with the identical note is a silent
  no-op (no audit event), so idempotent retries stay quiet in the
  compliance trail; updating the note re-audits. Removing an undeclared
  row → `404`.
- An empty/invalid `system`, an empty or overlong `external_id`, or an
  overlong `note` → `400`. Unknown JSON fields → `400` (strict body, like
  every write endpoint). Like aliases, `{id}` is the canonical account ID
  on writes: no alias resolution, so a rebound alias cannot retarget an
  audited mapping.
- The `Reconcile` report discloses every mapping as `external_mappings`
  (sorted by account, system, external ID), and `GET /metrics` counts
  mapping changes as `ledger_external_mapping_changes_total` (sets plus
  removals; idempotent replays not counted).
- **File-export resolution**: an export that must name exactly one external
  identifier for an account in a system resolves the deterministic first
  row — the smallest `external_id` in that system — and falls back to the
  internal account ID when the account has no mapping in the system. OFX
  statements (`GET /accounts/{id}/ofx`) use the `ofx` system's mapping for
  `<ACCTID>`; the identifier is XML-escaped like the internal ID was.
- **pain.001 stays fail-closed on IBANs**: `GET /exports/pain001` keeps
  requiring registered, ISO-validated IBANs (`ledger.SetAccountIBAN`) for
  debtor and creditor and refuses the export without them. A generic
  external mapping is not an IBAN — substituting a PSP merchant number
  for a debtor account identifier would emit an invalid payment
  instruction — so the mapping registry deliberately does not feed the
  pain.001 export.

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
- `meta.<key>`: optional structured-metadata filters — one `meta.<key>=<value>`
  query parameter per pair; only entries carrying every given pair are
  returned (exact match per pair). It composes with the time window, the
  `memo` and `ref` filters, and cursor pagination, so reconciliation
  tooling can page through every entry of a payment channel.
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
- Every *new* anchoring also appends a `chain_anchored` op to the
  hash-chained audit log (`chain_seq`, `head_hash`, `tsa_url`, `gen_time`,
  `serial`, `tsa_token`), so deleting `tsa_anchors.jsonl` cannot erase all
  evidence that an anchor happened — the LG-32 chain still attests to it.
  Idempotent re-anchors of an unchanged head emit no new op.
  (manual requests and worker ticks alike).

### Audit chain archiving (`ledger.ExportChainSegment`)

For long-lived ledgers the audit chain grows one link per Post forever.
`Ledger.ExportChainSegment(fromSeq, toSeq, dir)` (Go API) writes a cold
segment to `dir` as a gzip JSONL archive (`chain-segment-<from>-<to>.jsonl.gz`,
one link per line: `seq`, `entry_id`, `prev_hash`, `hash`) plus a JSON
manifest (`chain-segment-<from>-<to>.manifest.json`). The snapshot is
taken under the read lock and the files are written after it is
released, so a concurrent Post is blocked only for the slice copy, not
for the compression. Both files are fsync'd: a segment archive is a
compliance artifact.

Manifest fields: `format` (`"chain-segment/1"`), `from_seq` / `to_seq`
(1-based chain seqs = ledger versions at Post time), `link_count`,
`segment_head_prev_hash` (the first link's `prev_hash` — the all-zero
genesis marker when `from_seq` is 1), `segment_tail_hash`, the
`export_chain_head_*` snapshot of the live head at export time (seq,
hash, hot-link count), and `exported_at`.

- `Ledger.VerifyChainWithArchives(archivePaths...)` re-verifies the chain
  when its cold prefix lives in archives: each archive's internal
  continuity (contiguous seqs, `prev_hash` links, every link's hash
  recomputed from the in-memory journal), archive-to-archive stitching
  (`segment_head_prev_hash` of each segment equals the previous
  segment's `segment_tail_hash`, the first segment anchored at the
  genesis), and the hot chain stitched onto the last archive's tail.
  With no archives it degrades to the ordinary `VerifyChain` genesis
  check.
- `Ledger.PruneChainPrefix(toSeq)` drops the in-memory links with
  `seq <= toSeq` — but only after re-verifying the archives that cover
  the prefix: a missing or corrupt archive refuses the prune instead of
  destroying the only verifiable copy. Journal entries are kept, so
  balances, statements, and reconciliation are unaffected. After
  pruning, `VerifyChain` fails honestly (the hot chain no longer starts
  at the genesis) and tells you to use `VerifyChainWithArchives`.
- `segment_tail_hash` is the value an external LG-44 anchor checkpoint
  can witness to attest a segment: anchor the segment tail exactly like
  anchoring a chain head. Note that pruning changes the hot chain's
  length, so pre-prune anchors (which record `len(chain)` as their seq)
  report "anchored data lost — chain was truncated" afterwards — the
  honest signal; re-anchor after pruning.

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
| `splits`               | every committed account split, sorted by split ID: `split_id`, `from_account`, `to_account`, the per-currency `legs`, and `created_at`. Unlike merges, split sources keep operating — nothing here is frozen by the split |
| `entry_links`          | every declared entry link (LG-123), in declaration order: `from_entry_id`, `to_entry_id`, `link_type` (`reversal_of` / `adjustment_of` / `fee_of` / `refund_of` / `mandate_of`), `declared_at`, and `declared_by` — the explicit directed associations between journal entries (for `mandate_of` the `to` endpoint is the mandate's reference record, not a journal entry) |
| `link_chains`          | the `reversal_of`/`adjustment_of` links expanded into per-root adjustment chains, sorted by `root_entry_id`: each chain's `root_entry_id` is the ultimate original entry and `links` lists every correction link reachable from it in declaration order, so the full correction history of an entry audits end to end (`fee_of`/`refund_of`/`mandate_of` links stay out of the chains — they are associations, not corrections — and are disclosed flat in `entry_links`) |
| `mandates`             | every direct-debit mandate (LG-125), sorted by ID: `payer`, `payee`, `max_amount_cents`, `currency`, `frequency`, validity window, effective `status`, plus `total_debits` and `window_debits` usage — the standing pull authorizations and how far each has been drawn |
| `fx_applied`           | `true` when the report carries the opt-in base-currency summary (see below); `false` on a plain scan |
| `base_currency_summary` | the base-currency rollup, present only when `fx_applied` is `true`: `base_currency`, base-currency `total_debits_cents`/`total_credits_cents`, per-currency `conversions`, converted `discrepancies`, the `fx_snapshot` of rates used, `missing_rates`, and `fx_incomplete` |
| `large_tx_reports` | the large-transaction (CTR) reports generated for the report's UTC day (LG-94), sorted by (account, currency, direction): `account`, `day`, `currency`, `direction` (`inflow`/`outflow`), `amount_cents`, `threshold_cents`, `report_currency`, `converted_cents` (+ `conversion_ok`, `rate_version`). Empty when nothing was generated — `POST /reconcile` discloses, it never generates |

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

### Large-transaction reporting (CTR)

Currency Transaction Report generation for compliance (LG-94). The ledger
aggregates every account's UTC-day inflow (debit legs) and outflow (credit
legs) per currency; when one side reaches the configured per-currency
threshold, `POST /compliance/large-tx-reports` generates a report entry
into the structured audit chain (`op: "large_tx_report"`, hash-sealed like
every other audit event).

```bash
curl -s -X POST localhost:8080/compliance/large-tx-reports \
  -d '{"day":"2026-10-15"}'
```

- Thresholds are per currency in that currency's own cents — the CTR
  convention (a USD $10,000 line and a EUR €9,000 line are independent).
  Configure with `LEDGER_LARGE_TX_THRESHOLDS="USD:1000000,EUR:900000"`.
- Every report discloses the converted amount in the reporting currency
  (default USD, `LEDGER_LARGE_TX_REPORT_CURRENCY`), converted at the FX
  table's mid price effective at the end of the report day. A flow
  currency with no live rate is reported anyway with `conversion_ok:
  false` — never silently dropped.
- Generation is read-only (books nothing, never blocks postings) and
  idempotent: regenerating a day replays the stored reports (`duplicate:
  true`) instead of double-reporting. The first generation per
  (account, currency, day, direction) wins — run it as an end-of-day
  batch for complete daily totals. Rows pending dual-control review are
  excluded (no money moved).
- `ledger_large_tx_reports_total` counts newly generated reports (replays
  excluded); the reconcile report's `large_tx_reports` section discloses
  the report day's generated rows.

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

### `GET /cash-flow` — cash-flow statement (LG-144)

The treasury question at period end: how much cash moved in and out, in
which activity, and does the period's net cash movement reconcile with
the cash accounts' opening and closing balances?
`GET /cash-flow?from=2026-10-01&to=2026-10-31&currency=USD`.

- **Cash-class accounts**: opt-in via `MarkCashAccount(account,
  currency)` in the `ledger` package — an empty currency marks the
  account in every currency, a code scopes it (a multi-currency
  settlement account that is cash-class only in its settlement currency).
  Structural config like the chart-of-accounts registrations: marking or
  unmarking does not bump the journal version, changes are audited
  (`cash_account_marked` / `cash_account_unmarked`), and the marks
  survive disaster-recovery snapshots. A cash account that pays out
  typically stays unregistered from the chart — `chart.go`'s direction
  control rejects crediting an asset-class account — and the mark is
  independent of chart registration either way.
- **Aggregation**: every effective leg on a cash-class account in
  `[from, to]` (UTC calendar days, inclusive) is a cash movement — a
  debit leg is an inflow, a credit leg an outflow. Journal entries marked
  `PendingReview` are excluded (booked but not effective, the LG-43
  settlement convention). The activity comes from the counterparty — the
  other leg's account — and its chart-of-accounts type:
  `revenue`/`expense` → `operating`; `equity`/`liability` →
  `financing`; `asset` → `investing`; unregistered counterparty →
  `operating` (the opt-in chart's honest fallback). Documented
  approximations: a liability counterparty labels working-capital moves
  like trade payables as `financing` (keep payables unregistered to land
  them in `operating`), and under the strict chart direction control
  investing inflows via asset counterparties do not occur (crediting an
  asset is rejected) — the investing leg is typically capex-style
  outflows. A transfer between two cash accounts nets to zero at the
  pool level (gross operating inflow + outflow by the fallback rule).
- **Reconciliation**: the report independently accumulates each cash
  account's opening balance, its period net flow, and its closing
  balance. A cash account whose `closing != opening + net flow` lands in
  `discrepancies` — the same self-consistency role
  `TrialBalanceDiscrepancy` plays in `POST /reconcile`. Aggregation uses
  checked `int64` cents arithmetic: a pathological overflow fails the
  report instead of wrapping.
- **Semantics**: the report is read-only — no version bump, no audit
  event, no journal rows. A ledger with no cash-class marks returns
  `200` with an empty report (zero totals, no discrepancies), not `404`.
  Malformed `from`/`to` (`YYYY-MM-DD` required, `from <= to`), an empty
  or invalid `currency`, or a `format` other than `json`/`csv` are `400`.
  `?format=csv` downloads the per-`(account, activity)` breakdown
  (`from,to,currency,account,activity,inflow_cents,outflow_cents,net_cents,opening_cents,closing_cents`,
  sorted by account, activities in operating/investing/financing order).
  Every `GET /cash-flow` increments `ledger_cash_flow_runs_total` on
  `/metrics`.

```bash
curl -s 'localhost:8080/cash-flow?from=2026-10-01&to=2026-10-31&currency=USD' | head -30
curl -s 'localhost:8080/cash-flow?from=2026-10-01&to=2026-10-31&currency=USD&format=csv' -o cash-flow.csv
```

### `GET /tax-report` — tax/VAT report export

The finance team's filing query: `GET
/tax-report?from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z&rule=vat-output:output&rule=vat-input:input`
aggregates derived tax legs (automatic posting rules whose derived
entries carry memo `"rule:<name>"`) per tax rule and currency over the
period — input/output tax totals, per-rule line items, and each rule's
rate snapshot (the bps amount expression, frozen at generation time so
an auditor can reproduce the filing). `exempt_prefix` excludes
tax-exempt counterparties; `currency` filters to one ISO code; `format`
selects `json` (default) or `csv` (deterministic row order).
`sign=true` attaches the LG-44 anchor key's Ed25519 signature over the
report (distinct domain — a report signature never verifies as an
anchor checkpoint); without a configured anchor key a signed request
is `400`, never silently downgraded. The report is read-only: it books
nothing, bumps no version, and appends no audit link. Go API:
`Ledger.TaxReport` with `TaxReportOptions`; `VerifyTaxReportSignature`
checks a report against a public key.

### Settlement cutoff windows

Per-currency settlement cutoff windows (LG-101): the payment-rail reality
behind the settlement view — a currency settles on windows, not
continuously. Configure a wall-clock cutoff per currency (`HH:MM` plus an
IANA timezone, default UTC); a transfer submitted after its currency's
cutoff misses the day's settlement run and is automatically booked for the
next window. The booking is never blocked — the journal commits exactly as
before — only the settlement flag is decided after the commit.

- **Configuration** (Go): `ledger.WithSettlementCutoff("USD", "17:00",
  "America/New_York")` at construction, or
  `SetSettlementCutoff("USD", "17:00", "America/New_York")` at runtime.
  `HH:MM` is strict (`9:05` and `24:00` are rejected) and the timezone
  must load (`time.LoadLocation`) — construction panics on invalid input,
  the setter returns an error, and both paths fail fast like the fee
  schedule. `ClearSettlementCutoff("USD")` removes a currency's window;
  currencies with no cutoff behave exactly as before (zero behavior
  change). Cutoffs are structural config: audited, no journal version
  bump, and they survive disaster-recovery snapshots.
- **Deferral**: a transfer posted strictly after the cutoff is recorded
  with `settlement_status: "scheduled"` on its receipt (before the
  cutoff: `"settled"`), the window it waits for in `settlement_window`,
  and a `transfer_settlement_deferred` audit event (`reason:
  settlement_cutoff`, the cutoff, the window). The cutoff instant itself
  still belongs to the current window.
- **Business-day windows**: windows only land on business days — the LG-95
  holiday calendar (weekend + custom holiday table) is consulted on the
  cutoff date, so a Friday-evening transfer waits for Monday's window and
  a configured holiday pushes it past the holiday. The cutoff clock is
  per-currency wall time; the holiday table is UTC-based by design.
- **Operator flow**: `GET /settlement/next-window?currency=USD` answers
  "when does this currency settle next" (the window in RFC3339, the
  timezone, and `is_today` — whether it is today's still-open cutoff;
  `404` when the currency has no cutoff). `GET /settlement/deferred`
  lists deferred transfers (`?status=scheduled|pending|settled`);
  `POST /settlement/advance` is the window-open tick (moves every
  scheduled deferral whose window opened to `pending`; idempotent);
  `POST /settlement/deferred/{id}/settle` confirms the run processed the
  transfer (`pending` → `settled`; `409` on a bad transition). The
  lifecycle is strict: `scheduled → pending → settled`, mirroring the
  dual-control review transitions, and every step is a hash-chained audit
  event.
- **Observability**: `ledger_settlement_deferred_total` counts deferred
  transfers; the reconcile report discloses the cutoff config
  (`settlement_cutoffs`) and every deferral (`settlement_deferrals`).

```bash
# When does USD settle next?
curl -s 'localhost:8080/settlement/next-window?currency=USD'
# {"currency":"USD","next_window":"2026-10-19T17:00:00-04:00","timezone":"America/New_York","cutoff":"17:00","is_today":false}

# Window-open tick, then confirm one transfer settled
curl -s -X POST localhost:8080/settlement/advance
curl -s -X POST -H 'x-ledger-actor: ops-chen' localhost:8080/settlement/deferred/tx-cut-1/settle
```

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

### Dunning / collections escalation (LG-118)

The aging view reports; dunning acts. On top of the same FIFO
attribution, an opt-in per-`(account, currency)` policy escalates an
overdue receivable through collections levels `L1`/`L2`/`L3` as the
**oldest outstanding lot's age** crosses configurable day thresholds —
so a payment retiring the oldest lot drops the age, exactly like the
aging buckets.

- **Opt-in policy** (Go API: `SetDunningConfig` /
  `ClearDunningConfig`): `{account, currency, level1_days, level2_days,
  level3_days}` with `0 < L1 <= L2 <= L3`. No policy, no escalation —
  the scan iterates only configured pairs.
- **One-way transitions**: `EscalateDunning(now)` derives each pair's
  level from the books and escalates only when the derived level is
  strictly higher than the stored one. A level never moves down — not
  even when the balance hits zero. The operator closes the file with the
  explicit `ResetDunning(account, currency)` after the money comes back.
- **Audit trail**: every escalation emits a `dunning_escalated`
  structured audit event (`account`, `currency`, `level`, `level_label`,
  `previous_level`, `days_past_due`, `overdue_cents`); every reset emits
  `dunning_reset` (with the cleared level); policy changes emit
  `dunning_config_set` / `dunning_config_cleared`.
  `ledger_dunning_escalations_total` counts the escalations. The scan
  itself never bumps the version or writes journal rows.
- **Disclosure**: the reconcile report lists every escalated position
  (`dunning_statuses`), and policies + states survive
  disaster-recovery snapshots like the other structural config.

```go
l.SetDunningConfig(ledger.DunningConfig{Account: "cust-123", Currency: "USD",
    Level1Days: 30, Level2Days: 60, Level3Days: 90})
escalated := l.EscalateDunning(time.Now()) // []DunningEscalation, edge-triggered
l.ResetDunning("cust-123", "USD")          // after the money comes back
```

### Receivable invoices (`POST /invoices`, 开票/应收)

Invoices are off-journal receivables: the operator's claim that a payer
account owes a fixed amount in a currency by a due date. They never
appear in entries, the audit chain, or the version counter — but they
drive the collections workflow that dunning escalates on.

- **Lifecycle** (Go API: `IssueInvoice` / `ApplyPayment` / `VoidInvoice` /
  `SweepOverdueInvoices`, or the `POST /invoices*` endpoints): an invoice
  opens as `issued`; `ApplyPayment` retires open invoices **FIFO** — the
  oldest invoice is paid first, a partially covered invoice becomes
  `partial`, a fully covered one becomes `paid`, and any amount beyond
  the open balance is returned as a remainder (an overpayment), never
  silently kept; `SweepOverdueInvoices` moves `issued`/`partial` invoices
  past their due date to `overdue` (the operator's "到期复核"); `VoidInvoice`
  cancels an invoice that never received a payment — a paid invoice
  cannot be voided (`422`), the record must stay for the audit trail.
- **Dunning 衔接**: open invoices past due feed the collections
  escalation directly — an overdue invoice is a FIFO lot aged from its
  due date, so a `(account, currency)` with a dunning policy escalates
  on an overdue invoice even when the journal shows no outstanding
  receivable. Not-yet-due invoices never escalate.
- **Disclosure**: the reconcile report carries per-currency
  `receivables_aging` (current / 1-30 / 31-60 / 61-90 / 90+ days past
  due, outstanding cents of open invoices), and the invoice register
  round-trips through disaster-recovery snapshots.

```bash
curl -s -X POST localhost:8080/invoices \
  -d '{"id":"inv-2026-001","account":"cust-123","amount_cents":500000,"due_date":"2026-11-30T00:00:00Z"}'
curl -s -X POST localhost:8080/invoices/apply \
  -d '{"account":"cust-123","currency":"USD","amount_cents":200000}'
# {"applied":[{"invoice_id":"inv-2026-001","applied_cents":200000,"status_after":"partial"}],"remainder_cents":0}
curl -s -X POST localhost:8080/invoices/sweep-overdue   # {"overdue":[...]}
```

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

### Net settlement batches (`ledger/clearing.go`)

The workflow container around netting (LG-135): where multilateral
netting nets on-book positions at cut-off, a clearing batch collects
off-book transfer intents over time — payouts queued through the day,
marketplace seller settlements, payroll legs — and settles them later in
one atomic batch. `OpenClearingBatch(id, legs, key)` validates and stores
the intents without touching the journal, so the operator sees exactly
what will move before anything moves.

`SettleClearingBatch(id, now)` aggregates the legs per (account,
currency), then books the minimal greedy transfer set (largest debtor
against largest creditor per currency, deterministic by account ID) in
one atomic batch through the full Post gate order — any failing leg
rejects the whole batch before the first mutation, the batch stays
`open`, and zero partial rows land, so a retry after fixing the cause is
safe. Settlement legs follow the transfer convention on the netted
amounts: the net payee is debited, the net payer credited. Net entry
IDs are deterministic (`clearing/<batch-id>/<n>`) and carry
`BatchID: "clearing/<batch-id>"`, so a retried settle replays through
the shared entry idempotency namespace instead of double-booking.

Lifecycle: `open → settled | cancelled`. `CancelClearingBatch` discards
an open batch's intents (nothing was ever journaled); cancelling a
settled batch is rejected. Every transition is audited
(`clearing_batch_open`, `clearing_batch_settled`,
`clearing_batch_cancelled`). Re-settling a settled batch returns it
unchanged. `Reconcile` lists every batch (`id`, `status`, `legs`,
`currencies`, `settled_at`) on the report, so the operator can always
see which batches produced the rows on the books. Like netting
sessions, batches are in-memory only and do not round-trip through
disaster-recovery snapshots — the journaled net rows do.

```go
batch, err := l.OpenClearingBatch("payouts-2026-10-19", []ledger.ClearingLeg{
    {ID: "l1", From: "marketplace", To: "seller-a", AmountCents: 10000, Currency: "USD"},
    {ID: "l2", From: "marketplace", To: "seller-b", AmountCents: 5000, Currency: "USD"},
}, "")
// batch.Status == "open", nothing journaled yet
settled, err := l.SettleClearingBatch("payouts-2026-10-19", time.Now())
// settled.Status == "settled", settled.EntryIDs == ["clearing/payouts-2026-10-19/1", ...]
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

#### Outbox live tail (`GET /events/stream`)

The pull API above is for catch-up; downstream indexers and
notification fan-out want low-latency push. `GET /events/stream` is
the outbox as **Server-Sent Events**: it first replays every event
after the cursor from the outbox directory (the same store
`GET /events` reads), then switches to live push — every newly
committed event arrives in `seq` order, no polling.

```bash
curl -N localhost:8080/events/stream '?cursor=<opaque>&account=cash'
# : connected
#
# id: 41
# event: entry_posted
# data: {"seq":41,"type":"entry_posted",...}
#
# : ping          <- every 15s, keeps idle connections alive
```

Semantics:

- **Resume.** `?cursor=` takes the opaque page cursor; the standard
  `Last-Event-ID` header (the raw `seq` the stream sends as the SSE
  `id`) is accepted too, so `EventSource` clients reconnect with zero
  client code. The subscription is taken before the replay scan, and
  events committed mid-replay are deduped by `seq` — nothing lost,
  nothing duplicated.
- **Filters.** `account` and `type` filter the replay and the live
  push alike (same `account` semantics as `GET /events`: the event's
  account or either leg of an `entry_posted`).
- **Backpressure.** The live subscription is a bounded 512-event
  queue. A consumer that falls behind past that is dropped — the
  commit path never blocks on a slow consumer — the stream ends with
  `: slow-consumer-disconnected`, and the consumer reconnects with
  its last id. (This is the Go equivalent of a writableLength guard.)
  Slow-consumer drops are counted by the outbox; open streams are the
  `ledger_outbox_stream_connections` gauge on `GET /metrics`.
- **Auth.** Like every `GET` route it needs the `read:ledger` API key
  scope when key enforcement is on; with zero keys configured it is
  fail-open, exactly like `GET /events`. Without
  `LEDGER_OUTBOX_DIR` it 404s fail-closed.
- **At-least-once**, like the outbox itself: consumers dedupe on
  `(type, seq)`.

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

#### Outbound signed webhooks (`ledger/webhook.go`) — `GET /webhooks/deliveries`

An opt-in subscriber of the domain-event outbox that POSTs selected
domain facts — `transfer_posted` (settled), `review_rejected`,
`pain001_status_changed` (instruction and per-transfer bank-side moves),
or any other outbox event type — to a registry of external endpoints
as HMAC-SHA256-signed JSON webhooks. Where the audit sink above is a
single compliance pipe that tails the outbox files, the dispatcher is a
live notification fan-out: each durably emitted event is pushed to
every enabled endpoint whose event filter matches, on per-endpoint
background workers. Disabled by default; set `LEDGER_WEBHOOKS` (and
`LEDGER_OUTBOX_DIR`, which webhooks require) to enable — see
[Environment](#running).

```bash
LEDGER_OUTBOX_DIR=/var/lib/ledger/outbox \
LEDGER_WEBHOOKS='[{"name":"risk","url":"https://risk.example.com/hooks/ledger",
  "events":["transfer_posted","review_rejected","pain001_status_changed"],
  "secret":"<redacted>"}]' \
  ./ledger-api-go
```

Endpoint object fields: `name` (required, unique), `url` (required,
absolute http(s)), `events` (event-type filter; empty means all),
`secret` (single-key shorthand; `key_id` names it, default `primary`)
or `keys` (a list of `{id, secret, retired_at?}` — the first is the
primary, the rest are retired keys still inside their grace period),
`enabled` (default true), `max_attempts` (default 8),
`initial_backoff`/`max_backoff` (defaults `500ms`/`30s`, Go duration
strings), `http_timeout` (default `10s`), `queue_size` (default 1024),
`key_grace_period` (default `24h`). An invalid value fails startup
fast; an endpoint without key material refuses to register — unsigned
webhooks can be forged.

Delivery contract, stated honestly:

- Every delivery POSTs the outbox event JSON with
  `Content-Type: application/json`,
  `X-Ledger-Signature: sha256=<hex>` (`HMAC-SHA256(key, body)`),
  `X-Ledger-Key-Id` (which signing key), `X-Ledger-Event-Type`, and
  `X-Ledger-Event-Seq`.
- The posting path never blocks: fan-out uses non-blocking sends into
  per-endpoint bounded queues, so a slow or dead endpoint cannot stall
  `POST /entries` / `POST /transfers`. A full queue drops the event
  (counted) — the outbox stream stays the durable record; webhooks are
  a live fan-out, and a crash loses whatever sat in the in-memory
  queues.
- Retries use exponential backoff (doubling, plus up to +25% jitter)
  up to `max_attempts`. Network errors, `429`s and `5xx` retry; other
  `4xx` are terminal (the receiver rejects the payload — retrying the
  same bytes will not help).
- A delivery that exhausts its retries is recorded as `failed` and
  counted by `ledger_webhook_failures_total` — alert on growth.
- Signing keys rotate without downtime: `RotatingKey` keeps exactly
  one primary (which signs) while retired keys keep verifying for the
  grace period, so in-flight payloads signed with the old key stay
  verifiable. Rotate at runtime via the Go API
  (`dispatcher.RotateKey(endpoint, newID, newSecret)`).
- `GET /webhooks/deliveries` queries recent delivery records
  (`event_seq`, `endpoint`, `attempts`, `status`, `last_error`, `at`),
  latest first, with `?endpoint=`, `?status=pending|delivered|failed`,
  and `?limit=` (1–100, default 50). Without `LEDGER_WEBHOOKS` it 404s
  fail-closed.

```bash
curl -s "localhost:8080/webhooks/deliveries?status=failed&limit=20"
# {"deliveries":[{"event_seq":42,"endpoint":"risk","attempts":8,
#   "status":"failed","last_error":"http 500","at":"2026-10-16T09:30:00Z"}]}
```

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

### `GET /audit/export`

Builds a read-only snapshot of the sealed audit chain (LG-128): the
audit files for the `?from=&to=` UTC-day range (inclusive,
`YYYY-MM-DD`; omitted means all) are copied byte-for-byte into a temp
dir as `0444` files, and the snapshot's `manifest.json` is streamed
back as the response body. The temp dir is removed after the response —
the endpoint is stateless; operators who need a persistent snapshot
call `ledger.ExportAuditChain` from a small Go program (the ledger
package function is the CLI-usable entry point: pass the audit dir, an
`AuditChainExportOptions{FromDay, ToDay, ExportDir, SigningKeys}`, and
it writes the snapshot plus `manifest.json`).

```bash
curl -s 'localhost:8080/audit/export?from=2026-10-01&to=2026-10-15'
# {"format":"audit-chain-export/1","exported_at":"2026-10-18T08:00:00.1Z",
#  "from_day":"2026-10-01","to_day":"2026-10-15",
#  "files":[{"name":"audit-2026-10-09.jsonl","day":"2026-10-09",
#             "sha256":"9f2c…","size":1234}],
#  "report":{"ok":true,"checked_entries":128,"skipped_lines":0,
#            "files_checked":3,"head":"9f2c…"},
#  "signed":true,"signature":{"kid":"k1","mac":"…"}}
```

Manifest fields:

- `format` — `"audit-chain-export/1"`.
- `exported_at` — export time, UTC RFC3339Nano (informational; excluded
  from the signed bytes).
- `from_day` / `to_day` — the requested range, echoed back.
- `files[]` — one entry per copied file: `name`, `day`, `sha256` (hex
  SHA-256 of the copied bytes, so the snapshot verifies byte-for-byte),
  `size`.
- `report` — `VerifyAuditLog` replayed over the exported copies:
  `ok`, `checked_entries`, `skipped_lines`, `files_checked`, and
  `head` (the seal-chain head hash of the snapshot).
- `signed` / `signature` — the manifest is HMAC-signed with
  `LEDGER_AUDIT_SIGNING_KEYS` when configured (`kid` + `mac` over the
  canonical manifest JSON with `signature` absent and `exported_at`
  blanked — the same convention as per-line audit signing, LG-65); with
  no keys it is honestly recorded as `signed:false` with no signature,
  never a failure.

Semantics worth knowing:

- The export never touches the source audit files — it only reads them.
  A tampered source exports faithfully and the manifest's report shows
  `ok:false`; the export still completes.
- A day range that starts after the log's first day cannot link the
  genesis marker, so `report.ok` is honestly `false` with the break on
  the earliest copied entry (`prev_mismatch`). For a fully verifiable
  export, start the range at the log's first day.
- `200` with `report.ok:false` is still a `200` — like
  `GET /audit/verify`, unhealthy findings live in the body. A bad day
  format is `400`. `404 {"error":"audit log disabled"}` — no
  `LEDGER_AUDIT_DIR` configured; fail-closed.
- Counted by `ledger_audit_export_total` /
  `ledger_audit_export_breaks_total` — alert on any nonzero breaks.

### Audit log retention (`LEDGER_AUDIT_RETENTION_DAYS`)

Compliance regimes require audit trails to be kept for a defined period
— and privacy regimes require them not to be kept forever.
`LEDGER_AUDIT_RETENTION_DAYS` sets the retention window in whole days
(integer; unset or `0` means unlimited retention). A negative or
non-integer value fails startup fast.

At startup the server runs one retention sweep
(`ledger.AuditLog.ApplyAuditRetention`, also callable periodically —
it is idempotent): whole audit files whose UTC day is strictly older
than `(today - retentionDays)` are deleted, plain `.jsonl` and `.gz`
copies alike. Inside the retention window files are append-only (WORM)
— the sweep never modifies a file, only deletes whole ones past the
cutoff. Files whose day cannot be parsed from the filename are never
deleted, and today's active file is never a candidate.

- Before deleting anything, the sweep records an
  `audit_retention_deleted` event in the trail (`deleted_files`,
  `cutoff_day`, `retention_days`, plus each deleted file's last sealed
  hash so the cut is auditable). The event lands in today's active file,
  which is never deleted. If the event cannot be enqueued the sweep
  aborts and deletes nothing — an unaudited deletion must not happen —
  and startup fails fast.
- Deleting old files truncates the verifiable hash chain: the oldest
  retained file's first entry still links the deleted history's last
  seal, so `GET /audit/verify` reports a `prev_mismatch` at that
  boundary. That break is the expected, documented signal of a
  retention cut — not tampering — and the `last_seals` in the deletion
  event show exactly where the chain was cut.
- `ledger_audit_retention_deletions_total` counts deleted files in
  `/metrics` (synced from the ledgers on every scrape, like the other
  ledger-owned counters).
- A retention of 2+ days keeps the sweep a full day away from the
  background gzip rotation window; at 1 day a file can race the
  midnight gzip and survive one sweep to be caught by the next —
  benign, but use ≥ 2 days for a quiet boundary.

### API key management (`POST /api-keys`, LG-130)

Machine-to-machine authentication for the HTTP API. The ledger
historically had no auth at all (operator endpoints relied on network
posture plus the `x-ledger-actor` header, which is attribution, not
authentication); API keys close that gap for automation
(reconciliation jobs, payout workers) without disturbing the
anonymous-operator flow:

- `ledger.WithAPIKeyAuth()` enables enforcement; the middleware is a
  pure pass-through until then, so existing deployments and tests are
  unaffected.
- `POST /api-keys` mints a key (`{"name","scopes","expires_at?",
  "rate_limit_per_min?"}` → `201 {"key","secret"}`). The secret
  (`lak_` + 64 hex chars) is shown **exactly once** — the ledger stores
  only its SHA-256 hash, so a snapshot/database leak never yields a
  usable credential.
- Scopes are coarse and explicit: `read:ledger`, `write:entries`,
  `write:transfer`, `admin:audit`, `admin:keys` (`admin:keys` implies
  everything). Reads need `read:ledger`; `POST /entries*` needs
  `write:entries`; `POST /transfers` needs `write:transfer`; review
  decisions and every other mutating operator path need `admin:audit`.
- `POST /api-keys/{id}/rotate` (`{"grace_seconds"}`) issues a fresh
  secret for the same identity; the old secret dual-runs through the
  grace window so rolling deployments never drop in flight. `POST
  /api-keys/{id}/revoke` retires immediately (idempotent).
- Per-key per-minute rate quotas (opt-in) bound a compromised key's
  blast radius; over-quota requests get `429`.
- Every lifecycle change emits a structured audit event
  (`api_key_created` / `api_key_rotated` / `api_key_revoked`) with
  who/what/when — key management itself is tamper-evident. When
  `x-ledger-actor` is absent, requests are attributed as
  `apikey:<name>`.
- Bootstrap: with zero keys issued, `POST /api-keys` is open so the
  first operator key can be minted; everything else is `401` until a
  key exists.
- Keys round-trip through disaster-recovery snapshots (hashes only —
  existing keys keep authenticating after a restore; rotate for fresh
  secrets).
- Metrics: `ledger_api_key_auth_failures_total`,
  `ledger_api_key_creations_total`.

### PII erasure — the right to be forgotten (`POST /pii/erase`, LG-131)

A ledger is append-only by design: journal rows are sealed into a
tamper-evident hash chain, so "deleting" a journaled business note
would destroy the audit property the system is built on. Erasure
therefore works in two layers:

- **Account holder names** (`ledger.SetAccountHolderName`) live in a
  mutable PII registry. Erasing one overwrites the stored bytes with
  cryptographic randomness and drops the record — true deletion, with
  a tombstone left behind.
- **Entry memos** live on sealed journal rows whose bytes the chain
  hash covers. Erasing a memo does **not** rewrite the row (that
  would break `VerifyChain`); instead the erasure is recorded in an
  erasure registry, and every read/export path (`GET /entries`, `GET
  /accounts/{id}/entries`, OFX export) redacts the memo to
  `"[erased]"`. The books stay verifiable, the PII stops being
  disclosed.

Erasure is maker-checker: `POST /pii/erase` (`{"account","fields"}`,
→ `202`) files a pending request; a **different** reviewer must
approve it via `POST /pii/erase/{id}/approve` — self-approval is
rejected with `403` (the LG-126 dual-control convention). `POST
/pii/erase/{id}/reject` terminally rejects. `GET
/pii/erasures?account=` lists the tombstones.

- The erasable field set is a closed whitelist (`holder_name`,
  `memo`). Amounts, accounts, currencies, journal legs — the
  financial facts — can never be erased: requests naming them fail
  with `400`.
- The erasure event itself is appended to the structured audit chain,
  so the chain's hash continuity is preserved — erasure is proven,
  not hidden.
- `GET /audit/export` manifests annotate `erasure_version` /
  `erasure_count`, so downstream consumers can tell whether their
  copy predates an erasure.
- Erasure marks round-trip through disaster-recovery snapshots (a
  restore stays erased); unerased holder names round-trip as working
  data.
- Metrics: `ledger_pii_erasure_requests_total`,
  `ledger_pii_erasures_total`.

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
- `ledger_posting_rule_legs_total{rule}` — derived journal legs committed
  by automatic posting rules (LG-132), one series per rule name.
- `ledger_outbox_stream_connections` — currently open
  `GET /events/stream` live tails (gauge).
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
- `ledger_sweep_fx_total` — every `POST /sweeps/fx` request received.
- `ledger_sweep_fx_idempotency_hits_total` — cross-currency sweeps that
  replayed an existing idempotency key (returned the original receipt,
  booked nothing).
- `ledger_merges_total` — every `POST /merges` request received.
- `ledger_merge_idempotency_hits_total` — merges that replayed an existing
  idempotency key (returned the original receipt, booked nothing).
- `ledger_splits_total` — every `POST /accounts/{id}/split` request received.
- `ledger_split_idempotency_hits_total` — splits that replayed an existing
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
- `ledger_audit_export_total` — `GET /audit/export` requests that built
  an audit-chain export snapshot (LG-128).
- `ledger_audit_export_breaks_total` — audit-chain exports whose
  verification report found a broken chain. Any nonzero value is an
  integrity incident — alert on it.
- `ledger_balance_queries_total` — `GET /accounts/{id}/balance` requests
  served. Snapshot reads are not counted.
- `ledger_balance_at_queries_total` — `GET /accounts/{id}/balance-at`
  requests served.
- `ledger_ofx_exports_total` — `GET /accounts/{id}/ofx` OFX statement
  exports served.
- `ledger_ofx_imports_total` — `POST /ofx/import` OFX statement
  pre-posting imports served, including duplicates and rejected
  payloads.
- `ledger_camt053_imports_total` — `POST /camt053/import` camt.053
  statement pre-posting imports served, including duplicates and
  rejected payloads.
  debit/credit notification imports served, including duplicates
  and rejected payloads.
- `ledger_fednow_pacs008_requests_total` — `POST /fednow/pacs008`
  requests received, including rejected documents.
- `ledger_fednow_postings_total` — FedNow pacs.008 receipt postings
  committed (idempotent replays not counted twice).
- `ledger_fednow_rejections_total{reason}` — per-item pacs.008
  rejections by reason (idempotent replays not counted).
- `ledger_camt054_imports_total` — `POST /camt054/import` camt.054
  debit/credit notification imports served, including duplicates
  and rejected payloads.
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
- `ledger_large_tx_reports_total` — large-transaction (CTR) reports
  newly generated by `POST /compliance/large-tx-reports` (replays of
  already-generated reports are not counted).
- `ledger_frozen_rejections_total` — `POST /entries`, `POST /transfers`,
  `POST /sweeps`, `POST /merges`, and hold requests rejected with `403`
  because an account was frozen.
- `ledger_overdraft_rejections_total` — `POST /entries`, `POST
  /transfers`, `POST /merges`, and capture requests rejected with `422`
  because the posting would have overdrawn an overdraft-protected account.
- `ledger_kyc_rejections_total` — `POST /entries`, `POST
  /entries/batch`, and `POST /transfers` requests rejected with `422` by
  the KYC/尽调 gate: an expired profile blocks any amount, and an
  unverified (or pending) account is rejected above the ledger's opt-in
  KYC threshold. Replays never count — the gate runs after the
  idempotency replay check.
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
- `ledger_period_autocloses_total` — period auto-close plans fired by
  the background sweeper, executed and failed: a failed plan keeps its
  period open, so the count records the fire, not the outcome.
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
- `ledger_alias_sets_total` — `PUT /accounts/{id}/alias` requests served
  successfully (alias bindings, rebinds, and clears; rejected binds are
  not counted).
- `ledger_entry_links_total` — entry links declared (see "Entry links"
  above): explicit directed associations between journal entries
  (`reversal_of`, `adjustment_of`, `fee_of`, `refund_of`), including the
  `reversal_of` links `POST /entries/{id}/reverse` and
  `POST /entries/{id}/reverse-partial` declare automatically, plus the
  `mandate_of` links `POST /mandates/{id}/debits` declares (see
  "Direct-debit mandates" above). Idempotent replays of an identical
  link are not counted.
- `ledger_mandate_debits_total` — direct-debit mandate debits posted
  (see "Direct-debit mandates" above): pulls authorized by a mandate
  and booked through the full `POST` validation sequence. Idempotent
  replays of a debit key are not counted.
- `ledger_pain008_exports_total` — `GET /exports/pain008` ISO 20022
  pain.008 direct-debit initiation exports served.
- `ledger_camt052_exports_total` — `GET /exports/camt052` ISO 20022
  camt.052 intraday account report exports served, including rejected
  exports.
- `ledger_pacs002_exports_total` — `GET /exports/pacs002` ISO 20022
  pacs.002 FI-to-FI payment status report exports served, including
  rejected exports.
- `ledger_camt056_exports_total` — `GET /exports/camt056` ISO 20022
  camt.056 FI-to-FI cancellation request exports served, including
  rejected exports.
- `ledger_camt029_applies_total` — `POST /exports/camt029/apply` ISO
  20022 camt.029 cancellation status report applies served, including
  rejected reports.
- `ledger_camt056_exports_total` — `GET /exports/camt056` ISO 20022
  camt.056 FI-to-FI cancellation request exports served, including
  rejected exports.
- `ledger_camt029_applies_total` — `POST /exports/camt029/apply` ISO
  20022 camt.029 cancellation status report applies served, including
  rejected reports.
- `ledger_gpi_track_queries_total` — `GET /gpi/track/{uetr}` tracking
  queries served.
- `ledger_gpi_advances_total` — `POST /gpi/track/{uetr}/advance`
  operator advances served, including rejected advances.
- `ledger_gpi_links_total` — `POST /gpi/link` transfer-UETR
  associations served, including rejected links.
- `ledger_gpi_tracked_total` — gpi-tracked cross-border payments opened
  (one per payout drafted), synced from the ledger on every scrape.
- `ledger_gpi_settled_total` — gpi-tracked payments that reached `ACCP`.
- `ledger_gpi_rejected_total` — gpi-tracked payments that reached `RJCT`.
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
- `ledger_partial_captures_total` — every
  `POST /holds/{id}/capture/partial` request received.
- `ledger_releases_total` — `POST /holds/{id}/release` requests received.
- `ledger_hold_sweeps_total` — hold-expiry sweeps: `POST /holds/expire`
  requests received plus background hold-sweeper ticks (see
  `LEDGER_HOLD_SWEEP_INTERVAL` below).
- `ledger_term_deposits_total` — term deposits opened:
  `POST /term-deposits` requests received.
- `ledger_term_deposit_idempotency_hits_total` — term-deposit opens
  that replayed an existing idempotency key.
- `ledger_term_deposit_accruals_total` — term-deposit interest accrual
  requests received.
- `ledger_term_deposit_maturities_total` — term-deposit maturity
  requests received.
- `ledger_term_deposit_withdrawals_total` — term-deposit early
  withdrawals received.
- `ledger_term_deposit_sweeps_total` — `POST /term-deposits/sweep`
  requests received.
- `ledger_term_deposit_rejections_total` — term-deposit requests rejected
  (insufficient available funds, not-yet-matured, conflict, …).
- `ledger_payouts_total` — `POST /payouts` requests received.
- `ledger_payouts_settled_total` — payout instructions that reached settled.
- `ledger_payouts_rejected_total` — payout requests rejected
  (maker-checker violations, invalid transitions, statement mismatches,
  Post-gate rejections on the submit/settlement legs).
- `ledger_credit_lines_total` — credit lines granted plus
  reduce/revoke requests received.
- `ledger_credit_interest_accruals_total` — credit-line interest
  accrual requests received.
- `ledger_credit_limit_rejections_total` — postings and accruals rejected
  at the credit-line floor (`ErrCreditLimitExceeded`).
- `ledger_ecl_exposure_requests_total` — `POST /ecl/exposures` requests
  received.
- `ledger_ecl_provision_requests_total` — `POST /ecl/provisions`
  requests received.
- `ledger_ecl_provision_replays_total` — `POST /ecl/provisions`
  requests that replayed an already-booked provision entry for the day.
- `ledger_ecl_provision_rejections_total` — `POST /ecl/provisions`
  requests rejected (no tracked exposure, unconfigured booking
  accounts, dust provision).
- `ledger_ecl_provisions_total` — ECL provision journal entries
  committed (idempotent replays not counted twice).
- `ledger_ecl_stage_migrations_total` — ECL stage migrations recorded
  (`ecl_stage_migration` audit events).
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
- `ledger_settlement_deferred_total` — transfers deferred to the next
  settlement window: transfers posted after their currency's settlement
  cutoff (LG-101).
- `ledger_settlement_next_window_queries_total` — `GET
  /settlement/next-window` requests served.
- `ledger_settlement_alerts_total` — settlement mismatch alerts across
  served settlement reports: the day-end breaks an operator works
  through, not posting rejections.
- `ledger_aging_alerts_total` — AR aging overdue alerts across served
  `GET /aging` reports: accounts whose 61-90 + 90+ day overdue cents
  exceeded the report's alert threshold.
- `ledger_dunning_escalations_total` — collections escalations
  (`dunning_escalated` audit events) fired by `EscalateDunning`. Synced
  from the ledger on every scrape.
- `ledger_period_preflight_runs_total` — every
  `POST /periods/{id}/preflight` request received, including rejected
  ones.
- `ledger_api_key_auth_failures_total` — requests rejected by the API
  key middleware (missing/invalid/expired/revoked credentials,
  insufficient scope, rate-quota denials).
- `ledger_api_key_creations_total` — API keys issued through the HTTP
  API (creations and rotations).
- `ledger_pii_erasure_requests_total` — PII erasure requests filed
  (`POST /pii/erase` requests that passed validation).
- `ledger_pii_erasures_total` — PII erasures executed after
  dual-control approval.

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
- `LEDGER_PERIOD_AUTOCLOSE_INTERVAL` — enables the background period
  auto-close worker, e.g. `LEDGER_PERIOD_AUTOCLOSE_INTERVAL=1m`. While
  enabled, the server fires due plans registered with `POST
  /periods/{id}/auto-close` on the interval (executed plans close the
  period through the same path as `POST /periods/{id}/close`; failed
  plans keep their period open and are never retried by the sweeper).
  Every tick's decided plans are counted in
  `ledger_period_autocloses_total`. The first tick fires after one full
  interval; SIGINT/SIGTERM stops the worker with the server. Unset or
  invalid means disabled — periods stay on manual close.
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
  fast. The revenue account also collects term-deposit early-withdrawal
  penalties.
- `LEDGER_TD_INTEREST_EXPENSE` — the ledger-wide interest-expense
  account for term-deposit daily interest accruals (e.g.
  `LEDGER_TD_INTEREST_EXPENSE=interest-expense`). Accruals are
  fail-closed: without this set, `POST /term-deposits/{id}/accrue`
  returns `500`.
- `LEDGER_PAYOUT_CLEARING_ACCOUNT` — the ledger-wide clearing account
  that payout submit legs debit (e.g.
  `LEDGER_PAYOUT_CLEARING_ACCOUNT=payout-clearing`). Submits are
  fail-closed: without this set, `POST /payouts/{id}/submit` returns
  `500`.
- `LEDGER_CREDIT_INTEREST_INCOME` — the ledger-wide interest-income
  account for credit-line utilization interest (e.g.
  `LEDGER_CREDIT_INTEREST_INCOME=interest-income`). Accruals are
  fail-closed: without this set, `POST /credit-lines/accrue-interest`
  returns `500`.
- `LEDGER_ECL_EXPENSE` — the ledger-wide credit-loss expense account
  for ECL provisions (e.g. `LEDGER_ECL_EXPENSE=ecl-expense`).
  Provisions are fail-closed: without this set, `POST /ecl/provisions`
  returns `500`.
- `LEDGER_ECL_ALLOWANCE` — the ledger-wide loss-allowance account for
  ECL provisions (e.g. `LEDGER_ECL_ALLOWANCE=ecl-allowance`).
  Provisions are fail-closed: without this set, `POST /ecl/provisions`
  returns `500`.
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
- `LEDGER_LARGE_TX_THRESHOLDS` — per-currency large-transaction (CTR)
  reporting thresholds, `"<currency>:<thresholdCents>,..."` (e.g.
  `LEDGER_LARGE_TX_THRESHOLDS="USD:1000000,EUR:900000"`): an account's
  UTC-day inflow or outflow in that currency at/above the threshold
  generates a report into the structured audit chain on `POST
  /compliance/large-tx-reports`. Unset means no reporting; an invalid
  value fails startup fast.
- `LEDGER_LARGE_TX_REPORT_CURRENCY` — the currency converted
  large-transaction report amounts are disclosed in (default `USD`); an
  invalid value fails startup fast.
- `LEDGER_BUSINESS_DAY_WEEKEND` — the business-day calendar's weekly
  rest days, e.g. `LEDGER_BUSINESS_DAY_WEEKEND="Fri,Sat"` (default
  `Sat,Sun`); full and abbreviated English weekday names,
  case-insensitive. Unset means the default weekend. An unknown name
  fails startup fast.
- `LEDGER_BUSINESS_DAY_HOLIDAYS` — extra non-business dates for the
  business-day calendar, e.g.
  `LEDGER_BUSINESS_DAY_HOLIDAYS="2026-01-01,2026-10-01"`. A malformed
  date fails startup fast.
- `LEDGER_ACCRUAL_BUSINESS_DAYS_ONLY` — set to `1`/`true` to make
  interest and fee accruals business-day-only (a non-business-day `asof`
  is rejected with `ErrNotBusinessDay`). Unset means calendar-day
  accruals, the historical behavior.
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
- `LEDGER_AUDIT_RETENTION_DAYS` — the audit-log retention window in whole
  days (see "Audit log retention" above): whole audit files older than the
  window are deleted by a sweep at startup, recorded as an
  `audit_retention_deleted` event in the trail itself. Unset or `0` means
  unlimited retention; a negative or non-integer value fails startup fast.
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
- `LEDGER_WEBHOOKS` — enables the outbound signed-webhook dispatcher
  (see above): a JSON array of endpoint objects (`name`, `url`,
  `events`, `secret`/`keys`, plus optional tunables — see
  `ledger.ParseWebhookEndpoints`). Unset means disabled. Requires
  `LEDGER_OUTBOX_DIR` alongside (webhooks subscribe to the outbox
  stream); an invalid value fails startup fast. Secrets ride in the
  JSON — fine for dev/test; production deployments should render this
  variable from a secret manager rather than baking it into an image.

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
│   ├── alias.go               # human-readable account aliases (unique, audited) + alias resolution
│   ├── ledger_alias_test.go   # alias set/rebind/clear/conflict/validation/self-bind, audit events, snapshot round-trip, concurrency
│   ├── main_alias_test.go     # PUT/GET /accounts/{id}/alias + alias resolution/echo on account-scope reads
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
