package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Audit-log retention (LG-137): a bounded retention window for the
// structured audit log. Compliance regimes (PCI DSS, SOC 2, local
// banking rules) require audit trails to be kept for a defined period —
// and, symmetrically, privacy regimes require them not to be kept
// forever. LEDGER_AUDIT_RETENTION_DAYS sets the window in whole days;
// ApplyAuditRetention deletes whole audit files older than the window.
//
// WORM semantics: inside the retention window audit files are
// append-only — the writer never rewrites a line in place, rotation
// only ever adds files (see audit.go). Retention therefore deletes
// whole files and never modifies them; anything younger than the cutoff
// is untouched.
//
// Deleting old files truncates the verifiable hash chain: the oldest
// retained file's first entry still links the deleted history's last
// seal, so VerifyAuditLog reports a prev_mismatch at that boundary.
// That break is the expected, documented signal of a retention cut —
// not tampering — and the cut itself is recorded in the trail (see
// ApplyAuditRetention), including each deleted file's last seal, so an
// auditor can see exactly where the chain was cut.

// ParseAuditRetentionDays parses LEDGER_AUDIT_RETENTION_DAYS: an integer
// number of days. Empty means 0 (unlimited retention). A negative or
// non-integer value is an error — main.go fails startup fast on it,
// because a misconfigured retention must never silently run (or
// silently keep everything).
func ParseAuditRetentionDays(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("ledger: audit retention days: %q is not an integer number of days", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("ledger: audit retention days: %d is negative, want 0 (unlimited) or a positive number of days", n)
	}
	return n, nil
}

// WithAuditLogRetentionDays configures the audit-log retention window in
// whole days (LG-137). 0 — the default — keeps everything. A negative
// value makes NewAuditLog fail: negative retention is a configuration
// error, not "delete everything".
func WithAuditLogRetentionDays(n int) AuditLogOption {
	return func(c *auditLogConfig) {
		c.retentionDays = n
	}
}

// RetentionDays reports the configured retention window in days, or 0
// for unlimited retention.
func (a *AuditLog) RetentionDays() int { return a.retentionDays }

// RetentionDeletions reports how many audit-log files the retention
// sweep has deleted on this log. The HTTP layer syncs it into
// ledger_audit_retention_deletions_total on every GET /metrics scrape.
func (a *AuditLog) RetentionDeletions() uint64 { return a.retentionDeletions.Load() }

// ApplyAuditRetention deletes whole audit files whose UTC day is older
// than the retention window and returns the deleted files' base names.
// A non-positive retention (unlimited) deletes nothing and returns nil.
//
// The cutoff is (today - retentionDays) in UTC; a file is deleted only
// when its day — parsed from its audit-YYYY-MM-DD*.jsonl(.gz) name — is
// strictly older. Files whose day cannot be parsed are never deleted,
// and today's active file is never a candidate (skipped explicitly, not
// just by the arithmetic).
//
// Before deleting anything, the sweep records an audit_retention_deleted
// event in the trail (deleted_files, cutoff_day, retention_days, plus
// each deleted file's last sealed hash so the cut is auditable). The
// event lands in today's active file, which is never deleted, so the
// record cannot delete itself. If the event cannot even be enqueued
// (log closed or queue full) the sweep aborts and deletes nothing — an
// unaudited deletion must not happen. Enqueue is the synchronous fail
// point; once enqueued the event rides the normal async writer like
// every other audit event.
//
// Concurrency with the background writer: the writer only ever touches
// today's file and the file it just rotated (gzipping it in the
// background), while the sweep only deletes files strictly older than
// the cutoff — a retention of 2+ days keeps the sweep a full day away
// from the rotation window. At retentionDays == 1 the sweep can race
// the background gzip of yesterday's file around the UTC midnight
// boundary; the race is benign (gzipFile reads through its open fd, and
// the worst case is a file surviving one sweep to be caught by the
// next), but operators who want a quiet boundary should use >= 2 days.
//
// The sweep is idempotent: rerunning it deletes whatever an earlier run
// left behind and reports only what it deleted this time.
func (a *AuditLog) ApplyAuditRetention(now time.Time) (deleted []string, err error) {
	if a.retentionDays <= 0 {
		return nil, nil
	}
	cutoffDay := now.UTC().Truncate(24 * time.Hour).AddDate(0, 0, -a.retentionDays).Format("2006-01-02")
	today := now.UTC().Format("2006-01-02")

	files, err := listAuditFiles(a.dir)
	if err != nil {
		return nil, err
	}
	// Collect the logical files to delete, keyed by their base path
	// (the .jsonl path): a rotation caught mid-flight can leave both
	// the plain and the gzipped copy, and both go.
	seen := make(map[string]bool)
	var bases []string
	for _, path := range files {
		day, _, ok := parseAuditFilename(filepath.Base(path))
		if !ok {
			continue // never delete what we cannot date
		}
		if day >= cutoffDay || day == today {
			continue
		}
		base := strings.TrimSuffix(path, ".gz")
		if !seen[base] {
			seen[base] = true
			bases = append(bases, base)
		}
	}
	if len(bases) == 0 {
		return nil, nil
	}
	sort.Strings(bases)

	names := make([]string, len(bases))
	seals := make(map[string]string, len(bases))
	for i, base := range bases {
		names[i] = filepath.Base(base)
		seals[names[i]] = lastSealedHash(base + ".gz")
		if seals[names[i]] == "" {
			seals[names[i]] = lastSealedHash(base)
		}
	}
	if !a.Log(AuditEvent{
		Op:      "audit_retention_deleted",
		Actor:   "ApplyAuditRetention",
		TraceID: "audit-retention",
		Details: map[string]any{
			"deleted_files":  names,
			"cutoff_day":     cutoffDay,
			"retention_days": a.retentionDays,
			// last_seals attests the chain at the cut: each deleted
			// file's last sealed hash ("" when the file held no
			// sealed entries). Deleting old files truncates the
			// verifiable hash chain at these seals — the expected
			// prev_mismatch VerifyAuditLog reports at the oldest
			// retained file is the retention cut, not tampering.
			"last_seals": seals,
		},
	}) {
		return nil, fmt.Errorf("ledger: audit retention: failed to record the deletion event; refusing to delete %d file(s)", len(bases))
	}

	for _, base := range bases {
		if err := os.Remove(base + ".gz"); err != nil && !os.IsNotExist(err) {
			return deleted, fmt.Errorf("ledger: audit retention: remove %s: %w", base+".gz", err)
		}
		if err := os.Remove(base); err != nil && !os.IsNotExist(err) {
			return deleted, fmt.Errorf("ledger: audit retention: remove %s: %w", base, err)
		}
		deleted = append(deleted, filepath.Base(base))
	}
	// Count every deleted file once, like the other ledger-owned
	// counters: the HTTP layer syncs this into
	// ledger_audit_retention_deletions_total on every /metrics scrape.
	a.retentionDeletions.Add(uint64(len(deleted)))
	return deleted, nil
}
