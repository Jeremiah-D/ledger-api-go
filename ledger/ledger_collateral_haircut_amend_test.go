package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestAmendCollateralHaircutHappyPath(t *testing.T) {
	l := pledgeFixture(t)
	before, ok := l.GetCollateral("alice", "bond-1")
	if !ok {
		t.Fatal("pledge missing")
	}
	// Raising the haircut 2500 -> 5000 halves the effective value
	// (75000 -> 50000) but leaves everything else untouched.
	amended, calls, err := l.AmendCollateralHaircut("alice", "bond-1", 5000)
	if err != nil {
		t.Fatalf("AmendCollateralHaircut: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("margin calls = %+v, want none (nothing drawn)", calls)
	}
	if amended.HaircutBps != 5000 {
		t.Errorf("amended haircut = %d, want 5000", amended.HaircutBps)
	}
	amended.HaircutBps = before.HaircutBps
	if amended != before {
		t.Errorf("amended pledge = %+v, want %+v except the haircut", amended, before)
	}
	// The new haircut takes effect on the next coverage evaluation.
	if effective, _, _, ok := l.CollateralCoverage("alice", "USD"); !ok || effective != 50000 {
		t.Errorf("effective after amend = %d ok=%v, want 50000", effective, ok)
	}
}

func TestAmendCollateralHaircutValidation(t *testing.T) {
	l := pledgeFixture(t)
	if _, _, err := l.AmendCollateralHaircut("alice", "no-such-asset", 5000); !errors.Is(err, ErrCollateralNotFound) {
		t.Errorf("unknown asset: err=%v, want ErrCollateralNotFound", err)
	}
	if _, _, err := l.AmendCollateralHaircut("ghost", "bond-1", 5000); !errors.Is(err, ErrCollateralNotFound) {
		t.Errorf("unknown account: err=%v, want ErrCollateralNotFound", err)
	}
	if _, _, err := l.AmendCollateralHaircut("", "bond-1", 5000); !errors.Is(err, ErrEmptyCollateralAccount) {
		t.Errorf("empty account: err=%v, want ErrEmptyCollateralAccount", err)
	}
	if _, _, err := l.AmendCollateralHaircut("alice", "  ", 5000); !errors.Is(err, ErrEmptyCollateralAsset) {
		t.Errorf("empty asset: err=%v, want ErrEmptyCollateralAsset", err)
	}
	// The PledgeCollateral haircut gate, applied to the amendment.
	for _, haircut := range []int64{-1, 10001} {
		if _, _, err := l.AmendCollateralHaircut("alice", "bond-1", haircut); !errors.Is(err, ErrInvalidCollateralHaircut) {
			t.Errorf("haircut %d: err=%v, want ErrInvalidCollateralHaircut", haircut, err)
		}
	}
	// Every rejected amendment leaves the pledge exactly as it was.
	if p, ok := l.GetCollateral("alice", "bond-1"); !ok || p.HaircutBps != 2500 || p.Quantity != 10 || p.UnitValueCents != 10000 {
		t.Errorf("pledge after rejected amendments = %+v ok=%v, want 10/10000/2500", p, ok)
	}
	// Boundary values are legal: 0 removes the discount, 10000
	// discounts the asset to zero effective value.
	if _, _, err := l.AmendCollateralHaircut("alice", "bond-1", 0); err != nil {
		t.Fatalf("amend to 0: %v", err)
	}
	if _, _, err := l.AmendCollateralHaircut("alice", "bond-1", 10000); err != nil {
		t.Fatalf("amend to 10000: %v", err)
	}
	if p, _ := l.GetCollateral("alice", "bond-1"); p.HaircutBps != 10000 {
		t.Errorf("haircut after boundary amends = %d, want 10000", p.HaircutBps)
	}
}

func TestAmendCollateralHaircutNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500); err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	before, _ := l.GetCollateral("alice", "bond-1")
	amended, calls, err := l.AmendCollateralHaircut("alice", "bond-1", 2500)
	if err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("no-op margin calls = %+v, want none", calls)
	}
	if amended != before {
		t.Errorf("no-op pledge = %+v, want %+v", amended, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no collateral_haircut_amended audit event
	// (the pledge event is the only collateral event).
	if events := auditEventsByOp(t, dir)["collateral_haircut_amended"]; len(events) != 0 {
		t.Fatalf("no-op amend events = %d, want 0", len(events))
	}
}

func TestAmendCollateralHaircutMarginCall(t *testing.T) {
	l := pledgeFixture(t)
	seedBalance(t, l, "alice", 200000)
	// Seed 200000, then draw 260000: balance -60000, drawn 60000,
	// needing 75000 coverage at 125% — exactly covered by the
	// 75000 effective collateral.
	if _, err := l.PostTransfer(Transfer{ID: "hc-draw-1", From: "alice", To: "bob", AmountCents: 260000, Currency: "USD"}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	// Raising the haircut 2500 -> 5000 halves effective collateral
	// to 50000: 60000 drawn now needs 75000 — a margin call.
	amended, calls, err := l.AmendCollateralHaircut("alice", "bond-1", 5000)
	if err != nil {
		t.Fatalf("AmendCollateralHaircut: %v", err)
	}
	if amended.HaircutBps != 5000 {
		t.Errorf("amended haircut = %d, want 5000", amended.HaircutBps)
	}
	if len(calls) != 1 {
		t.Fatalf("margin calls = %+v, want 1", calls)
	}
	mc := calls[0]
	if mc.Account != "alice" || mc.Currency != "USD" || mc.EffectiveCents != 50000 ||
		mc.DrawnCents != 60000 || mc.RequiredBps != 12500 {
		t.Errorf("margin call = %+v, want alice/USD effective 50000 drawn 60000 required 12500", mc)
	}
	// Lowering the haircut back restores coverage: no margin call.
	if _, calls, err := l.AmendCollateralHaircut("alice", "bond-1", 0); err != nil {
		t.Fatalf("lower haircut: %v", err)
	} else if len(calls) != 0 {
		t.Errorf("restored margin calls = %+v, want none", calls)
	}
}

func TestAmendCollateralHaircutAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500); err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	before, _ := l.GetCollateral("alice", "bond-1")
	versionBefore := l.Version()
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, _, err := l.AmendCollateralHaircutCtx(ctx, "alice", "bond-1", 5000); err != nil {
		t.Fatalf("AmendCollateralHaircutCtx: %v", err)
	}
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (amendment must not bump)", l.Version(), versionBefore)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetCollateral("alice", "bond-1")
	if !ok {
		t.Fatal("restored pledge missing")
	}
	if after.HaircutBps != 5000 || after.Quantity != 10 || after.UnitValueCents != 10000 {
		t.Errorf("restored pledge = %+v, want 10/10000/5000", after)
	}
	if after.PledgedAt.UnixNano() != before.PledgedAt.UnixNano() {
		t.Errorf("restored pledged_at = %v, want %v", after.PledgedAt, before.PledgedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["collateral_haircut_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["asset_id"] != "bond-1" || d["previous_haircut_bps"] != float64(2500) ||
		d["haircut_bps"] != float64(5000) {
		t.Errorf("event details = %+v, want bond-1 2500->5000", d)
	}
	if events[0].VersionBefore != events[0].VersionAfter {
		t.Errorf("event versions = %d->%d, want no bump", events[0].VersionBefore, events[0].VersionAfter)
	}
}
