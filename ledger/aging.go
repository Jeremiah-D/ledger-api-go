package ledger

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// AR aging analysis (LG-50).
//
// The aging view answers the collections question a payments operator
// asks: for each account that owes us money, how old is the outstanding
// balance, in which currency, and which accounts are sitting on overdue
// receivables past the alert threshold?
//
// Semantics, kept honest:
//
//   - "Unsettled" is the current balance, built from effective journal
//     rows only. Rows marked PendingReview (see review.go) are booked but
//     not effective — balances never see them — so neither does the aging
//     view, exactly like the settlement view (see settlement.go).
//   - Age is measured per entry from its CreatedAt timestamp to the
//     report's as-of time, so backdated entries (period-close backfill)
//     age by their business date, not by when they were typed in.
//   - FIFO lot attribution: the outstanding balance is attributed to the
//     oldest entries first. A payment (credit) retires the oldest lots
//     before the newer ones, so a 90-day-old invoice that was paid last
//     week does not show up as overdue — bucketing raw signed deltas
//     would. An over-applied credit (payment larger than everything owed
//     so far) parks as a negative carry at the credit's own age and is
//     consumed by later debits; the bucket sums always add up to the
//     current balance.
//   - Only (account, currency) pairs with a positive outstanding balance
//     are receivables and appear in the report. A ?account= filter forces
//     that account's row to appear even at zero or negative balance —
//     the decomposition still adds up and is useful for review.
//
// Buckets are 0-30 / 31-60 / 61-90 / 90+ days: age 30 lands in the first
// bucket, 31 in the second, 90 in the third, 91 in the fourth. A
// future-dated entry (age < 0) is clamped into the 0-30 bucket rather
// than a negative-age phantom bucket.
//
// Overdue is the 61-90 + 90+ buckets. An opt-in per-call alert threshold
// (OverdueAlertThresholdCents > 0) flags every reported account whose
// overdue cents exceed it: one aging_alert structured audit event per
// breaching (account, currency), and the report's Alerts list. The
// threshold is per-call, not stored config — like settlement tolerances,
// the collections policy changes run to run — and unlike the
// low-balance watermark (LG-37) the alert re-fires on every scan while in
// breach, because aging is a point-in-time scan, not a crossing event.
//
// This is the structural complement to the settlement view (LG-43):
// settlement aggregates one day's net movement, aging decomposes the
// outstanding stock by age.

// Aging bucket indices, oldest last.
const (
	agingBucket0_30 = iota
	agingBucket31_60
	agingBucket61_90
	agingBucket90Plus
	numAgingBuckets
)

// agingBucketLabels pins the bucket order used by reports, audit events,
// and tests.
var agingBucketLabels = [numAgingBuckets]string{"0-30", "31-60", "61-90", "90+"}

// agingBucketFor maps an age in whole days to its bucket index. Negative
// ages (future-dated entries) clamp to the youngest bucket.
func agingBucketFor(ageDays int64) int {
	switch {
	case ageDays <= 30:
		return agingBucket0_30
	case ageDays <= 60:
		return agingBucket31_60
	case ageDays <= 90:
		return agingBucket61_90
	default:
		return agingBucket90Plus
	}
}

// AgingOptions tunes one aging scan. All fields are optional.
type AgingOptions struct {
	// AsOf is the business date the ages are measured against. The zero
	// value means now (UTC), taken by the caller.
	AsOf time.Time
	// Account filters the report to one account, forcing its row to
	// appear even when its balance is not positive. Empty means all
	// accounts with positive outstanding balances.
	Account AccountID
	// OverdueAlertThresholdCents flags every reported (account, currency)
	// whose overdue (61-90 + 90+ days) cents exceed it. Zero or negative
	// disables alerting entirely.
	OverdueAlertThresholdCents int64
}

// AgingBuckets is the FIFO-attributed outstanding balance per age bucket,
// in cents. The four fields always sum to the account's current balance.
type AgingBuckets struct {
	Days0_30  int64 `json:"days_0_30_cents"`
	Days31_60 int64 `json:"days_31_60_cents"`
	Days61_90 int64 `json:"days_61_90_cents"`
	Days90Plus int64 `json:"days_90_plus_cents"`
}

// bucketsArray returns the fields in bucket order for internal math.
func (b AgingBuckets) bucketsArray() [numAgingBuckets]int64 {
	return [numAgingBuckets]int64{b.Days0_30, b.Days31_60, b.Days61_90, b.Days90Plus}
}

// AccountAging is one (account, currency) row of the aging report.
type AccountAging struct {
	Account      AccountID    `json:"account"`
	Currency     string       `json:"currency"`
	Buckets      AgingBuckets `json:"buckets"`
	TotalCents   int64        `json:"total_cents"`
	OverdueCents int64        `json:"overdue_cents"`
}

// AgingAlert is one breaching (account, currency): overdue cents above
// the scan's threshold.
type AgingAlert struct {
	Account         AccountID    `json:"account"`
	Currency        string       `json:"currency"`
	OverdueCents    int64        `json:"overdue_cents"`
	ThresholdCents  int64        `json:"threshold_cents"`
	Buckets         AgingBuckets `json:"buckets"`
}

// AgingReport is the full aging scan result.
type AgingReport struct {
	AsOf     time.Time      `json:"asof"`
	Version  uint64         `json:"version"`
	Accounts []AccountAging `json:"accounts"`
	Alerts   []AgingAlert   `json:"alerts"`
}

// WriteJSON encodes the report as indented JSON to w, the form the
// operator archives from GET /aging.
func (r AgingReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// agingDelta is one signed balance movement for an (account, currency),
// in oldest-first order.
type agingDelta struct {
	createdAt time.Time
	id        string
	cents     int64 // >0 debit leg, <0 credit leg
}

// agingLot is one outstanding FIFO lot: cents owed, aged at bucket.
type agingLot struct {
	bucket int
	cents  int64 // >0
}

// attributeAgingLots runs the FIFO lot attribution over oldest-first
// deltas and returns the per-bucket outstanding cents. The returned sums
// always equal the sum of the deltas.
func attributeAgingLots(deltas []agingDelta, asof time.Time) [numAgingBuckets]int64 {
	var out [numAgingBuckets]int64
	lots := make([]agingLot, 0, len(deltas))
	var negCarry int64 // over-applied credit waiting for future debits
	negCarryBucket := -1
	for _, d := range deltas {
		bucket := agingBucketFor(int64(asof.Sub(d.createdAt) / (24 * time.Hour)))
		switch {
		case d.cents > 0:
			amt := d.cents
			if negCarry < 0 {
				// A parked overpayment offsets the new debit first.
				if off := -negCarry; off < amt {
					amt -= off
					negCarry = 0
					negCarryBucket = -1
				} else {
					negCarry += amt
					amt = 0
				}
			}
			if amt > 0 {
				lots = append(lots, agingLot{bucket: bucket, cents: amt})
			}
		case d.cents < 0:
			amt := -d.cents
			for amt > 0 && len(lots) > 0 {
				if lots[0].cents <= amt {
					amt -= lots[0].cents
					lots = lots[1:]
				} else {
					lots[0].cents -= amt
					amt = 0
				}
			}
			if amt > 0 {
				// Payment larger than everything owed so far: park the
				// remainder as a negative carry at the payment's age.
				negCarry -= amt
				negCarryBucket = bucket
			}
		}
	}
	for _, l := range lots {
		out[l.bucket] += l.cents
	}
	if negCarry < 0 {
		out[negCarryBucket] += negCarry
	}
	return out
}

// Aging scans the journal and returns the AR aging report: per
// (account, currency) outstanding balances decomposed by age bucket,
// FIFO-attributed, plus overdue alerts when a threshold is configured.
//
// Read-only: one RLock, no version bump, no chain append. Alert audit
// events are emitted under the read lock; the enqueue never blocks (see
// emitAudit), exactly like the settlement_report event.
func (l *Ledger) Aging(now time.Time, opts AgingOptions) AgingReport {
	asof := opts.AsOf
	if asof.IsZero() {
		asof = now.UTC()
	}

	type key struct {
		account  AccountID
		currency string
	}
	deltas := make(map[key][]agingDelta)
	totals := make(map[key]int64)

	l.mu.RLock()
	defer l.mu.RUnlock()

	for _, e := range l.entries {
		// Pending-review rows are booked but not effective: balances
		// never see them, so the aging view must not either, or the
		// bucket sums would stop adding up to the balance.
		if e.PendingReview {
			continue
		}
		k := key{account: e.DebitAccount, currency: e.Currency}
		deltas[k] = append(deltas[k], agingDelta{createdAt: e.CreatedAt, id: e.ID, cents: e.AmountCents})
		totals[k] += e.AmountCents
		k = key{account: e.CreditAccount, currency: e.Currency}
		deltas[k] = append(deltas[k], agingDelta{createdAt: e.CreatedAt, id: e.ID, cents: -e.AmountCents})
		totals[k] -= e.AmountCents
	}

	report := AgingReport{
		AsOf:     asof,
		Version:  l.version,
		Accounts: make([]AccountAging, 0, len(deltas)),
		Alerts:   make([]AgingAlert, 0),
	}
	for k, ds := range deltas {
		if opts.Account != "" && k.account != opts.Account {
			continue
		}
		total := totals[k]
		if total <= 0 && (opts.Account == "" || k.account != opts.Account) {
			// Not a receivable: nothing outstanding to age.
			continue
		}
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].createdAt.Equal(ds[j].createdAt) {
				return ds[i].id < ds[j].id
			}
			return ds[i].createdAt.Before(ds[j].createdAt)
		})
		arr := attributeAgingLots(ds, asof)
		buckets := AgingBuckets{
			Days0_30:   arr[agingBucket0_30],
			Days31_60:  arr[agingBucket31_60],
			Days61_90:  arr[agingBucket61_90],
			Days90Plus: arr[agingBucket90Plus],
		}
		overdue := arr[agingBucket61_90] + arr[agingBucket90Plus]
		report.Accounts = append(report.Accounts, AccountAging{
			Account:      k.account,
			Currency:     k.currency,
			Buckets:      buckets,
			TotalCents:   total,
			OverdueCents: overdue,
		})
		if opts.OverdueAlertThresholdCents > 0 && overdue > opts.OverdueAlertThresholdCents {
			alert := AgingAlert{
				Account:        k.account,
				Currency:       k.currency,
				OverdueCents:   overdue,
				ThresholdCents: opts.OverdueAlertThresholdCents,
			}
			alert.Buckets = buckets
			report.Alerts = append(report.Alerts, alert)
			// Read-only scan, so the version brackets are identical —
			// the event is the durable record of the finding, like the
			// settlement_report event. Emitted under the read lock; the
			// enqueue never blocks.
			l.emitAudit(AuditEvent{
				Op:            "aging_alert",
				Actor:         "Aging",
				TraceID:       fmt.Sprintf("aging@%d", l.version),
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{k.account},
				Details: map[string]any{
					"currency":             k.currency,
					"overdue_cents":        overdue,
					"threshold_cents":      opts.OverdueAlertThresholdCents,
					"buckets_0_30_cents":   arr[agingBucket0_30],
					"buckets_31_60_cents":  arr[agingBucket31_60],
					"buckets_61_90_cents":  arr[agingBucket61_90],
					"buckets_90_plus_cents": arr[agingBucket90Plus],
					"asof":                 asof.Format(time.RFC3339),
				},
			})
		}
	}

	sort.Slice(report.Accounts, func(i, j int) bool {
		if report.Accounts[i].Account != report.Accounts[j].Account {
			return report.Accounts[i].Account < report.Accounts[j].Account
		}
		return report.Accounts[i].Currency < report.Accounts[j].Currency
	})
	sort.Slice(report.Alerts, func(i, j int) bool {
		if report.Alerts[i].Account != report.Alerts[j].Account {
			return report.Alerts[i].Account < report.Alerts[j].Account
		}
		return report.Alerts[i].Currency < report.Alerts[j].Currency
	})
	return report
}
