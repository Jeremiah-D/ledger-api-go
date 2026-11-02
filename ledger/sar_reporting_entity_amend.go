package ledger

import (
	"context"
)

// SAR reporting-entity amendment (LG-295).
//
// WithSARReportingEntity (sar.go) installs the reporting entity — the
// goAML filing institution stamped on SAR drafts — at construction,
// defaulting to empty, which omits the field. This module is the
// orthogonal runtime path: an operator revises the entity in place,
// instead of rebuilding the ledger and replaying its journal. Revising
// in place matters because a rebuild is a new ledger — a gap in which
// the old entity (or none) governs stamping, plus a fresh identity
// that breaks the audit story — while an amendment keeps the same
// ledger, the same journal, and the same audit trail from old entity
// to new.
//
// Semantics, deliberately fail-closed throughout (along LG-241):
//
//   - The revision governs only subsequent DraftSAR calls: DraftSAR
//     reads the live l.sarCfg.reportingEntity under l.mu at draft
//     time (sar.go:437), so a draft assembled after the amendment
//     is stamped with the new entity, while a draft assembled
//     before it keeps the entity it was stamped with. No draft path
//     needed a code change — the revision is clean exactly because
//     the reader was already live.
//   - Already-drafted and already-filed reports are immutable:
//     filing seals a report with a SHA-256 content hash (LG-188)
//     and there is no update path, so revising the entity never
//     rewrites a filed report's stamping, reason codes, sightings,
//     or hash. A report that needs the new entity is re-drafted,
//     not amended.
//   - Any string is legal, including the empty string, which
//     restores the default omission: the entity is a label, not a
//     detector tuning, so there is no range to validate and no
//     construction-time panic to mirror with an error. The
//     amendment therefore never fails on the value itself.
//   - Revising to the entity already in force is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, but it lands one sar_reporting_entity_amended audit
//     event carrying the previous and new entity. The entity does
//     NOT ride snapshots — snapshot.go carries no sarCfg line — so
//     a restored ledger comes back on the empty default and
//     operators must re-set any amended entity after a restore.
//
// Orthogonality: WithSARReportingEntity owns the construction-time
// stamping, DraftSAR owns drafting (reading the live entity),
// FileSAR owns the immutable filing, and this module owns only the
// runtime revision of the entity itself. There is no HTTP endpoint
// for the revision (per LG-241/242).

// SetSARReportingEntity replaces the reporting entity stamped on SAR
// drafts in place. Only DraftSAR calls made after the call stamp the
// new entity; already-drafted and already-filed reports are
// untouched. The empty string is legal and restores the default
// omission of the field. Revising to the current entity is a no-op.
// The amendment is audited as sar_reporting_entity_amended.
func (l *Ledger) SetSARReportingEntity(entity string) error {
	return l.SetSARReportingEntityCtx(context.Background(), entity)
}

// SetSARReportingEntityCtx is SetSARReportingEntity with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetSARReportingEntityCtx(ctx context.Context, entity string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.sarCfg.reportingEntity
	if previous == entity {
		return nil
	}
	l.sarCfg.reportingEntity = entity
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sar_reporting_entity_amended",
		Actor:         "SetSARReportingEntity",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_entity": previous,
			"entity":          entity,
		},
	})
	return nil
}
