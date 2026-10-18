package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Entry links (LG-123).
//
// An EntryLink is an explicit directed association between two journal
// entries: FromEntryID is the dependent entry, ToEntryID the referenced
// entry, and LinkType names the relationship. The five link types cover
// the correction and money-flow relationships operators otherwise track
// by fragile memo-text matching:
//
//   - reversal_of:   From is the reversal of To (declared automatically
//     by ReverseEntry and ReverseEntryPartial, alongside the
//     "reversal_of:<id>" memo).
//   - adjustment_of: From adjusts To (a correcting rebooking, a fee
//     true-up, a reclassification).
//   - fee_of:        From is the fee leg of To (e.g. a transfer's
//     "<transfer ID>/fee" leg of the "<transfer ID>" principal).
//   - refund_of:     From refunds To (a refund transfer of the original
//     payment).
//   - mandate_of:    From is a direct-debit journal entry authorized by
//     To, where To is the mandate's reference record — the mandate ID,
//     not a journal entry (see LinkTypeMandateOf).
//
// Links are declared through DeclareEntryLink (or the Reversal paths,
// which declare reversal_of themselves, or DebitWithMandate, which
// declares mandate_of itself). Both endpoint entries must already be
// journaled — except mandate_of links, whose To endpoint is the
// mandate's reference record and must name an existing mandate — a link
// never references its own entry, and declaring the identical (from,
// to, type) triple twice is a silent no-op — idempotent retries record
// no second audit event, like SetExternalMapping.
//
// Links are structural, like external account mappings: declaring one
// does not bump the journal version, it round-trips through snapshots,
// and every declaration is recorded as an "entry_link_declared" audit
// event. Tamper-evidence comes from the link chain (see below): the
// declaration order, the endpoints, the type, and the declaration
// metadata are hashed into an append-only chain that VerifyChain
// recomputes, so rewriting a declared link breaks verification exactly
// like rewriting a journaled amount.
//
// The link chain is deliberately separate from the entry chain (see
// chain.go): the entry chain's "one link per Post, links == version"
// invariant is untouched, and links stay out of the JournalEntry struct
// itself — a link is an independent audit object, not an entry field.

// Link types for DeclareEntryLink.
const (
	// LinkTypeReversalOf marks From as the reversal of To.
	LinkTypeReversalOf = "reversal_of"
	// LinkTypeAdjustmentOf marks From as an adjustment of To.
	LinkTypeAdjustmentOf = "adjustment_of"
	// LinkTypeFeeOf marks From as the fee leg of To.
	LinkTypeFeeOf = "fee_of"
	// LinkTypeRefundOf marks From as the refund of To.
	LinkTypeRefundOf = "refund_of"
	// LinkTypeMandateOf marks From as a direct-debit journal entry
	// authorized by To, where To is the mandate's reference record —
	// the mandate ID, not a journal entry (LG-125, see mandate.go).
	// Mandates are structural config, like external account mappings:
	// journaling a zero-amount marker entry at mandate creation just to
	// give the link an entry-shaped To endpoint would fabricate money
	// movement, so mandate_of is the one link type whose To endpoint
	// lives in the mandate namespace. The declaration is validated
	// against the mandates registry (the mandate must exist, like a
	// journaled To endpoint must exist) and hashed into the link chain
	// exactly like the entry->entry links. Declared automatically by
	// DebitWithMandate, alongside the "mandate_of:<id>" memo.
	LinkTypeMandateOf = "mandate_of"
)

// Link directions for DirectedEntryLink: whether the queried entry is
// the link's From endpoint ("outgoing") or its To endpoint ("incoming").
const (
	LinkDirectionOutgoing = "outgoing"
	LinkDirectionIncoming = "incoming"
)

// Link declaration errors.
var (
	// ErrInvalidLinkType is returned by DeclareEntryLink when the link
	// type is not one of reversal_of, adjustment_of, fee_of, refund_of,
	// or mandate_of. It is a 400-class error: the caller named a
	// relationship the ledger does not model.
	ErrInvalidLinkType = errors.New("ledger: invalid entry link type")
	// ErrEmptyLinkEntryID is returned by DeclareEntryLink when either
	// endpoint entry ID is empty. It is a 400-class error.
	ErrEmptyLinkEntryID = errors.New("ledger: entry link entry ID must not be empty")
	// ErrLinkSelfReference is returned by DeclareEntryLink when a link
	// would reference its own entry: every link type is asymmetric (X
	// is the reversal/adjustment/fee/refund *of* Y), so a self-link is
	// meaningless. It is a 400-class error.
	ErrLinkSelfReference = errors.New("ledger: entry link cannot reference the entry itself")
)

// EntryLink is one declared directed association between two journal
// entries (see the package doc above): FromEntryID is the dependent
// entry, ToEntryID the referenced entry.
type EntryLink struct {
	FromEntryID string    `json:"from_entry_id"`
	ToEntryID   string    `json:"to_entry_id"`
	LinkType    string    `json:"link_type"`
	DeclaredAt  time.Time `json:"declared_at"`
	// DeclaredBy is the operator identity from the declaration's audit
	// context (see ActorFromContext), empty for unattributed
	// declarations such as the reversal paths' automatic links.
	DeclaredBy string `json:"declared_by,omitempty"`
}

// DirectedEntryLink is an EntryLink annotated with the direction it was
// found in: the response row of GET /entries/{id}/links.
type DirectedEntryLink struct {
	EntryLink
	// Direction is "outgoing" when the queried entry is the link's From
	// endpoint, "incoming" when it is the To endpoint.
	Direction string `json:"direction"`
}

// EntryLinkChain is one Reconcile-disclosed adjustment chain: the links
// of type reversal_of/adjustment_of expanded from a root entry, so the
// full correction history of an entry audits end to end. RootEntryID is
// the ultimate original — the entry no link points away from — and
// Links are the chain's links in declaration order.
type EntryLinkChain struct {
	RootEntryID string      `json:"root_entry_id"`
	Links       []EntryLink `json:"links"`
}

// entryLinkChainLink is one link of the tamper-evident entry-link chain:
// the append-only record of link declarations. linkChain[i] covers
// entryLinks[i]; seq is the 1-based declaration order. The chain has its
// own genesis (32 zero bytes), independent of the entry chain — the two
// chains verify separately (see verifyChainLocked), and the entry
// chain's links == version invariant is untouched by link declarations.
type entryLinkChainLink struct {
	seq      uint64
	prevHash [32]byte
	hash     [32]byte
}

// validLinkType reports whether t is one of the five declared link
// types.
func validLinkType(t string) bool {
	switch t {
	case LinkTypeReversalOf, LinkTypeAdjustmentOf, LinkTypeFeeOf, LinkTypeRefundOf, LinkTypeMandateOf:
		return true
	}
	return false
}

// validateEntryLinkFields checks a link declaration's fields without
// touching ledger state, so snapshot imports and the write path share
// the same admission gate: a link that could never have been declared
// is rejected, never installed.
func validateEntryLinkFields(from, to, linkType string) error {
	if !validLinkType(linkType) {
		return fmt.Errorf("%w: %q", ErrInvalidLinkType, linkType)
	}
	if from == "" || to == "" {
		return ErrEmptyLinkEntryID
	}
	if from == to {
		return ErrLinkSelfReference
	}
	return nil
}

// hashEntryLinkChainLink computes SHA-256(prevHash || canonical(link)):
// the length-prefixed field encoding of chain.go, so adjacent fields
// cannot be confused, with DeclaredAt as UnixNano so the encoding is
// independent of time.Location. Rewriting any declared field — the
// endpoints, the type, the declaration time or declarer — breaks the
// chain exactly like rewriting a journaled amount.
func hashEntryLinkChainLink(prevHash [32]byte, link EntryLink) [32]byte {
	h := sha256.New()
	h.Write(prevHash[:])
	writeHashString(h, link.FromEntryID)
	writeHashString(h, link.ToEntryID)
	writeHashString(h, link.LinkType)
	writeHashInt(h, link.DeclaredAt.UnixNano())
	writeHashString(h, link.DeclaredBy)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// appendEntryLinkLocked records link as a new declaration at the head of
// the entry-link chain. Callers must hold l.mu.
func (l *Ledger) appendEntryLinkLocked(link EntryLink) {
	var prevHash [32]byte
	if n := len(l.linkChain); n > 0 {
		prevHash = l.linkChain[n-1].hash
	}
	l.entryLinks = append(l.entryLinks, link)
	l.linkChain = append(l.linkChain, entryLinkChainLink{
		seq:      uint64(len(l.linkChain) + 1),
		prevHash: prevHash,
		hash:     hashEntryLinkChainLink(prevHash, link),
	})
}

// DeclareEntryLink declares an explicit directed association between two
// journaled entries: from is linkType of to (see the link-type
// constants). Both endpoint entries must already be journaled
// (ErrEntryNotFound) — except mandate_of links (see LinkTypeMandateOf),
// whose To endpoint is the mandate's reference record and must name an
// existing mandate (ErrMandateNotFound); the From endpoint is always a
// journaled entry. The link type must be one of the five declared types
// (ErrInvalidLinkType); a link never references its own entry
// (ErrLinkSelfReference).
//
// Declaring the identical (from, to, type) triple twice is a silent
// no-op: it returns the existing link with declared == false, recording
// no second audit event and appending no second chain link, so
// idempotent retries stay silent in the compliance trail. A new
// declaration returns declared == true, appends to the tamper-evident
// link chain, and records an "entry_link_declared" audit event.
//
// The declaration is structural: it does not bump the ledger version,
// so a link declaration never looks like money movement to
// version-based change detectors.
func (l *Ledger) DeclareEntryLink(from, to, linkType string) (link EntryLink, declared bool, err error) {
	return l.DeclareEntryLinkCtx(context.Background(), from, to, linkType)
}

// DeclareEntryLinkCtx is DeclareEntryLink with per-call audit actor
// attribution (LG-73): the audit event records the operator carried by
// ctx when set, and the entrypoint name otherwise; the same identity is
// stored on the link as DeclaredBy.
func (l *Ledger) DeclareEntryLinkCtx(ctx context.Context, from, to, linkType string) (EntryLink, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.declareEntryLinkLocked(ctx, from, to, linkType)
}

// declareEntryLinkLocked is the shared implementation of DeclareEntryLink
// and DeclareEntryLinkCtx. Callers must hold l.mu (the write lock).
func (l *Ledger) declareEntryLinkLocked(ctx context.Context, from, to, linkType string) (EntryLink, bool, error) {
	if err := validateEntryLinkFields(from, to, linkType); err != nil {
		return EntryLink{}, false, err
	}
	if _, ok := l.entries[from]; !ok {
		return EntryLink{}, false, fmt.Errorf("%w: %q", ErrEntryNotFound, from)
	}
	if linkType == LinkTypeMandateOf {
		// mandate_of is the one link type whose To endpoint is not a
		// journal entry but the mandate's reference record (see
		// LinkTypeMandateOf): it must name an existing mandate.
		// Mandates are never deleted, so a genuine declaration's
		// mandate is always found, like a genuine entry endpoint.
		if _, ok := l.mandates[to]; !ok {
			return EntryLink{}, false, fmt.Errorf("%w: %q", ErrMandateNotFound, to)
		}
	} else if _, ok := l.entries[to]; !ok {
		return EntryLink{}, false, fmt.Errorf("%w: %q", ErrEntryNotFound, to)
	}
	for _, existing := range l.entryLinks {
		if existing.FromEntryID == from && existing.ToEntryID == to && existing.LinkType == linkType {
			// Idempotent replay of the current declaration: nothing
			// changed, so no audit event — the trail records
			// declarations, not retries.
			return existing, false, nil
		}
	}
	link := EntryLink{
		FromEntryID: from,
		ToEntryID:   to,
		LinkType:    linkType,
		// UTC like the other audit timestamps (see denylist.go): the
		// declaration time round-trips through snapshots byte-identical.
		DeclaredAt: time.Now().UTC(),
		DeclaredBy: strings.TrimSpace(ActorFromContext(ctx)),
	}
	l.appendEntryLinkLocked(link)
	details := map[string]any{
		"from_entry_id": from,
		"to_entry_id":   to,
		"link_type":     linkType,
	}
	if link.DeclaredBy != "" {
		details["declared_by"] = link.DeclaredBy
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "entry_link_declared",
		Actor:         "DeclareEntryLink",
		TraceID:       from,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{from, to},
		Details:       details,
	})
	l.entryLinkChanges.Add(1)
	return link, true, nil
}

// EntryExists reports whether the journal contains an entry with the
// given ID.
func (l *Ledger) EntryExists(id string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.entries[id]
	return ok
}

// EntryLinks returns every declared link touching id — both directions —
// in declaration order, each annotated with the direction id was found
// in. The result is never nil: an entry with no declared links returns
// an empty slice.
func (l *Ledger) EntryLinks(id string) []DirectedEntryLink {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.entryLinksForLocked(id)
}

// entryLinksForLocked returns the bidirectional link view of id (see
// EntryLinks). Callers must hold l.mu (either lock suffices).
func (l *Ledger) entryLinksForLocked(id string) []DirectedEntryLink {
	out := make([]DirectedEntryLink, 0)
	for _, link := range l.entryLinks {
		// Self-links are rejected at declaration, so the two cases are
		// exclusive.
		switch id {
		case link.FromEntryID:
			out = append(out, DirectedEntryLink{EntryLink: link, Direction: LinkDirectionOutgoing})
		case link.ToEntryID:
			out = append(out, DirectedEntryLink{EntryLink: link, Direction: LinkDirectionIncoming})
		}
	}
	return out
}

// entryLinksLocked returns every declared link in declaration order —
// the snapshot record form and the Reconcile disclosure form of the
// registry. Callers must hold l.mu (either lock suffices).
func (l *Ledger) entryLinksLocked() []EntryLink {
	out := make([]EntryLink, len(l.entryLinks))
	copy(out, l.entryLinks)
	return out
}

// entryLinkChainsLocked expands the reversal_of/adjustment_of links into
// per-root adjustment chains (see EntryLinkChain): each chain's root is
// the ultimate original entry — found by following the From -> To edges
// to the entry no link points away from — and the chain's links are in
// declaration order. fee_of/refund_of/mandate_of links are associations,
// not corrections, so they stay out of the chains; they are still
// disclosed flat in the report's entry_links. Callers must hold l.mu
// (either lock suffices).
func (l *Ledger) entryLinkChainsLocked() []EntryLinkChain {
	// The From -> To edge map of the correction link types: the first
	// declared edge wins for root-finding, so a manually declared
	// second correction of one entry cannot re-root the chain.
	toOf := make(map[string]string)
	for _, link := range l.entryLinks {
		if link.LinkType != LinkTypeReversalOf && link.LinkType != LinkTypeAdjustmentOf {
			continue
		}
		if _, ok := toOf[link.FromEntryID]; !ok {
			toOf[link.FromEntryID] = link.ToEntryID
		}
	}
	rootOf := func(from string) string {
		seen := map[string]bool{from: true}
		for {
			to, ok := toOf[from]
			if !ok {
				return from
			}
			if seen[to] {
				// A pathological manual cycle (A corrects B corrects
				// A): stop instead of looping forever; the chain is
				// still disclosed, rooted at the cycle entry.
				return to
			}
			seen[to] = true
			from = to
		}
	}
	byRoot := make(map[string][]EntryLink)
	var roots []string
	for _, link := range l.entryLinks {
		if link.LinkType != LinkTypeReversalOf && link.LinkType != LinkTypeAdjustmentOf {
			continue
		}
		root := rootOf(link.FromEntryID)
		if _, ok := byRoot[root]; !ok {
			roots = append(roots, root)
		}
		byRoot[root] = append(byRoot[root], link)
	}
	sort.Strings(roots)
	out := make([]EntryLinkChain, 0, len(roots))
	for _, root := range roots {
		out = append(out, EntryLinkChain{RootEntryID: root, Links: byRoot[root]})
	}
	return out
}

// EntryLinkChangeCount reports how many entry links the ledger has
// declared since it started (see DeclareEntryLink): new declarations
// only — idempotent replays of an identical link are not counted.
// Operational metric (surfaced on GET /metrics as
// ledger_entry_links_total), not books: it is in-memory only and does
// not round-trip through snapshots.
func (l *Ledger) EntryLinkChangeCount() uint64 {
	return l.entryLinkChanges.Load()
}
