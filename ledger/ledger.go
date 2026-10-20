// Package ledger implements a minimal in-memory double-entry ledger with
// idempotent posting.
//
// A journal entry always touches exactly two accounts: the debit account is
// increased by the entry amount and the credit account is decreased by the
// same amount, so the sum of all balances is invariantly zero. Amounts are
// stored as integer cents; floating point is never used for money.
//
// Posting is idempotent: when a JournalEntry carries an IdempotencyKey that
// was already posted, the original entry is returned and no second booking
// occurs.
package ledger

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// AccountID identifies an account in the ledger.
type AccountID string

// JournalEntry is a single double-entry journal posting.
//
// Currency is the ISO 4217 alpha-3 code the posting is denominated in
// (e.g. "USD", "EUR", "CNY"). An empty currency on input means the
// default currency (see DefaultCurrency); Post normalizes it before
// committing, so journaled entries always carry an explicit code and the
// audit-chain hash covers it like every other journaled field. One entry
// is always single-currency: debit and credit legs can never span
// currencies, and balances are tracked per (account, currency), so the
// accounting equation is verified per currency, never across them.
type JournalEntry struct {
	ID             string    `json:"id"`
	DebitAccount   AccountID `json:"debit_account"`
	CreditAccount  AccountID `json:"credit_account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	// BatchID names the atomic batch this entry was posted in (see
	// PostBatch in batch.go); it is empty for entries posted individually.
	// It is query metadata: the audit chain covers the entry itself, and
	// VerifyChain/Reconcile cover batch entries exactly like ordinary
	// postings.
	BatchID string `json:"batch_id,omitempty"`
	// Memo is a free-form business note attached to the entry at post
	// time (an order ID, an invoice reference, a reconciliation tag).
	// It is part of the journaled record: the audit-chain hash covers
	// it, snapshots carry it, and GET /entries?memo= filters on it. At
	// most 255 UTF-8 characters (see ErrMemoTooLong); longer memos are
	// rejected with a 400 before anything is recorded. Transfers carry
	// the memo on the principal entry (see Transfer.Memo).
	Memo string `json:"memo,omitempty"`
	// ExternalRef is the caller's external business reference for the
	// entry (a payment provider's transaction ID, an external order
	// number, a reconciliation reference). Unlike the memo it is a
	// uniqueness domain: within one ledger no two journal rows may
	// carry the same non-empty external reference, so downstream
	// systems can correlate journal rows with external records
	// without ambiguity. At most 64 UTF-8 characters (see
	// ErrExternalRefTooLong); a posting that would duplicate an
	// already-journaled reference is rejected with
	// ErrDuplicateExternalRef. The audit-chain hash covers it, like
	// the memo, and GET /entries?ref= filters on it. Independent of
	// the idempotency-key namespace: an idempotent replay returns the
	// original entry without consulting the reference index.
	ExternalRef string `json:"external_ref,omitempty"`
	// Attachments are caller-supplied evidence references for the entry
	// (a receipt scan, an invoice PDF, a compliance document): at most
	// maxAttachments https URIs, each at most maxAttachmentRunes UTF-8
	// characters (see ErrInvalidAttachment). They are journal metadata
	// like the memo: the audit-chain hash covers them, snapshots carry
	// them, GET /entries echoes them, and idempotent replays return them
	// with the original entry. Transfers stamp them on the principal
	// entry (the fee leg carries none), reversals inherit the original
	// entry's attachments, and captures stamp the capture's attachments
	// on the settlement entry. Holds are off-journal reservations and
	// carry no attachments.
	Attachments []string `json:"attachments,omitempty"`
	// Metadata is caller-supplied structured metadata for the entry:
	// downstream reconciliation tags like channel=alipay or
	// risk_score=12. At most maxMetadataPairs pairs; keys are 1-64
	// characters of [A-Za-z0-9._-], values at most
	// maxMetadataValueRunes UTF-8 characters (see ErrInvalidMetadata);
	// violations are rejected with a 400 before anything is recorded.
	// It is part of the journaled record: the audit-chain hash covers
	// it (sorted keys, deterministic encoding; entries without metadata
	// hash exactly as before), snapshots carry it, GET /entries echoes
	// it and GET /entries?meta.<key>=<value> filters on it, and
	// idempotent replays return it with the original entry. Transfers,
	// batch items, reversals, and captures stamp the caller's metadata
	// on the principal/settlement entry; fee legs, FX conversion legs,
	// journal-rule derived legs, and other internal/system postings
	// carry no metadata — it is never inherited, only explicitly set.
	Metadata map[string]string `json:"metadata,omitempty"`
	// PendingReview marks a journal row frozen by the large-transfer
	// dual-control review (see review.go): the row is booked but not
	// effective — balances, debit/credit totals, the ledger version,
	// and the audit chain exclude it until an operator approves the
	// review, at which point the marker flips to effective and the legs
	// commit like an ordinary transfer. The audit-chain hash covers the
	// marker, so rewriting it breaks the chain exactly like rewriting
	// an amount. It is query metadata like BatchID: VerifyChain and
	// Reconcile treat pending rows as journaled rows, while Balance,
	// TrialBalance, and VerifyAccountingEquation only ever see
	// effective rows (they read balances/totals, which pending rows
	// never touch).
	PendingReview bool      `json:"pending_review,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Validation errors returned by Post.
var (
	ErrEmptyID            = errors.New("ledger: journal entry ID must not be empty")
	ErrEmptyDebitAccount  = errors.New("ledger: debit account must not be empty")
	ErrEmptyCreditAccount = errors.New("ledger: credit account must not be empty")
	ErrSameAccount        = errors.New("ledger: debit and credit accounts must differ")
	ErrNonPositiveAmount  = errors.New("ledger: amount must be greater than zero")
	ErrInvalidCursor      = errors.New("ledger: pagination cursor is invalid")
	ErrInvalidLimit       = errors.New("ledger: limit must be between 1 and 1000")
	ErrMemoTooLong        = errors.New("ledger: memo must be at most 255 UTF-8 characters")
	// ErrExternalRefTooLong is returned when a journal entry's external
	// business reference exceeds maxExternalRefRunes UTF-8 characters.
	ErrExternalRefTooLong = errors.New("ledger: external_ref must be at most 64 UTF-8 characters")
	// ErrDuplicateExternalRef is returned when a posting carries an
	// external business reference that is already journaled on another
	// entry. The reference namespace is per ledger and independent of
	// the idempotency-key namespace; an idempotent replay of the
	// original entry does not consult it.
	ErrDuplicateExternalRef = errors.New("ledger: external_ref already used by another journal entry")
	// ErrInvalidAttachment is returned when a journal entry's
	// attachments violate the attachment contract: more than
	// maxAttachments entries, a non-https URI, or a URI longer than
	// maxAttachmentRunes UTF-8 characters.
	ErrInvalidAttachment = errors.New("ledger: attachments must be at most 8 https URIs of at most 512 characters each")
	// ErrInvalidMetadata is returned when a journal entry's metadata
	// violates the metadata contract: more than maxMetadataPairs
	// pairs, a key outside the 1-64 character [A-Za-z0-9._-] charset,
	// or a value longer than maxMetadataValueRunes UTF-8 characters.
	ErrInvalidMetadata = errors.New("ledger: metadata must be at most 16 pairs with keys of 1-64 characters [A-Za-z0-9._-] and values of at most 256 UTF-8 characters")
)

// maxMemoRunes caps a journal entry's memo at 255 UTF-8 characters. Long
// enough for an order ID plus a human note, short enough to keep journal
// rows and the audit chain bounded.
const maxMemoRunes = 255

// checkMemoLength rejects memos longer than maxMemoRunes UTF-8
// characters. Callers: validateJournalEntry (Post and PostBatch entries)
// and PostTransfer (transfer-level memo).
func checkMemoLength(memo string) error {
	if utf8.RuneCountInString(memo) > maxMemoRunes {
		return ErrMemoTooLong
	}
	return nil
}

// maxExternalRefRunes caps a journal entry's external business reference
// at 64 UTF-8 characters. Long enough for a payment provider's
// transaction ID or an external order number, short enough to keep the
// reference index compact.
const maxExternalRefRunes = 64

// checkExternalRefLength rejects external references longer than
// maxExternalRefRunes UTF-8 characters. Callers: validateJournalEntry
// (Post and PostBatch entries), PostTransfer (transfer-level reference),
// and Capture (capture-level reference).
func checkExternalRefLength(ref string) error {
	if utf8.RuneCountInString(ref) > maxExternalRefRunes {
		return ErrExternalRefTooLong
	}
	return nil
}

// maxAttachments caps a journal entry's evidence attachments at 8 URIs:
// enough for a receipt scan plus supporting documents, few enough to
// keep journal rows bounded.
const maxAttachments = 8

// maxAttachmentRunes caps one attachment URI at 512 UTF-8 characters:
// enough for a signed object-storage URL with query parameters, short
// enough to keep the journal and the audit chain bounded.
const maxAttachmentRunes = 512

// checkAttachments enforces the attachment contract: at most
// maxAttachments entries, each an https URI of at most
// maxAttachmentRunes UTF-8 characters. The https-only rule keeps
// evidence references tamper-resistant in transit; a URI that does not
// parse, has no host, or uses another scheme is rejected. Callers:
// validateJournalEntry (Post and PostBatch entries), PostTransfer
// (transfer-level attachments), and Capture (capture-level attachments).
func checkAttachments(atts []string) error {
	if len(atts) > maxAttachments {
		return ErrInvalidAttachment
	}
	for _, a := range atts {
		if utf8.RuneCountInString(a) > maxAttachmentRunes {
			return ErrInvalidAttachment
		}
		u, err := url.Parse(a)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return ErrInvalidAttachment
		}
	}
	return nil
}

// maxMetadataPairs caps a journal entry's structured metadata at 16
// pairs: enough for channel, risk score, and a handful of routing tags,
// few enough to keep journal rows and the audit chain bounded.
const maxMetadataPairs = 16

// maxMetadataValueRunes caps one metadata value at 256 UTF-8
// characters: enough for an order ID plus a sub-merchant path, short
// enough to keep the journal and the audit chain bounded. Keys reuse the
// tag-segment charset (see checkTagSegment): 1-64 characters of
// [A-Za-z0-9._-], so metadata keys stay safe in URLs and JSON without
// escaping rules.
const maxMetadataValueRunes = 256

// checkMetadata enforces the metadata contract: at most
// maxMetadataPairs pairs, each key 1-64 characters of [A-Za-z0-9._-]
// (via the shared tag-segment validator, reusing the alias/tag charset),
// each value at most maxMetadataValueRunes UTF-8 characters. Callers:
// validateJournalEntry (Post and PostBatch entries), PostTransfer
// (transfer-level metadata), Capture (capture-level metadata), and
// ReverseEntryWithMetadata (reversal-level metadata).
func checkMetadata(meta map[string]string) error {
	if len(meta) > maxMetadataPairs {
		return fmt.Errorf("%w: %d pairs, at most %d", ErrInvalidMetadata, len(meta), maxMetadataPairs)
	}
	for k, v := range meta {
		if err := checkTagSegment(k, ErrInvalidMetadata); err != nil {
			return err
		}
		if utf8.RuneCountInString(v) > maxMetadataValueRunes {
			return fmt.Errorf("%w: value for key %q exceeds %d UTF-8 characters", ErrInvalidMetadata, k, maxMetadataValueRunes)
		}
	}
	return nil
}

// maxPageSize caps a single ListEntries page.
const maxPageSize = 1000

// defaultKeyPruneInterval is the minimum time between two lazy idempotency-key
// sweeps when a TTL is configured. It keeps Post amortized O(1): without it,
// every Post would scan the whole key table.
const defaultKeyPruneInterval = time.Minute

// Ledger is an in-memory double-entry ledger. It is safe for concurrent use.
//
// Every successful Post bumps the ledger's version, a monotonically
// increasing sequence number. Versioned snapshots let reconciliation
// consumers detect whether anything changed between two reads: if the
// version is identical, the balance necessarily is too.
//
// Idempotency keys are kept in a separate index so replays can be detected
// without scanning the journal. The index grows with every distinct key, so
// a TTL can be configured (WithIdempotencyTTL): keys older than the TTL are
// eligible for eviction, which bounds memory in long-running processes. A
// zero TTL disables expiry entirely.
//
// A per-account index maps each account to the IDs of the entries that
// touched it (as debit or credit leg), so per-account listing scans only
// that account's entries instead of the whole journal.
//
// Every successful Post also appends one link to the tamper-evident audit
// chain (see chain.go): a SHA-256 link of the entry onto the previous
// link's hash. The chain makes silent rewrites of journaled entries
// detectable via VerifyChain.
//
// The mutex is an RWMutex: reads (Balance, Snapshot, ListEntries, ...) take
// the read lock and run concurrently, while Post and ExpireIdempotencyKeys
// take the write lock. Reads never block each other, only writers.
type Ledger struct {
	mu sync.RWMutex
	// balances, debitTotals, and creditTotals are keyed by
	// (account, currency): one account can hold several currencies side
	// by side, and the books for each currency stay isolated (see
	// currency.go). The per-account index below stays account-keyed —
	// it maps an account to every entry ID that touched it, in any
	// currency.
	balances     map[accountCurrency]int64
	entries      map[string]JournalEntry   // by entry ID
	byKey        map[string]JournalEntry   // by idempotency key
	byRef        map[string]string         // by external_ref: non-empty reference -> entry ID
	byAccount    map[AccountID][]string    // entry IDs per account, in Post order
	debitTotals  map[accountCurrency]int64 // total cents ever debited per (account, currency)
	creditTotals map[accountCurrency]int64 // total cents ever credited per (account, currency)
	frozen       map[AccountID]bool        // risk-control stops; frozen accounts reject new posts (see freeze.go)
	// frozenUntil carries the opt-in automatic-unfreeze deadline per
	// frozen account (see FreezeUntil in freeze.go): an account whose
	// deadline has passed is unfrozen lazily on the next write-path
	// frozen check or by the background freeze sweeper (see
	// freeze_sweep.go). Only accounts frozen with a deadline appear
	// here; a manual Freeze stores the zero time by deleting any
	// deadline, so a manual freeze never auto-releases.
	frozenUntil map[AccountID]time.Time
	// freezeEvents is the in-memory history of account freeze-state
	// transitions (see AccountFreezeEvent in statement.go), appended by
	// the freeze paths under the write lock and read by GenerateStatement
	// under the read lock. Operational index, not books: it does not
	// round-trip through snapshots (like the FrozenAutoReleases counter
	// below); the structured audit log stays the durable record.
	freezeEvents []AccountFreezeEvent
	// closed maps an account to its terminal lifecycle record (see
	// close.go): a closed account rejects every write path that would
	// book a new journal entry through it with ErrAccountClosed.
	// Closing is irreversible, unlike the reversible freeze above, and
	// the records are structural config — they do not bump the journal
	// version and they survive snapshots.
	closed map[AccountID]AccountClosure
	// suspended maps an account to its suspension record (see
	// suspend.go): a suspended account rejects every write path that
	// would book a new journal entry through it with
	// ErrAccountSuspended. Suspension is a reversible operational
	// pause, orthogonal to the frozen risk-control stop and the
	// terminal close above, and the records are structural config —
	// they do not bump the journal version and they survive
	// snapshots.
	suspended map[AccountID]AccountSuspension
	// dormant maps an account to its dormancy record (see dormancy.go):
	// a dormant account rejects operator Post and Transfer bookings
	// with ErrAccountDormant until ReactivateAccount clears the mark.
	// Dormancy is a state marking, not fund control — unlike the
	// terminal close above it is reversible, and unlike the frozen
	// risk-control stop it does not gate system adjusting entries
	// (accruals, dividends, dormancy fees), which post through the
	// system path. Structural config: marking never bumps the journal
	// version and survives snapshots.
	dormant map[AccountID]DormancyRecord
	// frozenAutoReleases counts automatic unfreezes (see FreezeUntil):
	// every lazy write-path release and every background-sweeper (or
	// explicit ExpireFrozenAccounts) release bumps it. Operational
	// metric, surfaced on GET /metrics as
	// ledger_freeze_auto_releases_total; not books, so it does not
	// round-trip through snapshots.
	frozenAutoReleases atomic.Uint64
	// chart maps an account to its registered chart-of-accounts type
	// (see chart.go): an opt-in posting-direction control. Accounts
	// without a row here skip the direction check. Structural config,
	// like frozen and noOverdraft: registering or clearing a type does
	// not bump the journal version and survives snapshots.
	chart map[AccountID]AccountType
	// cashAccounts marks accounts as cash-class for the cash-flow
	// statement (see cashflow.go): movements on these accounts are the
	// cash inflows and outflows the statement aggregates. The plain map
	// marks an account in every currency; cashAccountCurrencies marks
	// (account, currency) pairs for the scoped case — e.g. a
	// multi-currency settlement account that is cash-class only in its
	// settlement currency. Structural config, like chart: marking or
	// unmarking does not bump the journal version, changes are audited
	// (cash_account_marked / cash_account_unmarked), and both maps
	// round-trip through disaster-recovery snapshots.
	cashAccounts          map[AccountID]bool
	cashAccountCurrencies map[accountCurrency]bool
	// aliases maps a human-readable alias to the account it names, and
	// accountAliases is the reverse index (see alias.go). The mapping
	// is bijective: one alias names exactly one account, one account
	// carries at most one alias. Structural config, like tags and
	// chart: binding, rebinding, or clearing an alias does not bump the
	// journal version, changes are audited (alias_set/alias_changed/
	// alias_removed), and both indexes round-trip through
	// disaster-recovery snapshots.
	aliases        map[string]AccountID
	accountAliases map[AccountID]string
	// tags maps an account to its tag set (see tags.go): (key, value)
	// classification labels for tag-dimension reporting. Structural
	// config, like chart: setting or clearing tags does not bump the
	// journal version and survives snapshots. Only accounts with a
	// non-empty tag set appear here.
	tags map[AccountID]map[string]string
	// noOverdraft marks accounts guarded against overdrafts: a Post that
	// would take a protected credit (payer) account below zero is rejected
	// with ErrAccountOverdraft (see overdraft.go). Opt-in per account.
	noOverdraft map[AccountID]bool
	// kyc maps an account to its KYC/尽调 profile (see kyc.go): opt-in
	// per account. kycThresholdCents is the ledger-wide posting
	// threshold in cents above which accounts with no (or a
	// non-verified) profile are rejected; 0 disables the threshold
	// gate. Expired profiles block all new postings. Structural
	// config, like noOverdraft: profile changes do not bump the
	// journal version and survive disaster-recovery snapshots.
	kyc               map[AccountID]KycProfile
	kycThresholdCents int64
	// invoices maps invoice IDs to issued invoices (see invoice.go):
	// off-journal receivables. invoicesByAccount indexes them per
	// payer account for FIFO payment application. Invoices never
	// appear in entries, the audit chain, or the version counter.
	// Structural state, like holds: it round-trips through
	// disaster-recovery snapshots.
	invoices          map[string]Invoice
	invoicesByAccount map[AccountID][]string
	// deniedExact and deniedPrefixes are the sanctions/denylist (see
	// denylist.go): exact account IDs and prefix patterns whose accounts
	// may not move money. Post, PostTransfer, and Capture screen every
	// leg against them before the idempotency replay check, rejecting
	// hits with ErrAccountDenied. Opt-in; empty by default. Structural
	// config, like frozen and noOverdraft: list changes do not bump the
	// journal version and survive disaster-recovery snapshots.
	deniedExact    map[AccountID]DenylistEntry
	deniedPrefixes map[string]DenylistEntry
	// dailyLimits maps (account, currency) to the account's daily outflow
	// limit in cents, and dailyOutflow accumulates the outflow booked per
	// (account, currency, UTC day). A Post that would take the day's
	// cumulative outflow above the limit is rejected with
	// ErrDailyLimitExceeded (see daily_limit.go). Opt-in per account and
	// currency; structural config, like frozen and noOverdraft.
	dailyLimits  map[dailyLimitKey]int64
	dailyOutflow map[dailyLimitKey]int64
	// sweepReserves maps (account, currency) to the account's sweep
	// reserve floor in cents (see sweep_reserve.go): PostSweep moves
	// only the balance in excess of the floor to the target. Opt-in
	// per account and currency; structural config, like dailyLimits —
	// setting or clearing a reserve does not bump the journal version.
	sweepReserves map[accountCurrency]int64
	// transferAllowlist maps a payer account to its installed transfer
	// beneficiary allowlist (see allowlist.go, LG-113): when a payer has
	// an entry here, PostTransfer only books transfers whose payee is
	// in the set. Opt-in per payer account; structural config, like
	// sweepReserves — setting or clearing a list does not bump the
	// journal version, and the lists round-trip through the snapshot
	// config record.
	transferAllowlist map[AccountID]map[AccountID]bool
	// businessCalendar is the ledger's holiday calendar (see
	// businessday.go): weekend days plus the custom holiday table, with
	// a version counter bumped on every change. Structural config, like
	// dailyLimits — calendar changes do not bump the journal version and
	// survive disaster-recovery snapshots.
	businessCalendar BusinessDayCalendar
	// accrualBusinessDaysOnly restricts interest and fee accruals to
	// business days when true (see SetAccrualBusinessDaysOnly): a
	// non-business-day asof is rejected with ErrNotBusinessDay. Off by
	// default. Structural config, like businessCalendar.
	accrualBusinessDaysOnly bool
	// largeTxThresholds maps an ISO 4217 currency to the daily
	// large-transaction reporting threshold in that currency's cents (see
	// ctr.go): an account's UTC-day inflow or outflow in that currency
	// reaching the threshold generates a report on the next
	// GenerateLargeTxReports call. Opt-in; structural config, like
	// dailyLimits — threshold changes do not bump the journal version
	// and survive disaster-recovery snapshots.
	largeTxThresholds map[string]int64
	// largeTxReportCurrency is the ISO currency converted report amounts
	// are disclosed in (default USD, see SetLargeTxReportCurrency).
	// Structural config, like largeTxThresholds.
	largeTxReportCurrency string
	// largeTxReports stores generated large-transaction reports keyed by
	// (account, currency, day, direction): regenerating a day replays the
	// stored rows instead of double-reporting into the audit chain.
	// Pruned to the last two UTC days on each generation (see
	// maybePruneLargeTxReportsLocked).
	largeTxReports   map[largeTxReportKey]LargeTxReport
	lastLargeTxPrune time.Time
	// budgets maps (account, currency) to the account's monthly spend
	// budget in cents, and monthlySpend accumulates the spend booked per
	// (account, currency, UTC month). A posting that would take the
	// month's cumulative spend above the budget is rejected with
	// ErrBudgetExceeded (see budget.go). Opt-in per account and
	// currency; structural config, like dailyLimits — setting or
	// clearing a budget does not bump the journal version.
	budgets      map[budgetKey]int64
	monthlySpend map[monthlySpendKey]int64
	// budgetPaceAlertRatio is the opt-in configurable multiplier for the
	// budget pace alert (LG-116, see budget_pace.go): a pace alert fires
	// when a budget's projected month-end spend exceeds budget * ratio.
	// Defaults to 1.0 (alert when the trajectory overshoots the budget);
	// SetBudgetPaceAlertRatio adjusts it for earlier or later warning.
	budgetPaceAlertRatio float64
	// budgetPaceAlerted marks the (account, currency, month) buckets
	// whose pace alert already fired: the alert is a once-per-month
	// transition, not a per-call event. Operational state, pruned with
	// the monthly-spend buckets — not books, so it does not round-trip
	// through snapshots.
	budgetPaceAlerted map[monthlySpendKey]bool
	// budgetPaceAlerts counts fired budget pace alerts: operational
	// metric, surfaced on GET /metrics as
	// ledger_budget_pace_alerts_total; not books, so it does not
	// round-trip through snapshots.
	budgetPaceAlerts atomic.Uint64
	// balanceWatches maps (account, currency) to the account's balance
	// threshold watch (see balance_watch.go): an opt-in low and/or high
	// alert level in cents. balanceWatchBreached marks the (account,
	// currency, direction) pairs currently in breach (silenced until the
	// balance recovers inside the band). A posting that crosses a level
	// emits one balance_threshold_breached audit event and bumps
	// balanceWatchBreachCount. Advisory only — never a rejection.
	// Opt-in per account and currency; structural config, like
	// dailyLimits.
	balanceWatches       map[balanceWatchKey]BalanceWatch
	balanceWatchBreached map[balanceWatchKey]watchBreachState
	// liquidityThresholds maps (account, currency) to the account's
	// low-liquidity alert floor for the day's net position (see
	// liquidity.go): an opt-in threshold in cents. liquidityBreached
	// marks the (account, currency) pairs currently in breach (silenced
	// until the net position recovers at or above the floor). An
	// evaluation that finds a position below the floor emits one
	// low_liquidity_alert audit event and bumps liquidityAlertCount.
	// Advisory only — never a rejection. Opt-in per account and
	// currency; structural config, like balanceWatches.
	liquidityThresholds map[liquidityThresholdKey]LiquidityAlertThreshold
	liquidityBreached   map[liquidityThresholdKey]bool
	liquidityAlertCount uint64
	// tenantID is the tenant namespace this ledger belongs to (see
	// tenant.go): the empty string is the default tenant. It is stamped
	// onto every audit-log and outbox event the ledger emits, so the
	// process-wide compliance streams stay attributable per tenant.
	// Set by TenantStore.Adopt/For; never by hand.
	tenantID TenantID
	// settlementChannels maps an account to its settlement channel (the
	// payment rail the merchant settles through, e.g. "alipay"), the
	// channel dimension of the settlement view (see settlement.go).
	// Structural config, like frozen and dailyLimits: setting or
	// clearing a mapping does not bump the ledger version and survives
	// disaster-recovery snapshots.
	settlementChannels map[AccountID]string
	// settlementCutoffs maps a currency code to its settlement cutoff
	// window (see settlement_cutoff.go): transfers posted after the
	// cutoff wait for the next window instead of settling in the
	// current one. Structural config, like settlementChannels: setting
	// or clearing a cutoff does not bump the ledger version and survives
	// disaster-recovery snapshots. Currencies with no row behave exactly
	// as before cutoffs existed.
	settlementCutoffs map[string]SettlementCutoff
	// settlementDeferrals tracks transfers booked after their currency's
	// cutoff, awaiting the next settlement window (see
	// settlement_cutoff.go): scheduled -> pending -> settled.
	// Settlement bookkeeping, not accounting: deferral transitions never
	// bump the journal version, and the records round-trip through
	// snapshots so a restore keeps the in-flight settlement trail.
	settlementDeferrals map[string]*SettlementDeferral
	// settlementDeferred counts transfers deferred to the next settlement
	// window since the ledger started (see settlement_cutoff.go).
	// Operational metric, surfaced on GET /metrics as
	// ledger_settlement_deferred_total; not books, so it does not
	// round-trip through snapshots (the audit log's
	// "transfer_settlement_deferred" events are the durable record).
	settlementDeferred atomic.Uint64
	// balanceWatchBreachCount counts balance-threshold breach alerts the
	// ledger has emitted since it started (see balance_watch.go). The
	// counter resets on restart — alert history lives in the structured
	// audit log (op "balance_threshold_breached"), which is the durable
	// record. In-memory only: it does not round-trip through snapshots.
	balanceWatchBreachCount uint64
	// parents maps a child account to its parent account in the
	// sub-account hierarchy (see hierarchy.go). Only accounts with an
	// assigned parent appear here; clearing the parent deletes the row.
	// SetParent keeps the map cycle-free, so Rollup's subtree walk always
	// terminates.
	parents map[AccountID]AccountID
	// feeTiers / feeRevenueAccount configure the default transfer fee
	// policy (see WithTransferFeeSchedule and WithTransferFeePolicy in
	// transfer.go): unless a transfer carries an explicit fee or sets
	// SkipFee, PostTransfer books a fee leg of floor(amount * tierRateBps
	// / 10000) cents to feeRevenueAccount, where the tier is the last one
	// whose minimum does not exceed the transfer amount. Empty tiers (or
	// an empty revenue account) disable the policy.
	feeTiers          []FeeTier
	feeRevenueAccount AccountID
	// interestTiers is the opt-in tiered deposit-rate table for
	// AccrueInterestTiered (see interest_tiers.go): balance bands earn
	// different annual rates instead of one flat rateBps. A nil table
	// means tiered accrual is off and AccrueInterestTiered is rejected.
	// Structural config — it survives snapshots.
	interestTiers []InterestTier
	// maxTransferChunkCents is the opt-in single-transfer cap for
	// PostTransferSplit (see WithMaxTransferChunkCents in
	// transfer_split.go): a transfer whose amount exceeds it is split
	// into chunks of at most this size, committed atomically. Zero
	// disables chunking. Structural config, like feeTiers — it survives
	// snapshots.
	maxTransferChunkCents int64
	// splitFeePerChunk makes PostTransferSplit resolve the transfer fee
	// policy per chunk instead of once per transfer (see
	// WithSplitFeePerChunk in transfer_split.go). An explicit FeeCents
	// is always charged once per transfer. Structural config — it
	// survives snapshots.
	splitFeePerChunk bool
	// fxRates is the directional FX rate table (see fx.go): converting
	// fromCurrency to toCurrency multiplies by Num/Den and floors to
	// whole cents. fxAccount is the ledger-wide FX clearing account, the
	// counterparty of cross-currency transfer legs. Both are structural
	// config — like frozen and feeTiers — and survive snapshots.
	fxRates   map[fxPair]ExchangeRate
	fxAccount AccountID
	// fxIncomeAccount is the ledger-wide FX spread-income account (see
	// WithFXIncomeAccount, fx_spread.go): the credit counterparty of
	// cross-currency spread legs. Structural config — it survives
	// snapshots, like fxAccount.
	fxIncomeAccount AccountID
	// fxSnapshotVersion counts FX rate-table mutations (LG-45): every
	// SetFXRate / SetFXRateRat / RemoveFXRate bumps it and persists a
	// full copy of the table in fxRateSnapshots at the new version, so a
	// historical audit can reproduce exactly the table that converted
	// any FX transfer (see fx_snapshot.go). 0 means the table was never
	// populated.
	fxSnapshotVersion uint64
	fxRateSnapshots   map[uint64]FXRateTableSnapshot
	// forwards is the FX forward-contract registry (LG-110, see
	// forward.go), keyed by forward ID (the booking idempotency key).
	// Structural state — like the rate table — and it survives
	// snapshots.
	forwards map[string]*FXForward
	// forwardRollover is the opt-in auto-rollover config for FX
	// forward contracts (LG-117, see forward_roll.go): nil means
	// disabled. Structural config — like the rate table — setting or
	// clearing it does not bump the journal version.
	forwardRollover *ForwardRolloverConfig
	// ftpCurves is the per-currency FTP rate-curve registry (see
	// ftp.go), keyed by normalized currency code. Each SetFtpCurve
	// bumps the currency's version. ftpAccounts names the internal
	// counterparties of every charge. Both are structural config —
	// they survive snapshots, like the FX accounts.
	ftpCurves   map[string]*FtpCurve
	ftpAccounts FtpAccounts
	// rateOverrides is the registry of manual FX rate overrides (see
	// fx_override.go), keyed by directional pair: a pending override
	// awaits a second operator's approval (maker-checker); an approved
	// one is active until its ExpiresAt and prices the pair's FX
	// conversions instead of the market rate. Overrides are
	// operational state like transfer reviews — they do not round-trip
	// through snapshots. rateOverrideTTL is the default override
	// lifetime applied when SetRateOverride is called with a
	// non-positive ttl; rateOverrideMaxDeviationBps caps
	// |override-market|/market in basis points, enforced at proposal
	// and re-checked at approval.
	rateOverrides               map[fxPair]*RateOverride
	rateOverrideTTL             time.Duration
	rateOverrideMaxDeviationBps int64
	// fxOverrideConversions counts cross-currency transfers converted
	// at an approved manual override price (fx_rate_overridden audit
	// events). In-memory only — it does not round-trip through
	// snapshots.
	fxOverrideConversions uint64
	// fxSpreadBookings counts journaled FX spread legs (see
	// fx_spread.go) and fxSpreadCentsTotal their total spread cents,
	// since process start. Operational counters surfaced on GET
	// /metrics as ledger_fx_spread_bookings_total and
	// ledger_fx_spread_cents_total. In-memory only — they do not
	// round-trip through snapshots, like fxOverrideConversions.
	fxSpreadBookings   uint64
	fxSpreadCentsTotal int64
	// transferKeys maps a transfer's idempotency key to the IDs of the
	// journal entries it posted, in commit order (principal, then the fee
	// leg when one was booked), so a replayed transfer returns its full
	// receipt. Keys expire with the TTL like the entry-level index —
	// except keys of still-pending reviews (see pendingReviewByKeyLocked):
	// dropping a pending review's replay index would turn a later replay
	// into a transfer-ID conflict instead of the review receipt.
	transferKeys map[string][]string
	// reviews is the registry of large-transfer dual-control reviews
	// (see review.go), keyed by transfer ID: every transfer routed
	// into pending review registers here, whether or not it carried an
	// idempotency key. reviewThresholds maps an account to its
	// per-account review threshold in cents (the payer side; see
	// SetReviewThreshold), reviewThreshold is the ledger-wide threshold
	// in cents (0 = disabled), and reviewExpiry is the opt-in pending
	// review lifetime (0 = reviews never lapse on their own). All four
	// are structural config and survive snapshots.
	reviews          map[string]*TransferReview
	reviewThresholds map[AccountID]int64
	reviewThreshold  int64
	reviewExpiry     time.Duration
	// reviewQuorum is the opt-in N-of-M multi-approver quorum policy
	// for dual-control reviews (see review_quorum.go): nil disables it,
	// leaving the LG-42 single-operator decision path. Like the review
	// thresholds it is structural config — it does not bump the ledger
	// version — and it survives snapshots.
	reviewQuorum *QuorumPolicy
	// approvalChain is the opt-in sequential approval-chain policy
	// for dual-control reviews (see review_chain.go): nil disables it.
	// While configured it supersedes the flat quorum policy above — a
	// review is decided by one voting scheme at a time. Structural
	// config like the quorum policy: no version bump, survives
	// snapshots.
	approvalChain *ApprovalChain
	// reviewDelegations is the registry of review vote delegations
	// (see review_delegation.go), keyed by (scope, reviewID,
	// principal, delegatee): a principal's standing permission for a
	// delegatee to cast votes in the principal's place, on the flat
	// review path, the quorum path, or the chain path. Structural
	// config like the quorum policy: no version bump, survives
	// snapshots. reviewDelegationsGranted counts delegations ever
	// granted: the ledger-side source of the
	// ledger_review_delegations_total metric (monotonic — extensions
	// and revocations do not move it).
	reviewDelegations        map[delegationKey]*ReviewDelegation
	reviewDelegationsGranted uint64
	// multisigApprovers maps approver IDs to registered Ed25519 public
	// keys, multisigPolicies maps operation types to their N-of-M
	// policies, and multisigOps is the pending_multisig queue keyed by
	// operation ID (see multisig.go). Policies and the approver registry
	// are structural config — no version bump, survive snapshots. The
	// counters are the ledger-side sources of the ledger_multisig_*
	// metrics (monotonic).
	multisigApprovers map[string]ed25519.PublicKey
	multisigPolicies  map[string]MultisigPolicy
	multisigOps       map[string]*MultisigOperation
	multisigSubmitted atomic.Uint64
	multisigApproved  atomic.Uint64
	multisigRejected  atomic.Uint64
	multisigExpired   atomic.Uint64
	// holds maps hold IDs to authorization holds (see hold.go). Holds are
	// off-journal reservations: they never appear in entries, the audit
	// chain, or the version counter. holdsByAccount indexes them per
	// account so Available only scans that account's holds.
	holds          map[string]Hold
	holdsByAccount map[AccountID][]string
	// holdKeys maps a hold's idempotency key to its hold ID, and
	// captureKeys maps a capture's idempotency key to its receipt. The
	// two namespaces are independent of each other and of the
	// entry/transfer key namespaces; all of them expire with the TTL.
	// partialCaptureKeys is the same shape for partial captures (see
	// hold_partial.go): the key is the deterministic "<holdID>/capture/
	// <seq>" entry ID, the (holdID, captureSeq) identity.
	holdKeys           map[string]string
	captureKeys        map[string]CaptureReceipt
	partialCaptureKeys map[string]PartialCaptureReceipt
	// termDeposits maps term-deposit IDs to their records (see
	// termdeposit.go): time-bound principal locks that reserve available
	// funds off-journal, like authorization holds. termDepositKeys maps
	// a deposit open's idempotency key to its deposit ID — its own
	// namespace, expiring with the TTL like every other key index (see
	// pruneTermDepositKeysLocked). tdInterestExpense is the
	// ledger-wide interest-expense account term-deposit interest
	// accruals credit (see SetTermDepositInterestExpense): empty means
	// unconfigured and accruals are fail-closed. Structural config and
	// operational state like holds — both round-trip through
	// disaster-recovery snapshots.
	termDeposits      map[string]TermDeposit
	termDepositKeys   map[string]string
	tdInterestExpense AccountID
	// payouts maps payout instruction IDs to their records (see
	// payout.go): the operator lifecycle of money leaving the ledger.
	// payoutKeys maps a payout open's idempotency key to its payout ID
	// — its own namespace, expiring with the TTL like every other key
	// index (see prunePayoutKeysLocked). payoutClearingAccount is the
	// ledger-wide clearing account payout submit legs credit (see
	// SetPayoutClearingAccount): empty means unconfigured and submits
	// are fail-closed. The clearing account is structural config and
	// round-trips through disaster-recovery snapshots; the instructions
	// themselves are in-memory only, like clearing batches.
	payouts               map[string]PayoutInstruction
	payoutKeys            map[string]string
	payoutClearingAccount AccountID
	// creditLines maps (account, currency) to the granted credit line
	// (see creditline.go): an opt-in per-account overdraft floor of
	// -limit with daily interest on utilization. creditInterestIncome
	// is the ledger-wide interest-income account credit-line interest
	// accruals debit (see SetCreditInterestIncome): empty means
	// unconfigured and accruals are fail-closed. Structural config,
	// like noOverdraft — it never bumps the journal version and it
	// survives disaster-recovery snapshots.
	creditLines          map[creditLineKey]CreditLine
	creditInterestIncome AccountID
	// eclStageConfig is the IFRS 9 staging configuration (see ecl.go):
	// the DPD buckets and per-stage provision rates. eclExposures
	// tracks the per-(account, currency) exposure state: manually
	// registered exposures and DPD tracking for credit-line-sourced
	// exposures. eclExpenseAccount / eclAllowanceAccount are the
	// fail-closed P&L/allowance accounts ECL provisions book through —
	// empty means unconfigured and BookECLProvision refuses.
	// Structural config, like creditLines: it never bumps the journal
	// version and it survives disaster-recovery snapshots.
	// eclProvisionsCommitted counts ECL provision journal entries ever
	// committed (idempotent replays are not counted);
	// eclStageMigrations counts stage migrations recorded (the durable
	// record is the ecl_stage_migration audit events). Both are
	// in-memory only — they do not round-trip through snapshots.
	eclStageConfig         ECLStageConfig
	eclExposures           map[eclExposureKey]eclExposureRow
	eclExpenseAccount      AccountID
	eclAllowanceAccount    AccountID
	eclProvisionsCommitted atomic.Uint64
	eclStageMigrations     atomic.Uint64
	// sweepKeys maps a sweep's idempotency key to its sweepRecord, so
	// replays rebuild the full receipt (see sweep.go). Its own namespace,
	// like holdKeys/captureKeys, expiring with the TTL.
	sweepKeys map[string]sweepRecord
	// batchKeys maps a batch's idempotency key to its batchRecord, so
	// replays rebuild the full receipt (see batch.go). Its own namespace,
	// expiring with the TTL like every other key index (see
	// pruneBatchKeysLocked in batch.go).
	batchKeys map[string]batchRecord
	// allocRules is the registry of cost-allocation rules (see
	// allocation.go), keyed by rule ID. allocRuleVersion is the
	// rule-set mutation counter, bumped on every set/delete so
	// snapshots carry the generation. allocKeys maps an allocation's
	// deterministic idempotency key ("alloc/<ruleID>/<YYYY-MM>") to its
	// allocRecord, so replays rebuild the exact original receipt. Its
	// own namespace, expiring with the TTL like every other key index
	// (see pruneAllocKeysLocked in allocation.go).
	allocRules       map[string]AllocationRule
	allocRuleVersion uint64
	allocKeys        map[string]allocRecord
	// allocationsCommitted counts cost allocations ever committed (see
	// allocation.go); idempotent replays are not counted.
	allocationsCommitted atomic.Uint64
	// nettingKeys maps a netting session's deterministic key
	// ("netting:<sessionID>") to its nettingRecord, so reposts rebuild
	// the exact original report (see netting.go). Its own namespace,
	// expiring with the TTL like every other key index (see
	// pruneNettingKeysLocked). In-memory only: unlike the other key
	// namespaces it does not round-trip through disaster-recovery
	// snapshots — the journaled settlement entries do, so the money is
	// safe; only the replay shortcut is lost.
	nettingKeys map[string]nettingRecord
	// installments maps installment-plan IDs to their records (see
	// installment.go): scheduled receivables collected in equal
	// periods. installmentKeys maps a plan open's idempotency key to
	// its installmentKeyRecord — its own namespace, expiring with the
	// TTL like every other key index (see
	// pruneInstallmentKeysLocked). In-memory only: neither round-trips
	// through disaster-recovery snapshots (see the installment.go
	// package comment); the journaled period rows do.
	installments    map[string]InstallmentPlan
	installmentKeys map[string]installmentKeyRecord
	// clearingBatches maps clearing-batch IDs to their records (see
	// clearing.go): off-book transfer intents settled atomically in
	// net form. clearingKeys maps a batch open's idempotency key to
	// its clearingKeyRecord — its own namespace, expiring with the
	// TTL like every other key index (see
	// pruneClearingKeysLocked). In-memory only: neither round-trips
	// through disaster-recovery snapshots (see the clearing.go
	// package comment); the journaled net rows do.
	clearingBatches map[string]ClearingBatch
	clearingKeys    map[string]clearingKeyRecord
	// merges is the authoritative registry of committed account merges
	// (see merge.go), keyed by merge ID: every PostMerge appends here,
	// whether or not it carried an idempotency key. It feeds Reconcile's
	// merge history and idempotent replays (via mergeKeys).
	merges map[string]mergeRecord
	// mergeKeys maps a merge's idempotency key to its merge ID. Its own
	// namespace, expiring with the TTL like every other key index (see
	// pruneMergeKeysLocked in merge.go).
	mergeKeys map[string]string
	// splits is the authoritative registry of committed account splits
	// (see split.go), keyed by split ID: every SplitAccount appends
	// here, whether or not it carried an idempotency key. It feeds
	// Reconcile's split history and idempotent replays (via splitKeys).
	splits map[string]splitRecord
	// splitKeys maps a split's idempotency key to its split ID. Its own
	// namespace, expiring with the TTL like every other key index (see
	// pruneSplitKeysLocked in split.go).
	splitKeys map[string]string
	// schedules is the registry of recurring transfer plans (see
	// schedule.go), keyed by schedule ID. scheduleKeys maps a schedule
	// creation's idempotency key to its schedule ID — its own namespace,
	// like mergeKeys. scheduleRuns holds each schedule's bounded run
	// history (newest appended; evicted past maxScheduleRuns). All three
	// are structural state and survive snapshots; the run history does
	// not. scheduleFiring marks schedules with a sweep in flight so
	// concurrent SweepDue calls serialize per schedule.
	schedules      map[string]*TransferSchedule
	scheduleKeys   map[string]string
	scheduleRuns   map[string][]ScheduleRun
	scheduleFiring map[string]bool
	// reportSchedules is the registry of recurring report subscriptions
	// (see report_schedule.go), keyed by schedule ID, with its own
	// idempotency-key namespace (reportScheduleKeys), bounded run
	// history (reportScheduleRuns, evicted past maxReportRuns), and
	// per-schedule firing flags (reportScheduleFiring) so concurrent
	// SweepReportsDue calls serialize per schedule. Report schedules
	// are process-lifetime operational state and do not survive
	// snapshots — unlike transfer schedules.
	reportSchedules      map[string]*ReportSchedule
	reportScheduleKeys   map[string]string
	reportScheduleRuns   map[string][]ReportRun
	reportScheduleFiring map[string]bool
	// audit is the structured compliance audit log (see audit.go). Nil
	// means disabled: operations skip event construction entirely and
	// reads never touch it, so the read path is unaffected.
	audit *AuditLog
	// apiKeys is the issued API credential registry (see apikey.go,
	// LG-130): key ID -> key. Only SHA-256 hashes of secrets are
	// stored; the secrets themselves are shown once at issuance and
	// are unrecoverable afterwards. Structural config: survives
	// snapshots (hashes only).
	apiKeys map[string]*APIKey
	// apiKeyAuth flips HTTP-layer API key enforcement on (see
	// WithAPIKeyAuth). Off by default: the middleware is a
	// pass-through and the anonymous-operator flow is unchanged.
	apiKeyAuth bool
	// holderNames is the PII registry of account holder names (see
	// pii.go, LG-131). Names are kept as bytes so erasure can
	// overwrite the backing store before the record is dropped.
	holderNames map[AccountID]*holderNameRecord
	// erasureRequests tracks PII erasure maker-checker requests by ID;
	// erasedMemos marks journal rows whose memo is redacted on read
	// (the sealed rows are never rewritten); erasureRecords are the
	// tombstones in execution order; erasureVersion is the monotonic
	// counter annotated on export manifests (LG-128).
	erasureRequests map[string]*ErasureRequest
	erasedMemos     map[string]ErasureRecord
	erasureRecords  []ErasureRecord
	erasureVersion  uint64
	// outbox is the domain-event outbox (see outbox.go). Nil means
	// disabled: state changes skip event construction entirely and the
	// read path is unaffected. The dry-run twin (see dryrun.go) always
	// carries a nil outbox, so simulations emit no domain events.
	outbox *Outbox
	// webhooks is the outbound webhook dispatcher (see webhook.go, LG-102).
	// Nil means disabled: emitOutboxLocked skips the fan-out entirely.
	// It is a pure subscriber of the outbox stream — Notify runs after
	// the event is durably emitted, never blocks, and never fails the
	// posting.
	webhooks *WebhookDispatcher
	// closedPeriods is the set of locked accounting periods ("2006-01",
	// UTC months; see period.go). Journal entries whose timestamp falls
	// in a closed period are rejected with ErrPeriodClosed by every
	// journal-writing operation, so a closed month's books cannot change.
	// Structural config like frozen — it survives snapshots and is
	// listed in Reconcile.
	closedPeriods map[string]bool
	// closingPL and closingRetained configure the opt-in period-closing
	// mapping (see closing.go): the P&L accounts zeroed at period close
	// and the retained-earnings account that receives them. An empty
	// closingRetained disables closing. Structural config like frozen —
	// it survives snapshots.
	closingPL       map[AccountID]bool
	closingRetained AccountID
	// dunningConfigs maps a (account, currency) pair to its opt-in
	// collections escalation policy (see dunning.go); dunningStates
	// carries the current escalation level per pair. dunningEscalations
	// counts every level escalation (the dunning_escalated audit
	// events fired). Configs and states are structural config and
	// survive snapshots; the counter is in-memory observability.
	dunningConfigs     map[dunningKey]DunningConfig
	dunningStates      map[dunningKey]DunningState
	dunningEscalations uint64
	// disputes maps dispute IDs to transfer-dispute records (see
	// dispute.go): payer-initiated chargeback-style claims that freeze
	// the disputed amount on the payee's available balance through an
	// authorization hold and resolve terminally to upheld (the payer is
	// refunded) or rejected (the payee keeps the funds). disputeKeys
	// maps a dispute raise's idempotency key to its dispute ID — its
	// own namespace, expiring with the TTL like every other key index
	// (see pruneDisputeKeysLocked in dispute.go). Dispute records are
	// operational state that does not round-trip through
	// disaster-recovery snapshots: after a restore, an open dispute's
	// freeze hold is still present (holds do round-trip, so the funds
	// stay conservatively frozen) but the dispute record itself must be
	// re-derived from the dispute_raised / dispute_resolved audit
	// events — the structured audit trail is the durable record.
	disputes    map[string]Dispute
	disputeKeys map[string]string
	// healthViolations counts balance-health violations found by the
	// background watchdog (see health_watch.go): the number of
	// "balance_health_violation" audit events emitted. In-memory only —
	// it does not round-trip through snapshots. healthCursor is the
	// atomic rotation cursor of the equation spot check: observability
	// state, not books.
	healthViolations uint64
	healthCursor     atomic.Uint64
	// fxRateStaleAlerts / fxRateExpiredAlerts count the edge-triggered
	// "fx_rate_stale" / "fx_rate_expired" audit events from the
	// background FX-rate watch (see fx_watch.go). In-memory only — they
	// do not round-trip through snapshots.
	fxRateStaleAlerts   uint64
	fxRateExpiredAlerts uint64
	// fxRateInconsistentAlerts counts the edge-triggered
	// "fx_rate_inconsistent" audit events from the background
	// FX-consistency watch (see fx_consistency.go, LG-76). In-memory
	// only — they do not round-trip through snapshots.
	fxRateInconsistentAlerts uint64
	// fxRateProvider is the opt-in market FX rate adapter (see
	// fx_provider.go, LG-74): the FX-rate watch polls it on its
	// interval and installs quotes through the SetFXRateRat write
	// path. Nil — the default — disables provider refreshes and the
	// watcher keeps its static-table behavior unchanged.
	// fxRateRefreshes / fxRateMoves / fxRateProviderFails count
	// provider quote installs, price moves (fx_rate_moved), and fetch
	// failures (fx_rate_provider_failed). In-memory only — they do not
	// round-trip through snapshots.
	fxRateProvider      FXRateProvider
	fxRateRefreshes     uint64
	fxRateMoves         uint64
	fxRateProviderFails uint64
	// balanceHook is the opt-in balance-change notification callback
	// (see balance_hook.go): every committed Post/Transfer/Capture/
	// Sweep/Merge dispatches one BalanceChange per touched (account,
	// currency) asynchronously, with panic isolation and a configurable
	// per-callback timeout. Nil disables notifications entirely (the
	// default): the commit path then behaves exactly as before, with no
	// extra allocation.
	balanceHook        BalanceChangeHook
	balanceHookTimeout time.Duration // per-callback timeout; <= 0 means defaultBalanceHookTimeout
	// journalRules maps rule IDs to opt-in automatic journaling rules
	// (see rules.go): on Post, every enabled rule whose match predicate
	// fits the principal entry appends its derived legs atomically.
	// Empty by default — rules are opt-in, and a ledger without rules
	// posts exactly as before. Rule changes are structural config, like
	// frozen and dailyLimits: they do not bump the journal version and
	// they survive disaster-recovery snapshots.
	journalRules map[string]JournalRule
	// journalRuleVersion counts journal-rule mutations: every
	// SetJournalRule and DeleteJournalRule bumps it. The counter
	// round-trips through the snapshot config record, so a restored
	// ledger carries the same rule-set generation as the source.
	journalRuleVersion uint64
	// fixedAssets maps asset IDs to the registered fixed-asset master
	// records (see depreciation.go): cost, salvage value, useful life
	// and depreciation method. Registration is structural config — it
	// does not bump the journal version. The register is in-memory;
	// re-register assets after a disaster-recovery restore (the booked
	// depreciation entries themselves round-trip as ordinary journal
	// rows, so accumulated depreciation survives).
	fixedAssets map[string]FixedAsset
	// ruleLegsCommitted counts derived journal legs ever committed by
	// the rules engine. Operational metric (surfaced on GET /metrics),
	// not books: it is in-memory only and does not round-trip through
	// snapshots.
	ruleLegsCommitted atomic.Uint64
	// postingRules is the installed automatic posting rules for
	// transfer/capture/sweep events (see posting_rules.go), in
	// registration order: rules are deployment config set at
	// construction with WithPostingRules, validated fail-fast. Empty
	// by default — a ledger without posting rules posts exactly as
	// before. Rule installation is structural config, like frozen and
	// dailyLimits: it does not bump the journal version and it
	// survives disaster-recovery snapshots.
	postingRules []PostingRule
	// postingRuleLegs counts derived posting-rule legs ever committed,
	// per rule name: the source of truth for
	// ledger_posting_rule_legs_total{rule} on GET /metrics.
	// Operational metric, in-memory only, not in snapshots.
	postingRuleLegs map[string]uint64
	// vanAttributions counts virtual-account attributions ever applied
	// (see virtual_account.go). Operational metric (surfaced on GET
	// /metrics), not books: in-memory only, no snapshot round-trip.
	vanAttributions atomic.Uint64
	// virtualAccounts maps a virtual account number to its parent
	// account (see RegisterVirtualAccount in virtual_account.go):
	// receipts naming the VAN attribute to the parent. Structural
	// config like the account tags — it round-trips through snapshots,
	// because a restore that dropped it would silently stop
	// attributing VAN receipts to parents.
	virtualAccounts map[AccountID]AccountID
	// virtualAccountVersion is the VAN mapping mutation counter (see
	// VirtualAccountVersion): every registration change bumps it. It
	// round-trips through the snapshot config record, so a restored
	// ledger carries the same mapping generation as the source.
	virtualAccountVersion uint64
	// revalAccount is the P&L account that receives the gain/loss legs
	// of FX revaluations (see RevalueBalances in revaluation.go). Empty
	// means revaluation is not configured: RevalueBalances refuses with
	// ErrRevaluationAccountNotConfigured (fail-closed). Structural
	// config, like feeRevenueAccount — it survives snapshots.
	revalAccount AccountID
	// revalBaselines tracks the last remeasured carrying value per
	// (account, foreign currency, base currency) (see revaluation.go):
	// the foreign balance and its base-currency value at the last
	// revaluation, plus the rate that produced it. The next revaluation
	// books only the delta against this baseline, so repeated runs with
	// an unchanged book and rate table book nothing. Structural state,
	// like the idempotency indexes — it survives snapshots, because a
	// restore that dropped it would re-book full carrying values as
	// gains.
	revalBaselines map[revalKey]revalBaseline
	// ofxImports is the OFX statement-import registry (see ofx_import.go):
	// document SHA-256 (hex) -> import report, for duplicate-import
	// idempotency. Structural state like the idempotency indexes — it
	// survives snapshots, because a restore that dropped it would
	// re-import the same statement as new.
	ofxImports map[string]*OFXImportReport
	// camt053Imports is the ISO 20022 camt.053 statement import registry
	// (see camt053.go): document SHA-256 (hex) -> import report, for
	// duplicate-import idempotency. Structural state like the OFX
	// index — it survives snapshots, because a restore that dropped it
	// would re-import the same statement as new.
	camt053Imports map[string]*CAMT053ImportReport
	// statementMatches is the statement-line match registry (see
	// statement_match.go, LG-107): statement line ID
	// ("ofx/<FITID>" / "camt053/<stmtID>/<seq>", see DraftEntry.ID) ->
	// match record. A line matched once replays its recorded entry on
	// re-import — the same statement file can never double-match — and
	// Reconcile surfaces still-unmatched lines as
	// TrialBalanceDiscrepancy rows. Operational index, not books: it
	// does not round-trip through snapshots (like freezeEvents); the
	// structured audit log ("statement_match" events) remains the
	// durable record.
	statementMatches map[string]*statementMatchRecord
	// painTransfers is the ISO 20022 pain.001 export registry (see
	// pain001.go): transfer ID -> transfer principal for every
	// same-currency transfer (plain and split chunks), in booking order
	// via (CreatedAt, TransferID) sort. Structural state like the
	// idempotency indexes — it survives snapshots, because a restore
	// that dropped it would silently un-export pre-restore transfers.
	painTransfers map[string]PainTransfer
	// painInstructions is the pain.001 payment-instruction lifecycle
	// registry (see pain001_status.go): deterministic export MsgId ->
	// the instruction's lifecycle record. Structural state like the
	// idempotency indexes — it survives snapshots, because a restore
	// that dropped it would lose every instruction's receipt/settlement
	// position and the audit trail's from/to chain would restart.
	painInstructions map[string]*PainInstruction
	// accountIBANs is the opt-in IBAN registry (see SetAccountIBAN in
	// pain001.go): the debtor/creditor identifiers pain.001 payment
	// instructions name. Structural config like the account tags — it
	// round-trips through snapshots.
	accountIBANs map[AccountID]AccountIBAN
	// externalMappings is the external account mapping (see
	// external_mapping.go, LG-122): (account, system, externalID) ->
	// the mapping row. One internal account maps to many external
	// identifiers — a core-banking account number, a PSP merchant
	// number — each tagged with the external system that knows the
	// account by it. Structural config like accountIBANs: it does not
	// bump the journal version and it round-trips through snapshots.
	externalMappings map[externalMappingKey]ExternalAccountMapping
	// externalMappingChanges counts external account mapping changes
	// (see SetExternalMapping/RemoveExternalMapping in
	// external_mapping.go): rows declared or updated plus rows removed.
	// Operational metric, surfaced on GET /metrics as
	// ledger_external_mapping_changes_total; not books, so it does not
	// round-trip through snapshots.
	externalMappingChanges atomic.Uint64
	// entryLinks is the declared entry-link registry (see links.go,
	// LG-123): explicit directed associations between journal entries —
	// reversal_of, adjustment_of, fee_of, refund_of — in declaration
	// order. Append-only: links are never modified or removed. Each
	// declaration appends to the tamper-evident link chain below, which
	// VerifyChain recomputes. Structural state like externalMappings:
	// declarations do not bump the journal version, and the registry
	// round-trips through snapshots.
	entryLinks []EntryLink
	// linkChain is the tamper-evident hash chain over the entry links:
	// linkChain[i] covers entryLinks[i] (see hashEntryLinkChainLink).
	// The chain has its own genesis, independent of the entry chain —
	// link declarations never disturb the entry chain's
	// links == version invariant.
	linkChain []entryLinkChainLink
	// entryLinkChanges counts declared entry links (see
	// DeclareEntryLink in links.go): new declarations only —
	// idempotent replays of an identical link are not counted.
	// Operational metric, surfaced on GET /metrics as
	// ledger_entry_links_total; not books, so it does not round-trip
	// through snapshots.
	entryLinkChanges atomic.Uint64
	// mandates is the direct-debit mandate registry (see mandate.go,
	// LG-125): the standing payer->payee pull authorizations, by
	// mandate ID. Mandates are append-only: they are created active and
	// move to suspended/cancelled/expired, never deleted. Structural
	// state like entryLinks: changes do not bump the journal version,
	// and the registry round-trips through snapshots.
	mandates map[string]Mandate
	// mandateDebits is the mandate -> debit-entry index: mandate ID to
	// the IDs of the journal entries debited under it, in debit order.
	// Append-only, like the mandates above; it backs GET
	// /mandates/{id}/debits and the frequency-window gate.
	mandateDebits map[string][]string
	// mandateDebitChanges counts posted mandate debits (see
	// DebitWithMandate in mandate.go): new debits only — idempotent
	// replays of a debit key are not counted. Operational metric,
	// surfaced on GET /metrics as ledger_mandate_debits_total; not
	// books, so it does not round-trip through snapshots.
	mandateDebitChanges atomic.Uint64
	// mandateRetryPolicies is the opt-in automatic-retry policy per
	// mandate (see mandate_retry.go, LG-127): absent means retry
	// disabled. Operational config, like the sweeper intervals — it
	// does not bump the journal version and does not round-trip
	// through snapshots.
	mandateRetryPolicies map[string]MandateRetryPolicy
	// mandateRetries is the pending automatic-retry registry, by debit
	// ID: retries scheduled by failed debits, fired by
	// SweepMandateRetries. mandateRetryFiring serializes concurrent
	// sweeps of the same retry (see SweepDue for the convention).
	// In-memory operational state: a restart drops pending retries —
	// the debits were never booked, so there is nothing to reconcile.
	mandateRetries     map[string]*MandateRetry
	mandateRetryFiring map[string]bool
	chain              []chainLink // audit chain, one link per successful Post, in order
	// chainArchives remembers the chain-segment archives this process
	// exported (LG-136): PruneChainPrefix requires the dropped prefix
	// to be covered by these archives and re-verifies them before
	// dropping. Guarded by mu; in-memory only — a restart drops the
	// registry (see chain_archive.go).
	chainArchives []chainArchiveRecord
	// chainPrunedTo is the highest seq PruneChainPrefix has dropped
	// (LG-136). It lets VerifyChain fail honestly on a pruned chain —
	// including a fully pruned (empty) hot chain — instead of passing
	// on a chain that no longer covers the ledger's history. Guarded
	// by mu; monotonic.
	chainPrunedTo   uint64
	version         uint64        // bumped by every successful Post
	idempotencyTTL  time.Duration // 0 = never expire idempotency keys
	pruneInterval   time.Duration // min gap between lazy key sweeps
	lastKeyPrune    time.Time
	lastDailyPrune  time.Time // last run of the daily-outflow bucket sweep
	lastBudgetPrune time.Time // last run of the monthly-spend bucket sweep
	// anchor holds the external audit-chain anchoring state (LG-44):
	// the configured Ed25519 key and the loaded checkpoints. See
	// anchor.go for the lock-order contract.
	anchor anchorState
	// tsa holds the RFC 3161 timestamp-authority anchoring state
	// (LG-84): the configured TSA client/trust anchor and the loaded
	// timestamp anchors. See tsa.go for the lock-order contract.
	tsa tsaAnchorState
}

// Option configures a Ledger.
type Option func(*Ledger)

// WithIdempotencyTTL sets how long an idempotency key is retained after the
// entry was posted. Keys older than ttl are eligible for eviction via
// ExpireIdempotencyKeys and via lazy pruning on Post. A non-positive ttl
// disables expiry (the default).
func WithIdempotencyTTL(ttl time.Duration) Option {
	return func(l *Ledger) {
		l.idempotencyTTL = ttl
	}
}

// WithAuditLog attaches a structured audit log (see audit.go): every
// mutating operation and every Reconcile run emits one JSONL event after
// it commits. A nil log disables auditing entirely.
func WithAuditLog(al *AuditLog) Option {
	return func(l *Ledger) {
		l.audit = al
	}
}

// emitAudit enqueues one audit event when the audit log is enabled; it is
// a no-op when disabled. Callers must hold l.mu (either lock suffices) —
// the enqueue never blocks, so emitting under the write lock cannot stall
// the operation. The timestamp is taken here so the recorded time matches
// the commit, not the (asynchronous) flush. The ledger's tenant is stamped
// onto the event, so the process-wide compliance stream stays attributable
// per tenant (see tenant.go); the default tenant's events carry no tenant
// field, exactly as before.
func (l *Ledger) emitAudit(ev AuditEvent) {
	if l.audit == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if l.tenantID != DefaultTenantID {
		ev.Tenant = string(l.tenantID)
	}
	l.audit.Log(ev)
}

// AuditStats reports the audit log's written/dropped event counters.
// Enabled is false when no audit log is attached (WithAuditLog was never
// used): in that case the counters are zero and meaningless.
func (l *Ledger) AuditStats() (written, dropped uint64, enabled bool) {
	if l.audit == nil {
		return 0, 0, false
	}
	w, d := l.audit.Stats()
	return w, d, true
}

// AuditRetentionDeletions reports how many audit-log files the retention
// sweep has deleted on this ledger's audit log (LG-137). Enabled is
// false when no audit log is attached. The HTTP layer syncs it into
// ledger_audit_retention_deletions_total on every GET /metrics scrape,
// like the other ledger-owned counters.
func (l *Ledger) AuditRetentionDeletions() (deletions uint64, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.RetentionDeletions(), true
}

// WithOverdraftProtection marks the given accounts as protected from
// overdrafts from the start: any Post that would take a protected credit
// (payer) account's balance below zero is rejected with
// ErrAccountOverdraft. See EnableOverdraftProtection for the per-account
// semantics; the same option can be combined with WithIdempotencyTTL.
func WithOverdraftProtection(accounts ...AccountID) Option {
	return func(l *Ledger) {
		for _, a := range accounts {
			l.noOverdraft[a] = true
		}
	}
}

// New returns an empty Ledger. Options configure behavior; by default
// idempotency keys never expire.
func New(opts ...Option) *Ledger {
	l := &Ledger{
		balances:              make(map[accountCurrency]int64),
		entries:               make(map[string]JournalEntry),
		byKey:                 make(map[string]JournalEntry),
		byRef:                 make(map[string]string),
		byAccount:             make(map[AccountID][]string),
		debitTotals:           make(map[accountCurrency]int64),
		creditTotals:          make(map[accountCurrency]int64),
		frozen:                make(map[AccountID]bool),
		frozenUntil:           make(map[AccountID]time.Time),
		closed:                make(map[AccountID]AccountClosure),
		suspended:             make(map[AccountID]AccountSuspension),
		dormant:               make(map[AccountID]DormancyRecord),
		chart:                 make(map[AccountID]AccountType),
		cashAccounts:          make(map[AccountID]bool),
		cashAccountCurrencies: make(map[accountCurrency]bool),
		tags:                  make(map[AccountID]map[string]string),
		aliases:               make(map[string]AccountID),
		accountAliases:        make(map[AccountID]string),
		noOverdraft:           make(map[AccountID]bool),
		kyc:                 make(map[AccountID]KycProfile),
		invoices:            make(map[string]Invoice),
		invoicesByAccount:   make(map[AccountID][]string),
		deniedExact:           make(map[AccountID]DenylistEntry),
		deniedPrefixes:        make(map[string]DenylistEntry),
		apiKeys:               make(map[string]*APIKey),
		holderNames:           make(map[AccountID]*holderNameRecord),
		erasureRequests:       make(map[string]*ErasureRequest),
		erasedMemos:           make(map[string]ErasureRecord),
		dailyLimits:           make(map[dailyLimitKey]int64),
		dailyOutflow:          make(map[dailyLimitKey]int64),
		sweepReserves:         make(map[accountCurrency]int64),
		transferAllowlist:     make(map[AccountID]map[AccountID]bool),
		largeTxThresholds:     make(map[string]int64),
		largeTxReportCurrency: DefaultCurrency,
		largeTxReports:        make(map[largeTxReportKey]LargeTxReport),
		businessCalendar:      DefaultBusinessDayCalendar(),
		budgets:               make(map[budgetKey]int64),
		monthlySpend:          make(map[monthlySpendKey]int64),
		budgetPaceAlertRatio:  DefaultBudgetPaceAlertRatio,
		budgetPaceAlerted:     make(map[monthlySpendKey]bool),
		balanceWatches:        make(map[balanceWatchKey]BalanceWatch),
		balanceWatchBreached:  make(map[balanceWatchKey]watchBreachState),
		liquidityThresholds:   make(map[liquidityThresholdKey]LiquidityAlertThreshold),
		liquidityBreached:     make(map[liquidityThresholdKey]bool),
		settlementChannels:    make(map[AccountID]string),
		settlementCutoffs:     make(map[string]SettlementCutoff),
		settlementDeferrals:   make(map[string]*SettlementDeferral),
		parents:               make(map[AccountID]AccountID),
		transferKeys:          make(map[string][]string),
		reviews:               make(map[string]*TransferReview),
		reviewThresholds:      make(map[AccountID]int64),
		reviewDelegations:     make(map[delegationKey]*ReviewDelegation),
		multisigApprovers:     make(map[string]ed25519.PublicKey),
		multisigPolicies:      make(map[string]MultisigPolicy),
		multisigOps:           make(map[string]*MultisigOperation),
		holds:                 make(map[string]Hold),
		holdsByAccount:        make(map[AccountID][]string),
		holdKeys:              make(map[string]string),
		captureKeys:           make(map[string]CaptureReceipt),
		partialCaptureKeys:    make(map[string]PartialCaptureReceipt),
		termDeposits:          make(map[string]TermDeposit),
		termDepositKeys:       make(map[string]string),
		payouts:               make(map[string]PayoutInstruction),
		payoutKeys:            make(map[string]string),
		ftpCurves:             make(map[string]*FtpCurve),
		creditLines:           make(map[creditLineKey]CreditLine),
		eclStageConfig:        DefaultECLStageConfig(),
		eclExposures:          make(map[eclExposureKey]eclExposureRow),
		sweepKeys:             make(map[string]sweepRecord),
		batchKeys:             make(map[string]batchRecord),
		allocRules:            make(map[string]AllocationRule),
		allocKeys:             make(map[string]allocRecord),
		nettingKeys:           make(map[string]nettingRecord),
		installments:          make(map[string]InstallmentPlan),
		installmentKeys:       make(map[string]installmentKeyRecord),
		clearingBatches:       make(map[string]ClearingBatch),
		clearingKeys:          make(map[string]clearingKeyRecord),
		merges:                make(map[string]mergeRecord),
		mergeKeys:             make(map[string]string),
		splits:                make(map[string]splitRecord),
		splitKeys:             make(map[string]string),
		schedules:             make(map[string]*TransferSchedule),
		scheduleKeys:          make(map[string]string),
		scheduleRuns:          make(map[string][]ScheduleRun),
		scheduleFiring:        make(map[string]bool),
		reportSchedules:       make(map[string]*ReportSchedule),
		reportScheduleKeys:    make(map[string]string),
		reportScheduleRuns:    make(map[string][]ReportRun),
		reportScheduleFiring:  make(map[string]bool),
		fxRates:               make(map[fxPair]ExchangeRate),
		fxRateSnapshots:       make(map[uint64]FXRateTableSnapshot),
		forwards:              make(map[string]*FXForward),
		rateOverrides:         make(map[fxPair]*RateOverride),
		// LG-69 defaults: one-hour override lifetime, 10% deviation
		// cap. Both are construction-time config (see
		// WithRateOverrideTTL / WithRateOverrideMaxDeviationBps).
		rateOverrideTTL:             defaultRateOverrideTTL,
		rateOverrideMaxDeviationBps: defaultRateOverrideMaxDeviationBps,
		closedPeriods:               make(map[string]bool),
		closingPL:                   make(map[AccountID]bool),
		dunningConfigs:              make(map[dunningKey]DunningConfig),
		dunningStates:               make(map[dunningKey]DunningState),
		disputes:                    make(map[string]Dispute),
		disputeKeys:                 make(map[string]string),
		journalRules:                make(map[string]JournalRule),
		fixedAssets:                 make(map[string]FixedAsset),
		revalBaselines:              make(map[revalKey]revalBaseline),
		ofxImports:                  make(map[string]*OFXImportReport),
		camt053Imports:              make(map[string]*CAMT053ImportReport),
		statementMatches:            make(map[string]*statementMatchRecord),
		painTransfers:               make(map[string]PainTransfer),
		painInstructions:            make(map[string]*PainInstruction),
		accountIBANs:                make(map[AccountID]AccountIBAN),
		externalMappings:            make(map[externalMappingKey]ExternalAccountMapping),
		virtualAccounts:             make(map[AccountID]AccountID),
		// LG-125: the direct-debit mandate registry and its
		// mandate -> debit-entry index start empty; mandates are
		// created through CreateMandate and restored from snapshots.
		mandates:             make(map[string]Mandate),
		mandateDebits:        make(map[string][]string),
		mandateRetryPolicies: make(map[string]MandateRetryPolicy),
		mandateRetries:       make(map[string]*MandateRetry),
		mandateRetryFiring:   make(map[string]bool),
		pruneInterval:        defaultKeyPruneInterval,
	}
	for _, opt := range opts {
		opt(l)
	}
	// FX rate-table snapshot versioning (LG-45): rates installed at
	// construction (WithFXRateOption / LEDGER_FX_RATES) become snapshot
	// version 1, so every rate the ledger ever converts at is
	// reproducible from a snapshot. The lock is taken because
	// snapshotFXRateTableLocked emits an audit event, which requires it.
	if len(l.fxRates) > 0 {
		l.mu.Lock()
		l.snapshotFXRateTableLocked("genesis")
		l.mu.Unlock()
	}
	return l
}

// validateJournalEntry checks the double-entry fields of e and normalizes
// its currency in place: non-empty ID, non-empty distinct debit/credit
// legs, a positive amount, and a valid ISO 4217 code (empty normalizes to
// the default currency, like Post does). It records nothing, so it is safe
// to run as the admission gate of multi-entry operations (see PostBatch):
// a batch validates every entry through this before the first journal row
// lands. Callers that reuse a validated entry must not mutate its legs,
// amount, or currency afterwards.
func validateJournalEntry(e *JournalEntry) error {
	if e.ID == "" {
		return ErrEmptyID
	}
	if e.DebitAccount == "" {
		return ErrEmptyDebitAccount
	}
	if e.CreditAccount == "" {
		return ErrEmptyCreditAccount
	}
	if e.DebitAccount == e.CreditAccount {
		return ErrSameAccount
	}
	if e.AmountCents <= 0 {
		return ErrNonPositiveAmount
	}
	// The memo is field validation like the legs and the amount: a note
	// longer than maxMemoRunes UTF-8 characters is rejected before
	// anything is recorded.
	if err := checkMemoLength(e.Memo); err != nil {
		return err
	}
	// The external reference is field validation like the memo: a
	// reference longer than maxExternalRefRunes UTF-8 characters is
	// rejected before anything is recorded.
	if err := checkExternalRefLength(e.ExternalRef); err != nil {
		return err
	}
	// Attachments are field validation like the memo and the external
	// reference: more than maxAttachments entries, a non-https URI, or
	// a URI longer than maxAttachmentRunes characters is rejected before
	// anything is recorded.
	if err := checkAttachments(e.Attachments); err != nil {
		return err
	}
	// Metadata is field validation like the memo and the attachments:
	// more than maxMetadataPairs pairs, a key outside the tag-segment
	// charset, or a value longer than maxMetadataValueRunes UTF-8
	// characters is rejected before anything is recorded.
	if err := checkMetadata(e.Metadata); err != nil {
		return err
	}
	// Currency is field validation, like the legs and the amount: an
	// empty code normalizes to the default currency, anything else must
	// be a 3-letter uppercase ISO 4217 code. Normalization happens here,
	// before the idempotency replay check, so the journal, the replay
	// index, and the audit chain all store the canonical code.
	currency, err := normalizeCurrency(e.Currency)
	if err != nil {
		return err
	}
	e.Currency = currency
	return nil
}

// Post records a journal entry and applies its balance effects.
//
// Double-entry validation runs before anything is recorded: the entry must
// carry both legs of the posting — a non-empty debit account and a
// non-empty credit account, distinct from each other — a positive amount,
// and a valid currency (empty normalizes to the default currency; anything
// else must be a 3-letter uppercase ISO 4217 code), so the debit leg always
// equals the credit leg in one currency (the books balance by
// construction, per currency). A validation failure returns an error and
// records nothing.
//
// If e.IdempotencyKey is non-empty and the same key was posted before, Post
// returns the originally posted entry with duplicate == true and does not
// book anything again. A zero CreatedAt is filled with the current time.
//
// A sanctions/denylist hit (see denylist.go) rejects the post with
// ErrAccountDenied before the idempotency replay check below: screening
// is a legal obligation that applies to replays too, so a denied request
// leaves no idempotency trace and the attempt is audited as
// "denylist_rejected".
//
// A frozen account (see freeze.go) rejects any post that would book a new
// entry through it with ErrAccountFrozen. The frozen check runs after the
// idempotency replay check: replaying a key that was posted before the
// freeze returns the original entry instead of failing, because the replay
// books nothing new. A posting whose legs book against the accounts'
// registered chart-of-accounts types (see chart.go) — a debit leg on a
// liability/equity/revenue account, or a credit leg on an asset/expense
// account — is rejected with ErrAccountDirectionMismatch; the direction
// check runs after the replay check and before the frozen check for the
// same reason. A credit (payer) account under overdraft protection
// (see overdraft.go) rejects any post that would take its balance below
// zero with ErrAccountOverdraft; the check runs after the frozen check and
// after the idempotency replay check for the same reason. A credit
// (payer) account with a configured daily outflow limit (see
// SetDailyLimit) rejects any post that would take the UTC day's
// cumulative outflow above the limit with ErrDailyLimitExceeded; the check
// runs last among the account risk controls, so it only ever evaluates postings
// that book something new. A credit (payer) account with a configured
// monthly spend budget (see SetAccountBudget) rejects any post that would
// take the UTC calendar month's cumulative spend above the budget with
// ErrBudgetExceeded; it runs after the daily-limit check — the two are
// orthogonal (rolling outflow window vs accounting-period spend control)
// and a posting must clear both. A journal entry whose timestamp falls in a closed
// accounting period (see period.go) is rejected with ErrPeriodClosed; the
// period gate runs last overall, after the idempotency replay check, so
// replaying a key posted before the period closed returns the original
// entry. Rejected posts —
// validation failures, frozen rejections, chart-direction rejections,
// overdraft rejections,
// daily-limit rejections, budget rejections, and closed-period rejections alike —
// record nothing: no journal row, no chain link, no version bump.
//
// The commit is atomic: while holding the ledger's single mutex, Post
// applies every effect of the entry at once — the journal row, the
// idempotency index, both net balances, both debit/credit totals, and the
// audit-chain link — then bumps the version. Either all of them land or (on
// validation failure) none do; readers never observe a half-posted entry.
//
// Idempotent replays append no chain link: the chain records journaled
// entries, and a replay journals nothing new.
//
// Opt-in journal rules (see rules.go and SetJournalRule) may append
// derived legs to the posting: after the principal clears every gate
// above, each matching enabled rule appends its legs as full journal
// entries (own chain links, deterministic IDs "<entry ID>/rule/<rule
// ID>/<i>"), each re-checked through the frozen/overdraft/daily-limit/
// monthly-budget/period gates. The commit stays atomic across principal
// and derived legs, and the audit event lists every committed entry ID.
func (l *Ledger) Post(e JournalEntry) (posted JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.postLocked(e, true)
}

// postLocked is Post's body: the operator posting path. When fireRules is
// false, the journal rules engine is skipped: the principal still clears
// every gate and commits a full journal row (chain link, version bump,
// balance effects), but no derived legs are evaluated. System postings
// that must not cascade through operator-configured rules (accruals, see
// accrual.go) use the skip path. The dormant-account stop applies here:
// postings through dormant accounts are rejected with ErrAccountDormant
// (see dormancy.go). Callers must hold the write lock.
func (l *Ledger) postLocked(e JournalEntry, fireRules bool) (posted JournalEntry, duplicate bool, err error) {
	return l.postLockedGated(e, fireRules, true)
}

// postSystemLocked is the system-adjusting-entry posting path: the same
// gates as postLocked except the dormant-account stop. Accruals,
// dividends, and dormancy fees post through here — a dormant account's
// own inactivity fee must be chargeable, and corporate actions paying a
// dormant holder are the corporation's action, not operator activity
// through the account. Callers must hold the write lock.
func (l *Ledger) postSystemLocked(e JournalEntry, fireRules bool) (posted JournalEntry, duplicate bool, err error) {
	return l.postLockedGated(e, fireRules, false)
}

// postLockedGated is the shared posting core. When enforceDormant is
// true, postings through dormant accounts are rejected with
// ErrAccountDormant after the idempotency replay check (a replay books
// nothing new, so it must not fail on an account that went dormant after
// the original posting). Callers must hold the write lock.
func (l *Ledger) postLockedGated(e JournalEntry, fireRules bool, enforceDormant bool) (posted JournalEntry, duplicate bool, err error) {
	if err := validateJournalEntry(&e); err != nil {
		return JournalEntry{}, false, err
	}

	// Sanctions/denylist screening (see denylist.go): every account the
	// posting moves money through is screened before the idempotency
	// replay check — a hit rejects the posting and leaves no idempotency
	// trace, and the attempt is audited as "denylist_rejected".
	if acct, entry, denied := l.denylistRejectedLocked(e.DebitAccount, e.CreditAccount); denied {
		return JournalEntry{}, false, l.deniedRejectionLocked("Post", e.ID, acct, entry, []AccountID{e.DebitAccount, e.CreditAccount})
	}

	if e.IdempotencyKey != "" {
		if orig, ok := l.byKey[e.IdempotencyKey]; ok {
			return orig, true, nil
		}
	}

	// Virtual-account attribution (see virtual_account.go): a VAN on
	// the paying leg is rejected (ErrVANNoDebit — a virtual account
	// never holds a balance to pay from); a VAN on the receiving leg is
	// rewritten to its parent with a "van-attribution:<van>" memo
	// marker. Runs after the idempotency replay check — a replay books
	// nothing new, so it returns the original entry instead of failing
	// on a VAN registered after the original posting — and before every
	// account risk gate below, so frozen/overdraft/daily-limit/period
	// checks see the real (parent) account.
	var vanAttr *vanAttribution
	if vanAttr, err = l.resolveVANEntryLocked(&e); err != nil {
		return JournalEntry{}, false, err
	}

	// External-reference uniqueness is a journal-level constraint, not
	// bookkeeping validation: it runs after the idempotency replay
	// check — a replay of the original entry's own key returns the
	// original instead of tripping on its own reference. It is
	// independent of the idempotency-key namespace: the same key string
	// may appear as a key on one entry and as a reference on another.
	if e.ExternalRef != "" {
		if owner, taken := l.byRef[e.ExternalRef]; taken {
			return JournalEntry{}, false, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, e.ExternalRef, owner)
		}
	}

	// Chart-of-accounts direction control (see chart.go): a debit leg on
	// a liability/equity/revenue account, or a credit leg on an
	// asset/expense account, is rejected. Like the other account risk
	// controls it runs after the idempotency replay check: replaying a
	// key that was posted before the account was registered returns the
	// original entry instead of failing, because the replay books
	// nothing new. Rejected posts record nothing.
	if err := l.chartDirectionRejectedLocked(e); err != nil {
		return JournalEntry{}, false, err
	}

	// Expired timed freezes are released lazily on the write path: a
	// freeze whose auto-unfreeze deadline has passed must not keep
	// rejecting new posts (see FreezeUntil). The release runs before the
	// frozen gate below, so this Post sees the account unfrozen.
	l.releaseExpiredFreezesLocked(time.Now(), "write_path")

	// Expired KYC profiles are swept lazily on the write path, like the
	// timed-freeze release above: a verification that lapsed must stop
	// new postings from this Post on (see expireKycLocked in kyc.go).
	// The sweep runs before the KYC gate below, so this Post sees the
	// account expired.
	l.expireKycLocked(time.Now())

	// A closed account (see close.go) is a terminal lifecycle state and
	// the strongest of the account stops: it runs before the frozen
	// gate below, and rejected posts record nothing, bump nothing, and
	// leave the ledger version untouched. Like the frozen check it runs
	// after the idempotency replay check: a replay books nothing new, so
	// it must not fail on an account that closed after the original
	// posting.
	if l.closedLocked(e.DebitAccount) || l.closedLocked(e.CreditAccount) {
		return JournalEntry{}, false, ErrAccountClosed
	}
	// Dormant accounts (see dormancy.go) are a lifecycle state marking,
	// not fund control: like the closed check it runs after the
	// idempotency replay check — a replay books nothing new, so it must
	// not fail on an account that went dormant after the original
	// posting. It runs before the frozen check: lifecycle stops (closed,
	// dormant) precede risk-control stops. System adjusting entries
	// (accruals, dividends, dormancy fees) skip this gate via
	// postSystemLocked: charging a dormant account's own inactivity fee,
	// or paying a declared dividend to a dormant holder, is
	// bookkeeping, not operator activity.
	if enforceDormant && (l.dormantLocked(e.DebitAccount) || l.dormantLocked(e.CreditAccount)) {
		return JournalEntry{}, false, ErrAccountDormant
	}
	if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
		return JournalEntry{}, false, ErrAccountFrozen
	}
	// A suspended account (see suspend.go) is a reversible operational
	// pause: it rejects every write path, including the system postings
	// that funnel through here (accruals, dividends, dormancy fees,
	// ECL, escheat, capitalization, credit-line interest). It runs
	// after the frozen gate — the frozen stop is the older risk control
	// and wins on a double-set account — and after the idempotency
	// replay check above: a replay books nothing new, so it must not
	// fail on an account suspended after the original posting.
	if l.suspendedLocked(e.DebitAccount) || l.suspendedLocked(e.CreditAccount) {
		return JournalEntry{}, false, ErrAccountSuspended
	}

	// Overdraft protection is a risk control, not bookkeeping validation:
	// it runs after the frozen check (a frozen account fails 403 before
	// the overdraft question even arises) and after the idempotency replay
	// check above (a replay books nothing new, so it must not fail on an
	// account that was protected after the original posting).
	// A credit line (see creditline.go) lowers the floor to -limit for
	// the entry's currency: within-line postings clear the gate, and
	// breaching the floor is rejected with ErrCreditLimitExceeded.
	if err := l.overdraftRejectedLocked(e); err != nil {
		return JournalEntry{}, false, err
	}

	// KYC/尽调门控 (see kyc.go): an account whose KYC profile expired
	// rejects any new posting through it; an account with no (or a
	// non-verified) profile is rejected only above the ledger's opt-in
	// KYC threshold. The gate runs after the overdraft check — both are
	// account risk controls — and after the idempotency replay check
	// above: a replay books nothing new, so it must not fail on an
	// account whose KYC lapsed after the original posting. Rejected
	// posts record nothing.
	if err := l.kycRejectedLocked(e.DebitAccount, e.CreditAccount, e.AmountCents); err != nil {
		return JournalEntry{}, false, err
	}

	// CreatedAt is filled before the daily-limit check: the outflow is
	// booked against the entry's own UTC calendar day, so the timestamp
	// must be final when the limit is evaluated.
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	// Daily outflow limits are the last of the account risk controls, after
	// the frozen and overdraft checks: a replay books nothing new, so it
	// returned above; anything reaching this check books a new outflow.
	// The limited side is the credit (payer) account — the account funds
	// leave — matching PostTransfer's payer leg.
	if l.dailyLimitRejectedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt) {
		return JournalEntry{}, false, ErrDailyLimitExceeded
	}

	// Monthly spend budgets are the last of the account risk controls,
	// after the daily outflow limit: a replay books nothing new, so it
	// returned above; anything reaching this check books new spend. The
	// limited side is the credit (payer) account — the account funds
	// leave — matching the daily limit. The two are orthogonal (LG-26 is
	// a rolling outflow window, LG-81 an accounting-period spend
	// control) and accumulate independently: a posting must clear both.
	if l.budgetRejectedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt) {
		return JournalEntry{}, false, ErrBudgetExceeded
	}

	// The period gate is the last risk control overall, and it is keyed
	// on the entry's own timestamp — the last thing finalized above: a
	// backdated entry landing in a closed accounting period is rejected
	// with ErrPeriodClosed (see period.go). It runs after the idempotency
	// replay check, so replaying a key posted before the period closed
	// returns the original entry instead of failing.
	if err := l.periodRejectedLocked(e.CreatedAt); err != nil {
		return JournalEntry{}, false, err
	}

	// Journal rules (see rules.go) fire only after the principal entry
	// clears the full Post gate order above — validation, idempotency
	// replay, chart direction, frozen, overdraft, daily-limit, period.
	// When fireRules is false (system postings such as accruals), this
	// block is skipped entirely: the principal commits as a full
	// journal row without evaluating any rule. A replay returns the
	// original entry without re-firing rules; a principal that fails any
	// gate records nothing, derived legs included. Each derived leg
	// re-runs the account risk gates in the same order (frozen, then
	// overdraft and daily-limit on the aggregate per-credit-account
	// outflow, then the period gate on the shared timestamp): a leg that
	// fails any gate rejects the whole Post, and the commit below is
	// atomic across the principal and its derived legs — readers never
	// observe a principal without its rule legs.
	var derived []JournalEntry
	var ruleIDs []string
	if fireRules {
		derived, ruleIDs, err = l.derivedRuleLegsLocked(e)
		if err != nil {
			return JournalEntry{}, false, err
		}
	}

	l.maybePruneIdempotencyKeys(time.Now())
	l.maybePruneDailyOutflowLocked(time.Now())
	l.maybePruneMonthlySpendLocked(time.Now())
	versionBefore := l.version
	committed := make([]JournalEntry, 0, 1+len(derived))
	committed = append(committed, e)
	committed = append(committed, derived...)
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): a no-op allocation-free nil map when no
	// hook is registered.
	hookTouched := touchedAccounts(committed)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, c := range committed {
		l.commitEntryLocked(c)
		l.addDailyOutflowLocked(c.CreditAccount, c.Currency, c.AmountCents, c.CreatedAt)
		l.addMonthlySpendLocked(c.CreditAccount, c.Currency, c.AmountCents, c.CreatedAt)
	}
	l.ruleLegsCommitted.Add(uint64(len(derived)))
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only — it can
	// neither fail nor alter this posting.
	l.evaluateBalanceWatchesLocked(hookTouched, e.ID, "Post")
	postDetails := map[string]any{
		"amount_cents": e.AmountCents,
		"currency":     e.Currency,
	}
	if e.Memo != "" {
		postDetails["memo"] = e.Memo
	}
	if e.ExternalRef != "" {
		postDetails["external_ref"] = e.ExternalRef
	}
	if len(ruleIDs) > 0 {
		postDetails["rules_applied"] = ruleIDs
	}
	entryIDs := make([]string, 0, len(committed))
	accounts := []AccountID{e.DebitAccount, e.CreditAccount}
	for _, c := range committed {
		entryIDs = append(entryIDs, c.ID)
	}
	for _, d := range derived {
		accounts = append(accounts, d.DebitAccount, d.CreditAccount)
	}
	l.emitAudit(AuditEvent{
		Op:            "post",
		Actor:         "Post",
		TraceID:       e.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      accounts,
		Details:       postDetails,
	})
	// The virtual-account attribution rides its own audit event (see
	// virtual_account.go): the aliasing is a compliance fact distinct
	// from the posting itself.
	if vanAttr != nil {
		l.emitVANAttributionAuditLocked(vanAttr, e.ID, "Post")
	}
	// Domain outbox: one entry_posted per committed journal row
	// (principal plus any journal-rule derived legs).
	l.emitOutboxEntriesLocked(committed)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — the hook runs asynchronously and can
	// neither fail nor alter this posting.
	l.fireBalanceHooksLocked(hookTouched, hookOld, e.ID)

	return e, false, nil
}

// Balance returns the current net balance (in cents) of the given account
// in the default currency (see DefaultCurrency). Unknown accounts have a
// zero balance. In a multi-currency ledger a single number cannot describe
// an account — use BalanceIn or TrialBalance for a specific currency, or
// TrialBalance's ByCurrency breakdown for the full picture. Balance keeps
// its historical meaning (the default-currency balance) so existing
// readers keep working unchanged.
func (l *Ledger) Balance(a AccountID) int64 {
	return l.BalanceIn(a, DefaultCurrency)
}

// Version returns the ledger's current journal version: the count of
// committed journal entries. Read-only.
func (l *Ledger) Version() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}

// BalanceIn returns the current net balance (in cents) of the given
// account in the given currency. Unknown accounts, and accounts with no
// postings in that currency, have a zero balance. An empty currency means
// the default currency; other codes are looked up as-is (codes that never
// appeared simply report zero).
func (l *Ledger) BalanceIn(a AccountID, currency string) int64 {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[accountCurrency{account: a, currency: currency}]
}

// GetByIdempotencyKey returns the entry previously posted with the given
// idempotency key, or false if the key has never been posted.
func (l *Ledger) GetByIdempotencyKey(key string) (JournalEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.byKey[key]
	return e, ok
}

// ExpireIdempotencyKeys evicts idempotency keys whose entry was posted more
// than the configured TTL ago, and returns how many were removed. It is a
// no-op returning 0 when no TTL is configured. Expired keys are forgotten
// entirely: reposting the same key afterwards books a brand-new entry
// (duplicate == false), so callers must pick a TTL longer than any retry or
// reconciliation window they rely on. The journal itself is untouched — only
// the replay-detection index shrinks.
func (l *Ledger) ExpireIdempotencyKeys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pruneIdempotencyKeysLocked(time.Now())
}

// maybePruneIdempotencyKeys runs a lazy key sweep at most once per
// pruneInterval. It amortizes expiry across Posts so no background goroutine
// is needed. Callers must hold l.mu.
func (l *Ledger) maybePruneIdempotencyKeys(now time.Time) {
	if l.idempotencyTTL <= 0 {
		return
	}
	if now.Sub(l.lastKeyPrune) < l.pruneInterval {
		return
	}
	l.lastKeyPrune = now
	l.pruneIdempotencyKeysLocked(now)
}

// pruneIdempotencyKeysLocked removes keys older than the TTL. A non-positive
// TTL disables pruning. Callers must hold l.mu.
func (l *Ledger) pruneIdempotencyKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, e := range l.byKey {
		if e.CreatedAt.Before(cutoff) {
			delete(l.byKey, key)
			// A transfer's receipt index expires with its principal key:
			// after the TTL, reposting the transfer key books brand-new
			// entries, exactly like the entry-level contract — unless
			// the key belongs to a still-pending review (see review.go):
			// the review's replay must keep returning the review
			// receipt, not a transfer-ID conflict.
			if !l.pendingReviewByKeyLocked(key) {
				delete(l.transferKeys, key)
			}
			removed++
		}
	}
	// Hold and capture keys expire on the same schedule, in their own
	// namespaces (see pruneHoldKeysLocked in hold.go). Sweep keys expire
	// on the same schedule too (see pruneSweepKeysLocked in sweep.go),
	// and so do merge keys (see pruneMergeKeysLocked in merge.go) and
	// batch keys (see pruneBatchKeysLocked in batch.go). Split keys
	// expire on the same schedule, in their own namespace (see
	// pruneSplitKeysLocked in split.go). Netting session
	// keys expire on the same schedule, in their own namespace (see
	// pruneNettingKeysLocked in netting.go), and so do allocation receipt
	// keys (see pruneAllocKeysLocked in allocation.go) and term-deposit
	// open keys (see pruneTermDepositKeysLocked in termdeposit.go).
	// Dispute-raise idempotency keys expire on the same schedule, in
	// their own namespace (see pruneDisputeKeysLocked in dispute.go):
	// the disputes themselves are never evicted — only the
	// replay-detection index shrinks. Payout open keys expire on the
	// same schedule, in their own namespace (see prunePayoutKeysLocked
	// in payout.go).
	removed += l.pruneHoldKeysLocked(now)
	removed += l.pruneSweepKeysLocked(now)
	removed += l.pruneMergeKeysLocked(now)
	removed += l.pruneSplitKeysLocked(now)
	removed += l.pruneBatchKeysLocked(now)
	removed += l.pruneNettingKeysLocked(now)
	removed += l.pruneInstallmentKeysLocked(now)
	removed += l.pruneClearingKeysLocked(now)
	removed += l.pruneAllocKeysLocked(now)
	removed += l.pruneTermDepositKeysLocked(now)
	removed += l.pruneDisputeKeysLocked(now)
	removed += l.prunePayoutKeysLocked(now)
	return removed
}

// Entries returns a copy of all posted journal entries.
func (l *Ledger) Entries() []JournalEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]JournalEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	return out
}

// Snapshot returns the current net balance (in cents) of the given account
// in the default currency (see DefaultCurrency), together with the ledger
// version at read time. Unknown accounts have a zero balance. The version
// is bumped by every successful Post (idempotent replays and rejected
// entries do not count), so two snapshots with the same version are
// guaranteed to show the same balance — a cheap change detector for
// reconciliation jobs. For a specific currency, use BalanceIn; the
// version it pairs with is the same ledger-wide sequence.
func (l *Ledger) Snapshot(a AccountID) (balance int64, version uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[accountCurrency{account: a, currency: DefaultCurrency}], l.version
}

// TrialBalance is the double-entry breakdown of a single account: every cent
// ever debited to it, every cent ever credited from it, and the resulting
// net balance. NetBalance always equals TotalDebits - TotalCredits; across
// the whole ledger, the sum of all debit totals equals the sum of all
// credit totals within each currency — that per-currency equality is the
// accounting equation, and VerifyAccountingEquation checks it.
//
// The top-level totals describe the account in Currency (the default
// currency unless stated otherwise); ByCurrency breaks the same account
// down per currency, sorted by currency code, so multi-currency accounts
// report every currency they hold. A single-currency account's ByCurrency
// holds exactly one row, mirroring the top-level totals.
type TrialBalance struct {
	Account AccountID `json:"account"`
	// Alias is the account's human-readable alias (see SetAlias),
	// echoed so reporting consumers can label rows without a second
	// lookup. It is omitted when the account has no alias.
	Alias        string `json:"alias,omitempty"`
	Currency     string `json:"currency"`
	TotalDebits  int64  `json:"total_debits_cents"`
	TotalCredits int64  `json:"total_credits_cents"`
	NetBalance   int64  `json:"net_balance_cents"`
	// ByCurrency is the per-currency breakdown of this account, sorted by
	// currency code. It always covers every currency the account has
	// postings in, including Currency itself.
	ByCurrency []CurrencyTrialBalance `json:"by_currency"`
	Version    uint64                 `json:"version"`
	// Frozen reports whether the account is currently stopped by a
	// risk-control freeze (see Freeze). A frozen account keeps its
	// balances and history; only new postings through it are rejected.
	Frozen bool `json:"frozen"`
	// Closed reports whether the account is closed (see CloseAccount):
	// the terminal lifecycle state. A closed account keeps its
	// balances and history forever, but no new journal entry may book
	// through it — every write path rejects with ErrAccountClosed.
	// Closing is irreversible, unlike the frozen stop above.
	Closed bool `json:"closed"`
	// OverdraftProtected reports whether the account is guarded against
	// overdrafts (see EnableOverdraftProtection): Postings that would
	// take the balance below zero are rejected with ErrAccountOverdraft.
	OverdraftProtected bool `json:"overdraft_protected"`
	// DailyLimitCents reports the account's configured daily outflow
	// limit in the default currency (see SetDailyLimit): postings that
	// would take the UTC day's cumulative outflow above this number are
	// rejected with ErrDailyLimitExceeded. 0 means no limit is configured
	// for the default currency; per-currency limits live in the
	// reconciliation report and on Ledger.DailyLimit.
	DailyLimitCents int64 `json:"daily_limit_cents"`
	// BudgetCents reports the account's configured monthly spend budget
	// in the default currency (see SetAccountBudget): postings that
	// would take the UTC calendar month's cumulative spend above this
	// number are rejected with ErrBudgetExceeded. 0 means no budget is
	// configured for the default currency; per-currency budgets live in
	// the reconciliation report and on Ledger.AccountBudget.
	BudgetCents int64 `json:"budget_cents"`
	// LowBalanceThresholdCents reports the account's configured
	// low-balance alert level in the default currency (see
	// SetLowBalanceThreshold): the first posting that takes the balance
	// below this number emits one balance_threshold_breached audit event
	// (direction "low"). 0 means no low threshold is configured for the
	// default currency; per-currency watches live in the reconciliation
	// report and on Ledger.GetBalanceWatch.
	LowBalanceThresholdCents int64 `json:"low_balance_threshold_cents"`
	// HighBalanceThresholdCents reports the account's configured
	// high-balance alert level in the default currency (see
	// SetBalanceWatch): the first posting that takes the balance above
	// this number emits one balance_threshold_breached audit event
	// (direction "high"). 0 means no high threshold is configured for
	// the default currency.
	HighBalanceThresholdCents int64 `json:"high_balance_threshold_cents"`
	// AccountType reports the account's registered chart-of-accounts
	// type (see RegisterAccountType): asset, liability, equity,
	// revenue, or expense. Empty when the account is unregistered —
	// unregistered accounts skip Post's direction check.
	AccountType AccountType `json:"account_type,omitempty"`
	// Tags reports the account's tag set (see SetAccountTags): the
	// classification labels used by the tag-dimension rollup. Empty
	// when the account carries no tags.
	Tags map[string]string `json:"tags,omitempty"`
}

// TrialBalance returns the double-entry breakdown of the given account at
// the current ledger version. Unknown accounts report zeros. The version is
// the same sequence Snapshot reports, so a trial balance and a snapshot
// taken at one version describe the same books.
func (l *Ledger) TrialBalance(a AccountID) TrialBalance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.trialBalanceLocked(a)
}

// trialBalanceLocked builds the trial balance. Callers must hold l.mu; the
// read lock suffices because nothing is mutated. The reconciliation scan
// calls this directly so the per-account rows are built under the same
// read lock as the rest of the report.
func (l *Ledger) trialBalanceLocked(a AccountID) TrialBalance {
	tb := TrialBalance{
		Account:                   a,
		Alias:                     l.accountAliases[a],
		Currency:                  DefaultCurrency,
		Version:                   l.version,
		Frozen:                    l.frozen[a],
		Closed:                    l.closedLocked(a),
		AccountType:               l.chart[a],
		OverdraftProtected:        l.noOverdraft[a],
		DailyLimitCents:           l.dailyLimits[dailyLimitKey{account: a, currency: DefaultCurrency}],
		BudgetCents:               l.budgets[budgetKey{account: a, currency: DefaultCurrency}],
		LowBalanceThresholdCents:  l.balanceWatchLowCentsLocked(a, DefaultCurrency),
		HighBalanceThresholdCents: l.balanceWatchHighCentsLocked(a, DefaultCurrency),
	}
	if t, ok := l.tags[a]; ok {
		cp := make(map[string]string, len(t))
		for k, v := range t {
			cp[k] = v
		}
		tb.Tags = cp
	}
	seen := make(map[string]bool)
	for k := range l.balances {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	for k := range l.debitTotals {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	for k := range l.creditTotals {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	currencies := make([]string, 0, len(seen))
	for c := range seen {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		d := l.debitTotals[accountCurrency{account: a, currency: c}]
		cr := l.creditTotals[accountCurrency{account: a, currency: c}]
		row := CurrencyTrialBalance{
			Currency:     c,
			TotalDebits:  d,
			TotalCredits: cr,
			NetBalance:   l.balances[accountCurrency{account: a, currency: c}],
		}
		tb.ByCurrency = append(tb.ByCurrency, row)
		if c == DefaultCurrency {
			tb.TotalDebits = d
			tb.TotalCredits = cr
			tb.NetBalance = row.NetBalance
		}
	}
	return tb
}

// VerifyAccountingEquation checks the two invariants double-entry
// bookkeeping guarantees, isolated per currency: for every
// (account, currency), net balance == total debits − total credits; and
// within each currency, total debits == total credits (equivalently, the
// sum of all net balances in that currency is zero). Currencies never mix
// in the check — adding USD cents to EUR cents would be meaningless, so
// each currency's books must balance on their own. It returns nil when the
// books balance. Operators can run it after imports or restores; the test
// suite runs it after every scenario, including the concurrent stress test.
func (l *Ledger) VerifyAccountingEquation() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	seen := make(map[accountCurrency]bool)
	for k := range l.balances {
		seen[k] = true
	}
	for k := range l.debitTotals {
		seen[k] = true
	}
	for k := range l.creditTotals {
		seen[k] = true
	}
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for k := range seen {
		d := l.debitTotals[k]
		c := l.creditTotals[k]
		if want := d - c; l.balances[k] != want {
			return fmt.Errorf("ledger: account %q (%s) out of balance: net %d != debits %d - credits %d",
				k.account, k.currency, l.balances[k], d, c)
		}
		debits[k.currency] += d
		credits[k.currency] += c
	}
	for currency, d := range debits {
		if c := credits[currency]; d != c {
			return fmt.Errorf("ledger: books do not balance in %s: total debits %d != total credits %d",
				currency, d, c)
		}
	}
	return nil
}

// ListEntries returns up to limit journal entries whose CreatedAt falls in
// [since, until), sorted by (CreatedAt, ID). Pagination is cursor-based:
// pass the nextCursor returned by the previous call to continue; an empty
// cursor starts from the beginning. The returned nextCursor is empty when
// the last page has been returned.
//
// The cursor is opaque (base64url of the last returned entry's ID) and the
// resume position is defined by sort order, so entries posted between two
// pages are never duplicated or skipped: the ledger is append-only, so an
// entry named by a cursor can never move. A cursor naming an entry outside
// the requested window is rejected with ErrInvalidCursor.
func (l *Ledger) ListEntries(since, until time.Time, cursor string, limit int) (page []JournalEntry, nextCursor string, err error) {
	return l.ListEntriesFiltered(since, until, cursor, limit, "", "")
}

// ListEntriesFiltered is ListEntries with additional memo keyword and
// external-reference filters (see JournalEntry.Memo and
// JournalEntry.ExternalRef): when memo is non-empty, only entries whose
// memo contains it as a substring are returned; when ref is non-empty,
// only entries whose external reference equals it exactly are returned.
// The filters compose with each other, with the time window, and with
// the cursor pagination — a cursor from a filtered page resumes the same
// filtered sequence, because the cursor names an entry by ID and the
// filters are re-applied before pagination. The memo match is a
// case-sensitive substring search (memo notes are identifiers where case
// matters); the ref match is an exact equality, because references are a
// uniqueness domain.
func (l *Ledger) ListEntriesFiltered(since, until time.Time, cursor string, limit int, memo, ref string) (page []JournalEntry, nextCursor string, err error) {
	return l.ListEntriesMetadataFiltered(since, until, cursor, limit, memo, ref, nil)
}

// ListEntriesMetadataFiltered is ListEntriesFiltered with an additional
// structured-metadata filter (see JournalEntry.Metadata): when meta is
// non-empty, only entries carrying every (key, value) pair are returned.
// The metadata match is an exact equality per pair, like the ref filter.
// It composes with the memo and ref filters, the time window, and the
// cursor pagination, exactly like the other filters.
func (l *Ledger) ListEntriesMetadataFiltered(since, until time.Time, cursor string, limit int, memo, ref string, meta map[string]string) (page []JournalEntry, nextCursor string, err error) {
	if limit < 1 || limit > maxPageSize {
		return nil, "", ErrInvalidLimit
	}
	afterID, err := decodeListCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	l.mu.RLock()
	filtered := make([]JournalEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.CreatedAt.Before(since) {
			continue
		}
		if !e.CreatedAt.Before(until) {
			continue
		}
		if memo != "" && !strings.Contains(e.Memo, memo) {
			continue
		}
		if ref != "" && e.ExternalRef != ref {
			continue
		}
		if len(meta) > 0 && !metadataMatch(e.Metadata, meta) {
			continue
		}
		filtered = append(filtered, e)
	}
	l.mu.RUnlock()

	return paginateSortedEntries(filtered, afterID, limit)
}

// metadataMatch reports whether entry carries every (key, value) pair of
// want. An entry with no metadata matches only an empty want — callers
// pass a non-empty want, so the loop body decides.
func metadataMatch(entry, want map[string]string) bool {
	for k, v := range want {
		if ev, ok := entry[k]; !ok || ev != v {
			return false
		}
	}
	return true
}
