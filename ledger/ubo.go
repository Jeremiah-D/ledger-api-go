package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// UBO/受益所有人登记与穿透 (LG-189, 支付 fintech 合规).
//
// Every account may declare its beneficial-ownership structure: the
// natural persons (and intermediate legal entities) that own it, with
// each direct owner's share in basis points. ResolveUbos penetrates
// multi-layer chains down to the ultimate natural persons and flags
// the ones whose effective share reaches the ledger-wide
// substantive-control threshold (default 25% = 2500bp, configurable
// via SetUboControlThreshold).
//
// This module is orthogonal to kyc.go by construction: KYC identifies
// the account holder (尽调, "who is the counterparty"); UBO maps the
// ownership graph behind it ("who ultimately owns the counterparty").
// A KYC profile never implies an ownership declaration and vice
// versa — the two registries share nothing but the account key.
//
// Shares are basis points (1..10000 per link, links per entity summing
// to at most 10000 = 100%), so no floats ever enter the books.
// Penetration multiplies shares level by level with round-half-up at
// each step: a reported 5000bp is exact to the basis point up to that
// rounding.
//
// Fail-closed: a declaration whose links sum past 100% is refused, a
// penetration that revisits an account is refused as a cycle (an
// ownership graph is a DAG — a cycle is corrupt data, not a deeper
// truth), and penetration stops at maxUboDepth. An entity-kind owner
// with no declaration of its own is reported as unresolved, not
// silently dropped — the resolution says what it could not see.
//
// Like the KYC profiles, declarations are structural config: they do
// not bump the journal version, every change lands in the audit chain
// ("ubo_set" / "ubo_threshold_set"), and they round-trip through
// disaster-recovery snapshots.

// UBO owner kinds: a natural person is a terminal node; a legal
// entity is penetrated through its own declaration.
type UboOwnerKind string

const (
	// UboOwnerPerson is a natural-person owner: the penetration stops
	// here and the person accumulates effective share.
	UboOwnerPerson UboOwnerKind = "person"
	// UboOwnerEntity is a legal-entity owner: the penetration recurses
	// into the declaration registered under the owner's account ID.
	UboOwnerEntity UboOwnerKind = "entity"
)

// DefaultUboControlThresholdBP is the substantive-control threshold in
// basis points: a natural person whose penetrated effective share
// reaches 25% counts as a controlling beneficial owner.
const DefaultUboControlThresholdBP int64 = 2500

// maxUboDepth caps the penetration walk: deeper than this is either a
// pathological chain or a cycle the visited-set somehow missed.
const maxUboDepth = 32

// UboOwner is one direct ownership link of an entity account: Name is
// the natural person's name for kind person, or the owning entity's
// account ID for kind entity. ShareBP is the owner's share of the
// declared account in basis points (1..10000).
type UboOwner struct {
	Name    string       `json:"name"`
	Kind    UboOwnerKind `json:"kind"`
	ShareBP int64        `json:"share_bp"`
}

// UboDeclaration is one account's declared direct-ownership structure.
type UboDeclaration struct {
	Account   AccountID  `json:"account"`
	Owners    []UboOwner `json:"owners"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
}

// ResolvedUbo is one ultimate natural person (or one unresolvable
// entity) from the penetration, with the effective share in basis
// points of the resolved account.
type ResolvedUbo struct {
	Name        string `json:"name"`
	EffectiveBP int64  `json:"effective_share_bp"`
	// Controlling is true when the effective share reaches the
	// ledger's substantive-control threshold.
	Controlling bool `json:"controlling"`
	// Unresolved is true for an entity-kind owner with no declaration:
	// the penetration could not see through it, and the effective
	// share is the share held at the level it was found.
	Unresolved bool `json:"unresolved,omitempty"`
}

// UboResolution is the full penetration report for one account.
type UboResolution struct {
	Account     AccountID    `json:"account"`
	ThresholdBP int64        `json:"threshold_bp"`
	Owners      []ResolvedUbo `json:"owners"`
}

// UBO validation and resolution errors.
var (
	// ErrUboEmptyAccount is returned by SetUbo for an empty account.
	// 400-class field validation.
	ErrUboEmptyAccount = errors.New("ledger: UBO declaration account must not be empty")
	// ErrUboInvalidOwner is returned by SetUbo for a malformed owner
	// link: empty name, unknown kind, a share outside 1..10000bp, or a
	// duplicated (name, kind). 400-class field validation.
	ErrUboInvalidOwner = errors.New("ledger: UBO owner is invalid")
	// ErrUboSharesOverflow is returned by SetUbo when the declared
	// links sum past 10000bp: an entity cannot be more than wholly
	// owned. 422-class semantic rejection.
	ErrUboSharesOverflow = errors.New("ledger: UBO ownership shares exceed 100%")
	// ErrUboCycle is returned by ResolveUbos when the penetration
	// revisits an account: ownership graphs are DAGs, a cycle is
	// corrupt data. 422-class semantic rejection.
	ErrUboCycle = errors.New("ledger: UBO ownership chain contains a cycle")
	// ErrUboTooDeep is returned by ResolveUbos past maxUboDepth.
	// 422-class semantic rejection.
	ErrUboTooDeep = errors.New("ledger: UBO ownership chain exceeds the penetration depth limit")
	// ErrUboInvalidThreshold is returned by SetUboControlThreshold for
	// a threshold outside 1..10000bp. 400-class field validation.
	ErrUboInvalidThreshold = errors.New("ledger: UBO control threshold must be between 1 and 10000 basis points")
)

// validateUboOwners checks one declaration's links: every link needs a
// name, a known kind, a share in 1..10000bp, and the links must not
// sum past 100% or repeat a (name, kind).
func validateUboOwners(owners []UboOwner) error {
	var total int64
	seen := make(map[string]bool, len(owners))
	for i, o := range owners {
		if strings.TrimSpace(o.Name) == "" {
			return fmt.Errorf("%w: owner %d has an empty name", ErrUboInvalidOwner, i)
		}
		if o.Kind != UboOwnerPerson && o.Kind != UboOwnerEntity {
			return fmt.Errorf("%w: owner %q has unknown kind %q", ErrUboInvalidOwner, o.Name, o.Kind)
		}
		if o.ShareBP < 1 || o.ShareBP > 10000 {
			return fmt.Errorf("%w: owner %q share %d is outside 1..10000bp", ErrUboInvalidOwner, o.Name, o.ShareBP)
		}
		key := string(o.Kind) + "\x00" + o.Name
		if seen[key] {
			return fmt.Errorf("%w: owner %q is declared twice", ErrUboInvalidOwner, o.Name)
		}
		seen[key] = true
		var ok bool
		if total, ok = addCents(total, o.ShareBP); !ok {
			return fmt.Errorf("%w: shares overflow", ErrUboSharesOverflow)
		}
	}
	if total > 10000 {
		return fmt.Errorf("%w: declared %dbp", ErrUboSharesOverflow, total)
	}
	return nil
}

// SetUbo installs (or replaces) the UBO ownership declaration of
// account a. Replacing the declaration wholesale is the honest unit:
// beneficial ownership is a point-in-time statement, and the audit
// chain keeps every superseded version. An empty owner list is a
// valid declaration — "no beneficial owners declared" — distinct from
// no declaration at all. Setting a declaration does not bump the
// journal version: balances are unchanged by a compliance marking.
func (l *Ledger) SetUbo(a AccountID, owners []UboOwner) error {
	return l.SetUboCtx(context.Background(), a, owners)
}

// SetUboCtx is SetUbo with per-call audit actor attribution (LG-73).
func (l *Ledger) SetUboCtx(ctx context.Context, a AccountID, owners []UboOwner) error {
	if a == "" {
		return ErrUboEmptyAccount
	}
	if err := validateUboOwners(owners); err != nil {
		return err
	}
	copied := make([]UboOwner, len(owners))
	copy(copied, owners)
	decl := UboDeclaration{Account: a, Owners: copied, UpdatedAt: time.Now().UTC()}
	var total int64
	for _, o := range copied {
		total += o.ShareBP
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ubo[a] = decl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "ubo_set",
		Actor:         "SetUbo",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"owner_count":    len(copied),
			"total_share_bp": total,
		},
	})
	return nil
}

// GetUbo returns the UBO declaration of account a. The second return
// value is false for accounts with no declaration.
func (l *Ledger) GetUbo(a AccountID) (UboDeclaration, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	decl, ok := l.ubo[a]
	if !ok {
		return UboDeclaration{}, false
	}
	copied := make([]UboOwner, len(decl.Owners))
	copy(copied, decl.Owners)
	decl.Owners = copied
	return decl, true
}

// SetUboControlThreshold sets the ledger-wide substantive-control
// threshold in basis points (1..10000): a natural person whose
// penetrated effective share reaches it counts as a controlling
// beneficial owner. The default is DefaultUboControlThresholdBP (25%).
func (l *Ledger) SetUboControlThreshold(bp int64) error {
	return l.SetUboControlThresholdCtx(context.Background(), bp)
}

// SetUboControlThresholdCtx is SetUboControlThreshold with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetUboControlThresholdCtx(ctx context.Context, bp int64) error {
	if bp < 1 || bp > 10000 {
		return ErrUboInvalidThreshold
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.uboThresholdBP
	l.uboThresholdBP = bp
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "ubo_threshold_set",
		Actor:         "SetUboControlThreshold",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"old_threshold_bp": old,
			"new_threshold_bp": bp,
		},
	})
	return nil
}

// UboControlThreshold returns the ledger-wide substantive-control
// threshold in basis points.
func (l *Ledger) UboControlThreshold() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.uboThresholdBP
}

// uboShareBelow multiplies a parent-level effective share by one
// link's share with round-half-up: eff(child) = round(eff(parent) *
// shareBP / 10000). Both operands fit in 16 bits, so the product fits
// int64 with room to spare.
func uboShareBelow(parentBP, shareBP int64) int64 {
	return (parentBP*shareBP + 5000) / 10000
}

// ResolveUbos penetrates the ownership chain of account a down to the
// ultimate natural persons and reports each one's effective share in
// basis points of a, flagging the ones that reach the control
// threshold. Entity-kind owners recurse into their own declarations;
// an entity with no declaration is reported as unresolved rather than
// silently dropped. A revisit of an account on the current path is a
// cycle and fails the whole resolution: a half-penetrated report
// would understate control.
//
// An account with no declaration resolves to an empty owner list —
// "nothing declared", not an error. The walk is deterministic: owners
// report sorted by name.
func (l *Ledger) ResolveUbos(a AccountID) (UboResolution, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	res := UboResolution{Account: a, ThresholdBP: l.uboThresholdBP}
	if _, ok := l.ubo[a]; !ok {
		return res, nil
	}
	type acc struct {
		bp         int64
		unresolved bool
	}
	byName := map[string]*acc{}
	var visit func(acct AccountID, effBP int64, path map[AccountID]bool, depth int) error
	visit = func(acct AccountID, effBP int64, path map[AccountID]bool, depth int) error {
		if depth > maxUboDepth {
			return fmt.Errorf("%w at account %q", ErrUboTooDeep, acct)
		}
		if path[acct] {
			return fmt.Errorf("%w at account %q", ErrUboCycle, acct)
		}
		d, ok := l.ubo[acct]
		if !ok {
			return nil
		}
		path[acct] = true
		defer delete(path, acct)
		for _, o := range d.Owners {
			portion := uboShareBelow(effBP, o.ShareBP)
			if o.Kind == UboOwnerPerson {
				e := byName[o.Name]
				if e == nil {
					e = &acc{}
					byName[o.Name] = e
				}
				sum, ok := addCents(e.bp, portion)
				if !ok {
					return fmt.Errorf("ledger: UBO resolution share overflow for %q", o.Name)
				}
				e.bp = sum
				continue
			}
			if _, ok := l.ubo[AccountID(o.Name)]; !ok {
				e := byName[o.Name]
				if e == nil {
					e = &acc{}
					byName[o.Name] = e
				}
				e.unresolved = true
				sum, ok := addCents(e.bp, portion)
				if !ok {
					return fmt.Errorf("ledger: UBO resolution share overflow for %q", o.Name)
				}
				e.bp = sum
				continue
			}
			if err := visit(AccountID(o.Name), portion, path, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(a, 10000, map[AccountID]bool{}, 0); err != nil {
		return UboResolution{}, err
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := byName[n]
		res.Owners = append(res.Owners, ResolvedUbo{
			Name:        n,
			EffectiveBP: e.bp,
			Controlling: !e.unresolved && e.bp >= res.ThresholdBP,
			Unresolved:  e.unresolved,
		})
	}
	return res, nil
}

// uboDeclarationsLocked returns every recorded UBO declaration, sorted
// by account, for the snapshot export. Callers must hold l.mu.
func (l *Ledger) uboDeclarationsLocked() []UboDeclaration {
	if len(l.ubo) == 0 {
		return nil
	}
	accts := make([]AccountID, 0, len(l.ubo))
	for a := range l.ubo {
		accts = append(accts, a)
	}
	sort.Slice(accts, func(i, j int) bool { return accts[i] < accts[j] })
	out := make([]UboDeclaration, 0, len(accts))
	for _, a := range accts {
		d := l.ubo[a]
		owners := make([]UboOwner, len(d.Owners))
		copy(owners, d.Owners)
		d.Owners = owners
		out = append(out, d)
	}
	return out
}
