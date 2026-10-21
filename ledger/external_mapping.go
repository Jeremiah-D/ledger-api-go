package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// External account mappings (LG-122).
//
// An ExternalAccountMapping is an explicit link between one internal
// ledger account and one identifier that an external system knows it by:
// a core-banking account number, a PSP merchant number, the account
// identifier a bank statement export must name. The mapping is one
// internal account to many external identifiers, and it is keyed by the
// (account, system, externalID) triple: the same account can appear in
// the "bank" system under a settlement account number and in the "psp"
// system under a merchant number, and one system can hold several
// identifiers for the same account (a fallback settlement account, a
// second merchant number) distinguished by externalID.
//
// System names are 1-64 characters of [A-Za-z0-9._-] (the shared
// tag-segment charset, see checkTagSegment), so they stay safe in URLs
// and JSON without escaping rules. Well-known systems:
//   - "bank": the account's identifier at a core-banking / settlement
//     bank (a local account number or IBAN).
//   - "psp": the account's merchant number at a payment service
//     provider.
//   - "ofx": the account identifier OFX statement exports name in
//     <ACCTID> (see ExportOFX) — the identifier the importing bank or
//     accounting package expects, which may differ from the internal
//     account ID.
//   - "camt053": the account identifier camt.053 statement exports
//     name in Stmt/Acct (see ExportCAMT053) — the identifier the bank
//     expects for the account (usually the IBAN), which may differ
//     from the internal account ID.
//
// Mappings are structural config, like freezes, tags, and aliases: they
// do not bump the journal version, they round-trip through snapshots
// (see snapshot.go), and every change is recorded as a structured audit
// event — "external_mapping_set" on set/upsert, "external_mapping_removed"
// on remove. Setting the identical row twice is a silent no-op, like
// SetAlias: idempotent retries record no second audit event.
//
// File-export resolution: an export that must name exactly one external
// identifier for an account in a system (see ExportOFX) resolves through
// ExternalAccountID, which picks the deterministic first row — the
// smallest externalID in the system. A system with several identifiers
// per account therefore behaves predictably in exports; there is no
// "preferred" flag, on purpose: preference is an operational decision
// the caller encodes by removing the rows it does not want resolved.

// Validation errors for external account mappings.
var (
	// ErrInvalidExternalMappingSystem is returned by
	// SetExternalMapping when the system name is empty or outside the
	// 1-64 character [A-Za-z0-9._-] charset.
	ErrInvalidExternalMappingSystem = errors.New("ledger: external mapping system must be 1-64 characters of [A-Za-z0-9._-]")
	// ErrEmptyExternalMappingID is returned by SetExternalMapping when
	// the external identifier is empty.
	ErrEmptyExternalMappingID = errors.New("ledger: external mapping external ID must not be empty")
	// ErrExternalMappingIDTooLong is returned by SetExternalMapping
	// when the external identifier exceeds
	// maxExternalMappingIDRunes UTF-8 characters.
	ErrExternalMappingIDTooLong = errors.New("ledger: external mapping external ID must be at most 128 UTF-8 characters")
	// ErrExternalMappingNoteTooLong is returned by SetExternalMapping
	// when the note exceeds maxExternalMappingNoteRunes UTF-8
	// characters.
	ErrExternalMappingNoteTooLong = errors.New("ledger: external mapping note must be at most 255 UTF-8 characters")
)

// maxExternalMappingIDRunes caps an external identifier at 128 UTF-8
// characters: enough for a long PSP merchant reference plus a branch
// suffix, short enough to keep config rows bounded.
const maxExternalMappingIDRunes = 128

// maxExternalMappingNoteRunes caps a mapping note at 255 UTF-8
// characters, mirroring the journal memo cap: enough for an operator's
// routing note, short enough to keep the audit chain bounded.
const maxExternalMappingNoteRunes = 255

// ExternalSystemOFX is the well-known system name for the account
// identifier OFX statement exports name in <ACCTID> (see ExportOFX):
// the identifier the importing bank or accounting package expects for
// the account, which may differ from the internal account ID. When an
// account has no mapping in this system, exports name the internal
// account ID as before.
const ExternalSystemOFX = "ofx"

// ExternalSystemCAMT053 is the well-known system name for the account
// identifier camt.053 statement exports name in Stmt/Acct (see
// ExportCAMT053): the identifier the bank expects for the account
// (usually the IBAN), which may differ from the internal account ID.
// When an account has no mapping in this system, exports name the
// internal account ID as before.
const ExternalSystemCAMT053 = "camt053"

// ExternalSystemMT940 is the well-known system name for the account
// identifier MT940 statement exports name in :25: (see ExportMT940):
// the identifier the bank expects for the account (usually the IBAN),
// which may differ from the internal account ID. When an account has
// no mapping in this system, exports name the internal account ID as
// before.
const ExternalSystemMT940 = "mt940"

// ExternalAccountMapping is one row of the external account mapping
// (see the package doc above): the internal Account, the external System
// that knows it by ExternalID, and an optional human Note (which system
// the identifier routes to, who provisioned it, a decommission date).
type ExternalAccountMapping struct {
	Account    AccountID `json:"account"`
	System     string    `json:"system"`
	ExternalID string    `json:"external_id"`
	Note       string    `json:"note,omitempty"`
}

// externalMappingKey is the composite storage key of one mapping row:
// (account, system, externalID).
type externalMappingKey struct {
	account    AccountID
	system     string
	externalID string
}

// validateExternalMapping checks one mapping row's fields without
// touching ledger state, so snapshot imports and the write path share
// the same admission gate: a row that could never have been created is
// rejected, never installed.
func validateExternalMapping(account AccountID, system, externalID, note string) error {
	if account == "" {
		return ErrEmptyAccountID
	}
	if err := checkTagSegment(system, ErrInvalidExternalMappingSystem); err != nil {
		return err
	}
	if externalID == "" {
		return ErrEmptyExternalMappingID
	}
	if utf8.RuneCountInString(externalID) > maxExternalMappingIDRunes {
		return fmt.Errorf("%w: %d runes", ErrExternalMappingIDTooLong, utf8.RuneCountInString(externalID))
	}
	if utf8.RuneCountInString(note) > maxExternalMappingNoteRunes {
		return fmt.Errorf("%w: %d runes", ErrExternalMappingNoteTooLong, utf8.RuneCountInString(note))
	}
	return nil
}

// SetExternalMapping declares (or updates) the mapping of account in
// system to externalID, with an optional operator note. Setting a row
// that already exists with the identical note is a silent no-op — it
// returns nil without writing an audit event, so idempotent retries
// stay silent in the compliance trail. Updating the note, or declaring
// a new row, records an "external_mapping_set" audit event and bumps
// the external-mapping change counter.
//
// The declaration is structural: it does not bump the ledger version,
// so a mapping change never looks like money movement to version-based
// change detectors.
func (l *Ledger) SetExternalMapping(account AccountID, system, externalID, note string) error {
	return l.SetExternalMappingCtx(context.Background(), account, system, externalID, note)
}

// SetExternalMappingCtx is SetExternalMapping with per-call audit actor
// attribution (LG-73): the audit event records the operator carried by
// ctx when set, and the entrypoint name otherwise.
func (l *Ledger) SetExternalMappingCtx(ctx context.Context, account AccountID, system, externalID, note string) error {
	if err := validateExternalMapping(account, system, externalID, note); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := externalMappingKey{account: account, system: system, externalID: externalID}
	if old, ok := l.externalMappings[key]; ok && old.Note == note {
		// Idempotent replay of the current row: nothing changed, so no
		// audit event — the trail records changes, not retries.
		return nil
	}
	l.externalMappings[key] = ExternalAccountMapping{
		Account:    account,
		System:     system,
		ExternalID: externalID,
		Note:       note,
	}
	details := map[string]any{
		"system":      system,
		"external_id": externalID,
	}
	if note != "" {
		details["note"] = note
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "external_mapping_set",
		Actor:         "SetExternalMapping",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details:       details,
	})
	l.externalMappingChanges.Add(1)
	return nil
}

// RemoveExternalMapping deletes the mapping of account in system to
// externalID. It reports whether a row was actually deleted: removing
// an undeclared row is a no-op returning false, writing no audit event
// and bumping no counter, so teardown paths stay idempotent. A deletion
// records an "external_mapping_removed" audit event.
//
// Like SetExternalMapping, removal is structural: it does not bump the
// ledger version.
func (l *Ledger) RemoveExternalMapping(account AccountID, system, externalID string) bool {
	return l.RemoveExternalMappingCtx(context.Background(), account, system, externalID)
}

// RemoveExternalMappingCtx is RemoveExternalMapping with per-call audit
// actor attribution (LG-73).
func (l *Ledger) RemoveExternalMappingCtx(ctx context.Context, account AccountID, system, externalID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := externalMappingKey{account: account, system: system, externalID: externalID}
	if _, ok := l.externalMappings[key]; !ok {
		return false
	}
	delete(l.externalMappings, key)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "external_mapping_removed",
		Actor:         "RemoveExternalMapping",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"system":      system,
			"external_id": externalID,
		},
	})
	l.externalMappingChanges.Add(1)
	return true
}

// ListExternalMappings returns every mapping declared for account,
// sorted by (system, external_id). The result is never nil — an account
// with no declared mappings returns an empty slice.
func (l *Ledger) ListExternalMappings(account AccountID) []ExternalAccountMapping {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ExternalAccountMapping, 0)
	for k, m := range l.externalMappings {
		if k.account == account {
			out = append(out, m)
		}
	}
	sortExternalMappingsBySystemID(out)
	return out
}

// ExternalAccountID resolves the single external identifier file exports
// must name for account in system (see the package doc): the smallest
// externalID declared for (account, system), or false when the account
// has no mapping in that system. Callers fall back to the internal
// account ID — see ExportOFX — so an unmapped account exports with the
// identifier it always had.
func (l *Ledger) ExternalAccountID(account AccountID, system string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.externalAccountIDLocked(account, system)
}

// externalAccountIDLocked resolves like ExternalAccountID. Callers must
// hold l.mu (either lock suffices).
func (l *Ledger) externalAccountIDLocked(account AccountID, system string) (string, bool) {
	best := ""
	found := false
	for k := range l.externalMappings {
		if k.account != account || k.system != system {
			continue
		}
		if !found || k.externalID < best {
			best = k.externalID
			found = true
		}
	}
	return best, found
}

// externalMappingsLocked returns every declared mapping, sorted by
// (account, system, external_id) — the snapshot record form and the
// Reconcile disclosure form of the registry. Callers must hold l.mu
// (either lock suffices; export holds the read lock).
func (l *Ledger) externalMappingsLocked() []ExternalAccountMapping {
	out := make([]ExternalAccountMapping, 0, len(l.externalMappings))
	for _, m := range l.externalMappings {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].System != out[j].System {
			return out[i].System < out[j].System
		}
		return out[i].ExternalID < out[j].ExternalID
	})
	return out
}

// sortExternalMappingsBySystemID sorts mappings by (system, external_id)
// for the per-account list form. It is a package-level helper so the
// HTTP layer and tests can share the ordering contract.
func sortExternalMappingsBySystemID(out []ExternalAccountMapping) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return out[i].System < out[j].System
		}
		return out[i].ExternalID < out[j].ExternalID
	})
}

// ExternalMappingChangeCount reports how many external account mapping
// changes the ledger has applied since it started (see SetExternalMapping
// and RemoveExternalMapping): rows declared or updated plus rows
// removed. Idempotent replays of an unchanged row are not counted.
// Operational metric (surfaced on GET /metrics as
// ledger_external_mapping_changes_total), not books: it is in-memory
// only and does not round-trip through snapshots.
func (l *Ledger) ExternalMappingChangeCount() uint64 {
	return l.externalMappingChanges.Load()
}
