package ledger

import (
	"errors"
	"testing"
)

func mustPvP(t *testing.T, id string) *PvPSettlement {
	t.Helper()
	s, err := NewPvPSettlement(id,
		PvPLeg{LegID: "usd-leg", Account: "alice-usd", Currency: "USD", AmountCents: 100000},
		PvPLeg{LegID: "eur-leg", Account: "bob-eur", Currency: "EUR", AmountCents: 92000},
	)
	if err != nil {
		t.Fatalf("NewPvPSettlement: %v", err)
	}
	return s
}

func TestPvPValidation(t *testing.T) {
	usd := PvPLeg{LegID: "usd-leg", Account: "alice-usd", Currency: "USD", AmountCents: 100000}
	eur := PvPLeg{LegID: "eur-leg", Account: "bob-eur", Currency: "EUR", AmountCents: 92000}
	cases := []struct {
		name string
		id   string
		a, b PvPLeg
		want error
	}{
		{"empty settlement ID", "", usd, eur, ErrPvPInvalidSettlement},
		{"empty leg ID", "s1", PvPLeg{Account: "a", Currency: "USD", AmountCents: 1}, eur, ErrPvPInvalidLeg},
		{"empty account", "s1", PvPLeg{LegID: "x", Currency: "USD", AmountCents: 1}, eur, ErrPvPInvalidLeg},
		{"empty currency", "s1", PvPLeg{LegID: "x", Account: "a", AmountCents: 1}, eur, ErrPvPInvalidLeg},
		{"zero amount", "s1", PvPLeg{LegID: "x", Account: "a", Currency: "USD", AmountCents: 0}, eur, ErrPvPInvalidLeg},
		{"negative amount", "s1", usd, PvPLeg{LegID: "y", Account: "b", Currency: "EUR", AmountCents: -5}, ErrPvPInvalidLeg},
		{"duplicate leg ID", "s1", usd, PvPLeg{LegID: "usd-leg", Account: "bob-eur", Currency: "EUR", AmountCents: 1}, ErrPvPInvalidLeg},
		{"same payer account", "s1", usd, PvPLeg{LegID: "eur-leg", Account: "alice-usd", Currency: "EUR", AmountCents: 1}, ErrPvPInvalidLeg},
		{"same currency is not pvp", "s1", usd, PvPLeg{LegID: "eur-leg", Account: "bob-eur", Currency: "USD", AmountCents: 1}, ErrPvPInvalidLeg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPvPSettlement(tc.id, tc.a, tc.b)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
	if s := mustPvP(t, "ok"); s.Status() != PvPStatusCreated {
		t.Fatalf("new settlement status = %q, want created", s.Status())
	}
}

func TestPvPHoldCommitBidirectionalLinks(t *testing.T) {
	s := mustPvP(t, "pvp-1")
	if s.AuditLinks() != nil {
		t.Fatalf("audit links before commit = %v, want nil", s.AuditLinks())
	}
	if err := s.Commit(map[string]string{"usd-leg": "e1", "eur-leg": "e2"}); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("commit before hold: expected invalid state, got %v", err)
	}
	if err := s.Hold(); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := s.Hold(); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("second Hold: expected invalid state, got %v", err)
	}
	if err := s.Commit(map[string]string{"usd-leg": "entry-usd", "eur-leg": "entry-eur"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if s.Status() != PvPStatusCommitted {
		t.Fatalf("status = %q, want committed", s.Status())
	}
	links := s.AuditLinks()
	if len(links) != 2 {
		t.Fatalf("audit links = %d, want 2", len(links))
	}
	// Each link must name the other leg's entry: bidirectional.
	if links[0].LegID != "usd-leg" || links[0].EntryID != "entry-usd" || links[0].CounterpartyLegID != "eur-leg" || links[0].CounterpartyEntryID != "entry-eur" {
		t.Fatalf("leg 0 link = %+v", links[0])
	}
	if links[1].LegID != "eur-leg" || links[1].EntryID != "entry-eur" || links[1].CounterpartyLegID != "usd-leg" || links[1].CounterpartyEntryID != "entry-usd" {
		t.Fatalf("leg 1 link = %+v", links[1])
	}
	if links[0].SettlementID != "pvp-1" || links[1].SettlementID != "pvp-1" {
		t.Fatalf("settlement IDs = %q / %q, want pvp-1", links[0].SettlementID, links[1].SettlementID)
	}
}

func TestPvPCommitRejectsPartialAndStaysHeld(t *testing.T) {
	cases := []struct {
		name string
		ids  map[string]string
	}{
		{"missing leg", map[string]string{"usd-leg": "e1"}},
		{"empty entry", map[string]string{"usd-leg": "e1", "eur-leg": ""}},
		{"duplicate entry", map[string]string{"usd-leg": "e1", "eur-leg": "e1"}},
		{"unknown leg key", map[string]string{"usd-leg": "e1", "other-leg": "e2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustPvP(t, "pvp-partial")
			if err := s.Hold(); err != nil {
				t.Fatalf("Hold: %v", err)
			}
			if err := s.Commit(tc.ids); !errors.Is(err, ErrPvPCommitIncomplete) {
				t.Fatalf("expected commit-incomplete, got %v", err)
			}
			// A rejected commit never half-commits: still held, no links.
			if s.Status() != PvPStatusHeld {
				t.Fatalf("status after rejected commit = %q, want held", s.Status())
			}
			if s.AuditLinks() != nil {
				t.Fatalf("audit links after rejected commit = %v, want nil", s.AuditLinks())
			}
			// And the held settlement can still commit cleanly.
			if err := s.Commit(map[string]string{"usd-leg": "e1", "eur-leg": "e2"}); err != nil {
				t.Fatalf("clean commit after rejection: %v", err)
			}
		})
	}
}

func TestPvPRollbackAndFailLegAreTerminal(t *testing.T) {
	s := mustPvP(t, "pvp-rb")
	if err := s.Hold(); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := s.FailAndRollback("eur-leg", "eur rail rejected"); err != nil {
		t.Fatalf("FailAndRollback: %v", err)
	}
	if s.Status() != PvPStatusRolledBack || s.FailedLeg() != "eur-leg" || s.FailReason() != "eur rail rejected" {
		t.Fatalf("rolled back state = %q / %q / %q", s.Status(), s.FailedLeg(), s.FailReason())
	}
	// Terminal: no commit, no second rollback, no re-fail afterwards.
	if err := s.Commit(map[string]string{"usd-leg": "e1", "eur-leg": "e2"}); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("commit after rollback: expected invalid state, got %v", err)
	}
	if err := s.Rollback("again"); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("second rollback: expected invalid state, got %v", err)
	}
	if err := s.FailAndRollback("usd-leg", "again"); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("second fail: expected invalid state, got %v", err)
	}
	if s.AuditLinks() != nil {
		t.Fatalf("audit links after rollback = %v, want nil", s.AuditLinks())
	}
}

func TestPvPRollbackFromCreatedAndUnknownFailedLeg(t *testing.T) {
	s := mustPvP(t, "pvp-early")
	if err := s.Rollback("counterparty withdrew before hold"); err != nil {
		t.Fatalf("Rollback from created: %v", err)
	}
	if s.Status() != PvPStatusRolledBack {
		t.Fatalf("status = %q, want rolled_back", s.Status())
	}

	s2 := mustPvP(t, "pvp-unknown")
	if err := s2.FailAndRollback("no-such-leg", "x"); !errors.Is(err, ErrPvPInvalidLeg) {
		t.Fatalf("unknown failed leg: expected invalid leg, got %v", err)
	}
	if s2.Status() != PvPStatusCreated {
		t.Fatalf("status after unknown-leg failure = %q, want created (no state change)", s2.Status())
	}
}

func TestPvPCommittedCannotRollback(t *testing.T) {
	s := mustPvP(t, "pvp-done")
	if err := s.Hold(); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := s.Commit(map[string]string{"usd-leg": "e1", "eur-leg": "e2"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Rollback("too late"); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("rollback after commit: expected invalid state, got %v", err)
	}
	if err := s.Commit(map[string]string{"usd-leg": "e3", "eur-leg": "e4"}); !errors.Is(err, ErrPvPInvalidState) {
		t.Fatalf("second commit: expected invalid state, got %v", err)
	}
}
