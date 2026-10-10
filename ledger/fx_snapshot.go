package ledger

import (
	"fmt"
	"sort"
	"time"
)

// FX rate-table snapshot versioning (LG-45).
//
// An FX receipt already discloses the rate that converted it
// (FXConversion.RateNum/RateDen, see fx.go), but that alone does not let
// an auditor answer "what did the whole rate table look like when this
// transfer posted?" — the rate may have been replaced or removed since.
// Snapshot versioning closes that gap: every mutation of the rate table
// (SetFXRate, SetFXRateRat, RemoveFXRate, and rates installed at
// construction) bumps fxSnapshotVersion and persists a full copy of the
// table at the new version. Cross-currency transfers record the snapshot
// version in effect at post time (FXConversion.RateSnapshotVersion), so
// FXRateTableSnapshot(v) reproduces exactly the table that converted any
// historical transfer.
//
// Snapshots are full-table copies, not deltas: the table is small (one
// row per directional pair), a copy is O(pairs), and reconstruction is a
// map lookup instead of a fold over history — an auditor cannot get it
// wrong. Each snapshot also records when it was taken and a short change
// note ("set USD->EUR 108/100", "remove USD->EUR", "genesis"), and every
// mutation emits an "fx_rate_change" audit event, so the version history
// is visible in the audit log as well as in the snapshots themselves.
//
// Snapshots survive ExportSnapshot/ImportSnapshot like the rate table
// itself, and Reconcile reports the version continuity (see
// FXRateVersionReport): versions 1..current must all be present. A gap
// means snapshot history was lost — the current table still converts,
// but historical reproduction for the missing versions is gone, which is
// exactly what the continuity check flags.

// FXRateTableSnapshot is one versioned copy of the FX rate table: the
// complete table as it stood right after the mutation that created this
// version. Rates is sorted by (from, to) like fxRatesLocked returns.
type FXRateTableSnapshot struct {
	// Version is the 1-based snapshot version; 0 means "no snapshot".
	Version uint64 `json:"version"`
	// TakenAt is when the mutation that created this version landed.
	TakenAt time.Time `json:"taken_at"`
	// Change is a short human note: "genesis", "set USD->EUR 108/100",
	// "set USD->EUR 11/10 ttl=24h0m0s", "remove USD->EUR".
	Change string `json:"change"`
	// Rates is the full table at this version.
	Rates []ExchangeRate `json:"rates"`
}

// FXRateVersionReport is the Reconcile view of the snapshot history:
// which version is current and whether the history is gap-free.
type FXRateVersionReport struct {
	// CurrentVersion is the latest snapshot version (0 when the rate
	// table was never populated).
	CurrentVersion uint64 `json:"current_version"`
	// TotalSnapshots is how many snapshots are stored.
	TotalSnapshots int `json:"total_snapshots"`
	// ContinuityOK is false when some version in 1..CurrentVersion is
	// missing — historical reproduction for that version is lost.
	ContinuityOK bool `json:"continuity_ok"`
	// ContinuityError names the first missing version; empty when OK.
	ContinuityError string `json:"continuity_error,omitempty"`
}

// snapshotFXRateTableLocked bumps the snapshot version and persists a
// full copy of the current rate table. Callers must hold the write lock.
// change is the short note stored on the snapshot ("set USD->EUR
// 108/100", ...); the mutation itself must already be applied to
// l.fxRates before this runs.
func (l *Ledger) snapshotFXRateTableLocked(change string) {
	l.fxSnapshotVersion++
	v := l.fxSnapshotVersion
	l.fxRateSnapshots[v] = FXRateTableSnapshot{
		Version: v,
		TakenAt: time.Now().UTC(),
		Change:  change,
		Rates:   l.fxRatesLocked(),
	}
	// The version history is visible in the audit log too: an auditor
	// can walk "fx_rate_change" events to see when each version landed,
	// and pull the snapshot for what the table contained.
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_change",
		Actor:         "FXRateTable",
		TraceID:       fmt.Sprintf("fx-snapshot@%d", v),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"fx_snapshot_version": v,
			"change":              change,
			"rate_count":          len(l.fxRates),
		},
	})
}

// FXRateSnapshotVersion returns the current FX rate-table snapshot
// version: the number of table mutations since genesis. 0 means the table
// was never populated.
func (l *Ledger) FXRateSnapshotVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxSnapshotVersion
}

// FXRateTableSnapshot returns the stored copy of the rate table at the
// given snapshot version, or false when no snapshot was kept for it.
// The returned snapshot is a copy: mutating it cannot affect the ledger.
// This is the historical-audit primitive — given a transfer receipt's
// FXConversion.RateSnapshotVersion, it reproduces exactly the table that
// converted the transfer.
func (l *Ledger) FXRateTableSnapshot(version uint64) (FXRateTableSnapshot, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	snap, ok := l.fxRateSnapshots[version]
	if !ok {
		return FXRateTableSnapshot{}, false
	}
	rates := make([]ExchangeRate, len(snap.Rates))
	copy(rates, snap.Rates)
	snap.Rates = rates
	return snap, true
}

// fxRateTableSnapshotsLocked returns every stored snapshot sorted by
// version, for snapshots and operator inspection. Callers must hold l.mu;
// the read lock suffices. The returned snapshots are copies.
func (l *Ledger) fxRateTableSnapshotsLocked() []FXRateTableSnapshot {
	out := make([]FXRateTableSnapshot, 0, len(l.fxRateSnapshots))
	for _, snap := range l.fxRateSnapshots {
		rates := make([]ExchangeRate, len(snap.Rates))
		copy(rates, snap.Rates)
		snap.Rates = rates
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// fxRateVersionReportLocked builds the Reconcile view of the snapshot
// history. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) fxRateVersionReportLocked() FXRateVersionReport {
	rep := FXRateVersionReport{
		CurrentVersion: l.fxSnapshotVersion,
		TotalSnapshots: len(l.fxRateSnapshots),
		ContinuityOK:   true,
	}
	for v := uint64(1); v <= l.fxSnapshotVersion; v++ {
		if _, ok := l.fxRateSnapshots[v]; !ok {
			rep.ContinuityOK = false
			rep.ContinuityError = fmt.Sprintf("ledger: FX rate-table snapshot version %d is missing", v)
			break
		}
	}
	return rep
}

// exchangeRateEqual compares two rates by value, ignoring the
// monotonic clock reading inside ExpiresAt: a JSON round trip drops the
// monotonic component, so == / DeepEqual would report a false
// difference on TTL rates.
func exchangeRateEqual(a, b ExchangeRate) bool {
	return a.FromCurrency == b.FromCurrency &&
		a.ToCurrency == b.ToCurrency &&
		a.Num == b.Num && a.Den == b.Den &&
		a.EffectiveVersion == b.EffectiveVersion &&
		a.ExpiresAt.Equal(b.ExpiresAt)
}

// fxSnapshotsEqualLocked reports whether two ledgers carry the same
// snapshot history: same current version and same per-version rate
// tables. TakenAt/Change are operational metadata, not conversion state,
// and are deliberately excluded — a restore reproduces the tables, not
// the wall-clock instants they were taken at. Callers must hold both
// ledgers' read locks (used inside snapshotLedgersEqual).
func fxSnapshotsEqualLocked(a, b *Ledger) bool {
	if a.fxSnapshotVersion != b.fxSnapshotVersion {
		return false
	}
	if len(a.fxRateSnapshots) != len(b.fxRateSnapshots) {
		return false
	}
	for v, sa := range a.fxRateSnapshots {
		sb, ok := b.fxRateSnapshots[v]
		if !ok || len(sa.Rates) != len(sb.Rates) {
			return false
		}
		for i := range sa.Rates {
			if !exchangeRateEqual(sa.Rates[i], sb.Rates[i]) {
				return false
			}
		}
	}
	return true
}
