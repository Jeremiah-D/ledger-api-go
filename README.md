# ledger-api-go

> **Portfolio reconstruction** — a rebuild project created to demonstrate ledger/settlement engineering skills. This is not production code from any employer.

## Inspiration

The design borrows its posting semantics from a publicly described concept: **"idempotent fill settlement"** — the idea that re-submitting the same settlement must never book twice. Double-entry accounting is used here purely as this repo's own implementation pattern; no claim is made about any employer's systems.

## What this implements

- An in-memory double-entry ledger (`ledger` package) written with the Go standard library only.
- Money is represented as integer cents (`int64`); floating point is never used for amounts.
- Every journal entry touches exactly two accounts: the debit account's balance increases by the amount, the credit account's decreases by the same amount, so the sum of all balances is always zero.
- Entries are validated before booking: non-empty ID, distinct non-empty debit/credit accounts, and `amount_cents > 0`.
- Posting is idempotent: submitting an entry with a previously used `IdempotencyKey` returns the original entry and books nothing again. All state is guarded by a mutex and safe for concurrent use.
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
- Invalid entry (empty account, debit == credit, amount ≤ 0, malformed JSON) → `400` with an `{"error": ...}` body.

### `GET /accounts/{id}/balance`

```bash
curl -s localhost:8080/accounts/cash/balance
# {"account":"cash","balance_cents":1000}
```

## Running

Requires Go 1.27+.

```bash
go vet ./...
go test -race ./...
go run .                 # listens on :8080; override with PORT, e.g. PORT=9090 go run .
```

## Layout

```
.
├── ledger/
│   ├── ledger.go       # Ledger, JournalEntry, Post, Balance, GetByIdempotencyKey
│   └── ledger_test.go  # validation, idempotency, concurrency tests
├── main.go             # net/http JSON API (thin assembly only)
└── .github/workflows/ci.yml
```

## License

MIT
