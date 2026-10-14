package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fee splits (opt-in).
//
// A transfer's fee — explicit FeeCents or the ledger's fee-policy charge —
// can be divided across several fee recipients in basis points instead of
// going to one fee account. The caller sets Transfer.FeeSplits; the
// recipients' basis points must sum to exactly 10000, each recipient's
// share is floor(fee * bps / 10000), and the dust remainder lands on the
// first recipient, so the shares always sum to the fee exactly.
//
// Fee splits are orthogonal to cost allocations (see allocation.go):
// allocation divides a principal expense across cost centers, fee splits
// divide the fee across recipients. The two never interact — an
// allocation knows nothing about fees and a fee split never touches the
// principal.
//
// Each recipient gets its own fee leg journal entry with the deterministic
// ID "<transfer ID>/fee/<i>" (i from 0), booked debit-recipient /
// credit-payer like the single fee leg. The memo carries the split
// disclosure "fee-split:<i>/<n>:<account>:<bps>" so idempotent replays and
// the operator API can rebuild the per-recipient breakdown from the
// journal alone — the journal stays the source of truth across disaster
// recovery snapshots.
//
// Gate order mirrors the single fee leg exactly: the fee total is already
// folded into the payer's total-outflow checks (amount + fee), so the
// split changes nothing there; every recipient walks the same
// closed/frozen checks the single fee account walks (see transfer.go and
// fx.go). Like the single fee leg, recipients are deliberately not
// denylist-screened: they are operator-designated revenue accounts, and
// screening them would let the operator deny itself into a self-inflicted
// outage.
//
// Fee splits compose with PostTransfer (plain and cross-currency), not
// with PostTransferSplit: chunking a transfer and splitting its fee per
// recipient would need per-chunk split legs, which the journal ID scheme
// does not define. PostTransferSplit rejects FeeSplits fail-fast (see
// ErrFeeSplitUnsupported) instead of misbooking.

// Validation errors for fee splits.
var (
	// ErrInvalidFeeSplit is returned when a fee split names an empty
	// account or a basis-point share outside 1..10000: a zero share
	// books nothing and a share above 100% of the fee is a caller bug,
	// not a clampable slice.
	ErrInvalidFeeSplit = errors.New("ledger: invalid fee split: account must be non-empty and bps between 1 and 10000")
	// ErrFeeSplitSum is returned when the recipients' basis points do
	// not sum to exactly 10000: a split that does not cover the whole
	// fee is a caller bug.
	ErrFeeSplitSum = errors.New("ledger: fee split basis points must sum to exactly 10000")
	// ErrFeeSplitFeeAccountConflict is returned when FeeSplits and
	// FeeAccount are both set: FeeSplits replaces the single fee
	// account, so the two are mutually exclusive.
	ErrFeeSplitFeeAccountConflict = errors.New("ledger: fee_splits and fee_account are mutually exclusive")
	// ErrFeeSplitSkipFeeConflict is returned when FeeSplits is set
	// together with SkipFee: a split needs a fee to divide, and
	// SkipFee suppresses the only fee source left once FeeAccount is
	// out of the picture.
	ErrFeeSplitSkipFeeConflict = errors.New("ledger: fee_splits requires a fee: not allowed with skip_fee")
	// ErrFeeSplitZeroFee is returned when FeeSplits is set but the
	// resolved fee is zero — no explicit FeeCents and no policy fee
	// (disabled policy, SkipFee, or a fee-free dust tier). A split of
	// nothing is a caller misconfiguration, not a no-op: silently
	// dropping the recipients would hide the fact that the intended
	// revenue share never booked, so the transfer is rejected and the
	// caller must set an explicit fee or enable the policy.
	ErrFeeSplitZeroFee = errors.New("ledger: fee_splits requires a positive fee: set fee_cents or enable the transfer fee policy")
	// ErrFeeSplitUnsupported is returned by PostTransferSplit when
	// FeeSplits is set: fee splits compose with PostTransfer, not with
	// chunked transfers.
	ErrFeeSplitUnsupported = errors.New("ledger: fee_splits is not supported on split transfers")
	// ErrTransferNotFound is returned by TransferFeeSplit when no
	// transfer with the given ID was posted.
	ErrTransferNotFound = errors.New("ledger: transfer not found")
	// ErrNoFeeSplit is returned by TransferFeeSplit when the transfer
	// exists but posted no fee split: either it had no fee leg at all,
	// or its fee went to a single fee account.
	ErrNoFeeSplit = errors.New("ledger: transfer has no fee split")
)

// FeeSplit is one recipient of a transfer's fee: Bps basis points
// (1..10000) of the fee go to Account. The transfer's FeeSplits must
// sum to exactly 10000 basis points.
type FeeSplit struct {
	Account AccountID `json:"account"`
	Bps     int64     `json:"bps"`
}

// FeeSplitShare is one recipient's booked share of a transfer's fee:
// the basis-point slice it was promised and the cents actually booked
// (floor(fee * bps / 10000), plus the dust remainder on the first
// recipient).
type FeeSplitShare struct {
	Account  AccountID `json:"account"`
	Bps      int64     `json:"bps"`
	FeeCents int64     `json:"fee_cents"`
}

// FeeSplitDetail is the operator-facing disclosure of a transfer's fee
// split (see TransferFeeSplit): the total fee and each recipient's
// booked share, in recipient order.
type FeeSplitDetail struct {
	TransferID string          `json:"transfer_id"`
	FeeCents   int64           `json:"fee_cents"`
	Splits     []FeeSplitShare `json:"splits"`
}

// validateFeeSplits checks the field-level shape of a fee split list:
// non-empty accounts, basis points in 1..10000, and an exact 10000
// total. It does not check the fee itself — the zero-fee rejection
// lives in resolveFeeLocked, the first place the resolved fee is known.
func validateFeeSplits(splits []FeeSplit) error {
	var sum int64
	for _, s := range splits {
		if s.Account == "" {
			return ErrInvalidFeeSplit
		}
		if s.Bps < 1 || s.Bps > 10000 {
			return ErrInvalidFeeSplit
		}
		sum += s.Bps
	}
	if sum != 10000 {
		return ErrFeeSplitSum
	}
	return nil
}

// computeFeeSplitShares divides feeCents across the split recipients:
// floor(fee * bps / 10000) per recipient, computed overflow-safe like
// the fee policy (see policyFeeCents — bps is validated to 1..10000),
// with the dust remainder landing on the first recipient so the shares
// sum to the fee exactly. Callers must have validated the splits and
// resolved a positive fee.
func computeFeeSplitShares(feeCents int64, splits []FeeSplit) []FeeSplitShare {
	out := make([]FeeSplitShare, len(splits))
	var assigned int64
	for i, s := range splits {
		share := policyFeeCents(feeCents, s.Bps)
		out[i] = FeeSplitShare{Account: s.Account, Bps: s.Bps, FeeCents: share}
		assigned += share
	}
	// The dust remainder lands on the first recipient (the allocation.go
	// convention): the shares always sum to the fee exactly.
	out[0].FeeCents += feeCents - assigned
	return out
}

// feeSplitMemoPrefix marks a fee-split leg's memo, carrying the
// per-recipient disclosure the journal needs for replays and the
// operator API: "fee-split:<i>/<n>:<account>:<bps>". The account rides
// last so an account name containing ":" still parses — parseFeeSplitMemo
// splits from the outside in (index/count, then bps, then the account).
const feeSplitMemoPrefix = "fee-split:"

// feeSplitLegMemo builds the disclosure memo for split leg i of n.
func feeSplitLegMemo(i, n int, account AccountID, bps int64) string {
	return feeSplitMemoPrefix + strconv.Itoa(i) + "/" + strconv.Itoa(n) +
		":" + string(account) + ":" + strconv.FormatInt(bps, 10)
}

// parseFeeSplitMemo recovers the split disclosure from a journaled leg
// memo. ok is false for anything that is not a fee-split memo (plain
// transfers' fee legs carry no memo at all).
func parseFeeSplitMemo(memo string) (account AccountID, bps int64, idx, total int, ok bool) {
	rest, found := strings.CutPrefix(memo, feeSplitMemoPrefix)
	if !found {
		return "", 0, 0, 0, false
	}
	idxStr, rest, found := strings.Cut(rest, "/")
	if !found {
		return "", 0, 0, 0, false
	}
	// The remainder is "<n>:<account>:<bps>": bps rides after the last
	// colon, so an account name containing ":" parses intact.
	j := strings.LastIndex(rest, ":")
	if j < 0 {
		return "", 0, 0, 0, false
	}
	bpsStr := rest[j+1:]
	rest = rest[:j]
	totalStr, accountStr, found := strings.Cut(rest, ":")
	if !found {
		return "", 0, 0, 0, false
	}
	i, err1 := strconv.Atoi(idxStr)
	n, err2 := strconv.Atoi(totalStr)
	b, err3 := strconv.ParseInt(bpsStr, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || i < 0 || n <= 0 || b <= 0 {
		return "", 0, 0, 0, false
	}
	return AccountID(accountStr), b, i, n, true
}

// isFeeLegID reports whether a journal entry ID is a transfer fee leg:
// the single "<transfer ID>/fee" leg, or a fee-split leg
// "<transfer ID>/fee/<i>". The numeric-index check keeps an unrelated
// "/fee/" segment — e.g. inside a caller-chosen transfer ID — from being
// mistaken for a split leg. Chunk fee legs ("<id>/chunk-<i>/fee", see
// transfer_split.go) match by the "/fee" suffix, like before.
// isFeeLegID reports whether id is a fee leg of the given transfer: the
// single "<transfer ID>/fee" entry or a fee-split "<transfer ID>/fee/<i>"
// leg. Anchoring on the transfer ID (rather than a fuzzy suffix match)
// keeps a user-chosen transfer ID that merely ends in "/fee/<digits>"
// from being misread as a fee leg.
func isFeeLegID(transferID, id string) bool {
	if id == transferID+"/fee" {
		return true
	}
	rest, ok := strings.CutPrefix(id, transferID+"/fee/")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// feeLegIDs returns the deterministic journal entry IDs for a
// transfer's fee legs: none when there is no fee, a single
// "<transfer ID>/fee" for one fee account, or "<transfer ID>/fee/<i>"
// per split recipient. The single-leg shape is unchanged from before
// fee splits existed, so non-split transfers book byte-identical rows.
func feeLegIDs(transferID string, feeCents int64, splits []FeeSplitShare) []string {
	if feeCents <= 0 {
		return nil
	}
	if len(splits) == 0 {
		return []string{transferID + "/fee"}
	}
	ids := make([]string, len(splits))
	for i := range splits {
		ids[i] = transferID + "/fee/" + strconv.Itoa(i)
	}
	return ids
}

// feeLegAccounts returns every account a resolved fee plan touches: the
// single fee account, or each split recipient. The closed/frozen checks
// walk this list, so every recipient gets the same frozen coverage the
// single fee account always had.
func feeLegAccounts(feeAccount AccountID, splits []FeeSplitShare) []AccountID {
	if len(splits) > 0 {
		out := make([]AccountID, len(splits))
		for i, s := range splits {
			out[i] = s.Account
		}
		return out
	}
	if feeAccount != "" {
		return []AccountID{feeAccount}
	}
	return nil
}

// buildFeeLegEntries constructs the journal entries for a resolved fee
// plan: one "<transfer ID>/fee" leg for a single fee account, or one
// "<transfer ID>/fee/<i>" leg per split recipient. Every leg debits its
// recipient and credits the payer, in the transfer's currency — the fee
// stays in the source currency on cross-currency transfers, like the
// single fee leg it generalizes. The single-leg shape is byte-identical
// to the pre-split fee leg (same ID, same idempotency-key namespace,
// no memo); split legs carry the fee-split disclosure memo (see
// feeSplitLegMemo) so replays rebuild the breakdown from the journal.
func buildFeeLegEntries(t Transfer, feeCents int64, feeAccount AccountID, splits []FeeSplitShare, createdAt time.Time) []JournalEntry {
	if feeCents <= 0 {
		return nil
	}
	feeKey := func(i int) string {
		if t.IdempotencyKey == "" {
			return ""
		}
		if len(splits) == 0 {
			return t.IdempotencyKey + "/fee"
		}
		return t.IdempotencyKey + "/fee/" + strconv.Itoa(i)
	}
	if len(splits) == 0 {
		return []JournalEntry{{
			ID:             t.ID + "/fee",
			DebitAccount:   feeAccount,
			CreditAccount:  t.From,
			AmountCents:    feeCents,
			Currency:       t.Currency,
			IdempotencyKey: feeKey(0),
			CreatedAt:      createdAt,
		}}
	}
	out := make([]JournalEntry, len(splits))
	for i, s := range splits {
		out[i] = JournalEntry{
			ID:             t.ID + "/fee/" + strconv.Itoa(i),
			DebitAccount:   s.Account,
			CreditAccount:  t.From,
			AmountCents:    s.FeeCents,
			Currency:       t.Currency,
			IdempotencyKey: feeKey(i),
			Memo:           feeSplitLegMemo(i, len(splits), s.Account, s.Bps),
			CreatedAt:      createdAt,
		}
	}
	return out
}

// feeSplitSharesFromLegs rebuilds the per-recipient split disclosure
// from journaled entries: the split legs of transferID, ordered by
// their "<transferID>/fee/<i>" index. It returns nil when the transfer
// posted no split legs (no fee, or a single fee account). The journal
// is the source of truth, so idempotent replays, the operator API, and
// disaster-recovery restores all read the same breakdown.
func feeSplitSharesFromLegs(transferID string, entries []JournalEntry) []FeeSplitShare {
	prefix := transferID + "/fee/"
	type indexedShare struct {
		idx   int
		share FeeSplitShare
	}
	var found []indexedShare
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.ID, prefix)
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(rest)
		if err != nil || idx < 0 {
			continue
		}
		account, bps, _, _, ok := parseFeeSplitMemo(e.Memo)
		if !ok {
			continue
		}
		found = append(found, indexedShare{idx, FeeSplitShare{Account: account, Bps: bps, FeeCents: e.AmountCents}})
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].idx < found[j].idx })
	out := make([]FeeSplitShare, len(found))
	for i, f := range found {
		out[i] = f.share
	}
	return out
}

// feeSplitAuditDetails renders the split breakdown for the structured
// audit event (the "transfer" op's details): one row per recipient with
// the promised basis points and the cents actually booked.
func feeSplitAuditDetails(splits []FeeSplitShare) []any {
	out := make([]any, len(splits))
	for i, s := range splits {
		out[i] = map[string]any{
			"account":   string(s.Account),
			"bps":       s.Bps,
			"fee_cents": s.FeeCents,
		}
	}
	return out
}

// TransferFeeSplit reports a posted transfer's fee split: the total fee
// and each recipient's booked share, in recipient order. It returns
// ErrTransferNotFound for an unknown transfer ID and ErrNoFeeSplit —
// with a hint — when the transfer posted no fee split (no fee leg, or a
// single fee account). The breakdown is rebuilt from the journaled legs'
// memos, so it works for settled, review-frozen, and snapshot-restored
// transfers alike. Callers must not hold l.mu; it takes the read lock.
func (l *Ledger) TransferFeeSplit(transferID string) (FeeSplitDetail, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.entries[transferID]; !ok {
		return FeeSplitDetail{}, fmt.Errorf("%w: %q", ErrTransferNotFound, transferID)
	}
	var splits []FeeSplitShare
	var total int64
	// Split leg IDs are contiguous from 0 by construction, so the
	// first missing index ends the scan.
	for i := 0; ; i++ {
		e, ok := l.entries[transferID+"/fee/"+strconv.Itoa(i)]
		if !ok {
			break
		}
		account, bps, _, _, ok := parseFeeSplitMemo(e.Memo)
		if !ok {
			// A journaled split leg without a parseable disclosure
			// memo is corruption-adjacent, but the operator query
			// must not fail: disclose what the leg itself says —
			// its account and booked cents — with the bps unknown.
			account, bps = e.DebitAccount, 0
		}
		splits = append(splits, FeeSplitShare{Account: account, Bps: bps, FeeCents: e.AmountCents})
		total += e.AmountCents
	}
	if len(splits) == 0 {
		return FeeSplitDetail{}, fmt.Errorf("%w: transfer %q posted no fee split (fee_splits is only set when the transfer divided its fee across recipients; a single fee account books one \"<transfer ID>/fee\" leg instead)", ErrNoFeeSplit, transferID)
	}
	return FeeSplitDetail{TransferID: transferID, FeeCents: total, Splits: splits}, nil
}
