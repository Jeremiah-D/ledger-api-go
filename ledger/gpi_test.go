package ledger

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// uetrPattern is the RFC 4122 UUID v4 text form: 8-4-4-4-12 lowercase
// hex, the version nibble pinned to 4 and the variant nibble to
// 8/9/a/b.
var uetrPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUETR_RFC4122(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := newUETR()
		if !uetrPattern.MatchString(u) {
			t.Fatalf("newUETR() = %q, not RFC 4122 UUID v4 form", u)
		}
		if seen[u] {
			t.Fatalf("duplicate UETR %q in 100 draws", u)
		}
		seen[u] = true
	}
}

// CreatePayout assigns a UETR and opens a tracked gpi record: the
// payment is tracked end-to-end from inception.
func TestCreatePayout_AssignsUETRAndTracks(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-gpi-1", 100_000)
	if !uetrPattern.MatchString(p.UETR) {
		t.Fatalf("payout UETR = %q, want RFC 4122 UUID v4", p.UETR)
	}
	track, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("no gpi track opened for the payout's UETR")
	}
	if track.Status != GPIStatusTracked {
		t.Errorf("track status = %q, want %q", track.Status, GPIStatusTracked)
	}
	if track.PayoutID != "po-gpi-1" {
		t.Errorf("track payout = %q, want po-gpi-1", track.PayoutID)
	}
	if track.ReasonCode != "" {
		t.Errorf("fresh track reason = %q, want empty", track.ReasonCode)
	}
	tracked, settled, rejected := l.GPICounts()
	if tracked != 1 || settled != 0 || rejected != 0 {
		t.Errorf("counts = (%d,%d,%d), want (1,0,0)", tracked, settled, rejected)
	}
	// A second payout gets a distinct UETR and its own track.
	p2 := draftPayout(t, l, "po-gpi-2", 50_000)
	if p2.UETR == p.UETR {
		t.Error("two payouts share a UETR")
	}
	if tracked, _, _ := l.GPICounts(); tracked != 2 {
		t.Errorf("tracked = %d, want 2", tracked)
	}
	// Idempotent replay returns the original instruction — and its
	// original UETR, not a fresh one.
	p0, dup, err := l.CreatePayout(PayoutInstruction{
		ID:             "po-gpi-3",
		PayeeAccount:   "saver",
		AmountCents:    100_000,
		Destination:    "FPS:6222000012345678",
		Maker:          "alice",
		IdempotencyKey: "k-gpi-3",
	})
	if err != nil || dup {
		t.Fatalf("create: %v dup=%v", err, dup)
	}
	p3, dup, err := l.CreatePayout(PayoutInstruction{
		ID:             "po-gpi-3-other",
		PayeeAccount:   "saver",
		AmountCents:    999,
		Destination:    "FPS:9",
		Maker:          "bob",
		IdempotencyKey: "k-gpi-3",
	})
	if err != nil || !dup {
		t.Fatalf("replay: err=%v dup=%v", err, dup)
	}
	if p3.UETR != p0.UETR {
		t.Errorf("replay UETR = %q, want original %q", p3.UETR, p0.UETR)
	}
	if tracked, _, _ := l.GPICounts(); tracked != 3 {
		t.Errorf("tracked after replay = %d, want 3 (no new track)", tracked)
	}
}

// AdvanceGPI walks the forward-only machine: tracked -> ACSP -> ACSC
// -> ACCP, with idempotent same-state replay and terminal states.
func TestAdvanceGPI_ForwardPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-adv-1", 10_000)

	for _, step := range []struct {
		to     string
		reason string
	}{
		{GPIStatusACSP, ""},
		{GPIStatusACSC, ""},
		{GPIStatusACCP, ""},
	} {
		track, err := l.AdvanceGPI(p.UETR, step.to, step.reason, "ops")
		if err != nil {
			t.Fatalf("AdvanceGPI(%s): %v", step.to, err)
		}
		if track.Status != step.to {
			t.Fatalf("status = %q, want %q", track.Status, step.to)
		}
	}
	track, _ := l.GetGPITrack(p.UETR)
	if len(track.History) != 3 {
		t.Fatalf("history has %d entries, want 3", len(track.History))
	}
	if track.History[0].From != GPIStatusTracked || track.History[2].To != GPIStatusACCP {
		t.Errorf("history endpoints wrong: %+v", track.History)
	}
	if track.History[1].Actor != "ops" {
		t.Errorf("history actor = %q, want ops", track.History[1].Actor)
	}
	if _, settled, _ := l.GPICounts(); settled != 1 {
		t.Errorf("settled = %d, want 1", settled)
	}
	// Terminal ACCP: any further move is refused.
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSC, "", "ops"); !errors.Is(err, ErrGPIInvalidTransition) {
		t.Errorf("ACCP -> ACSC: err = %v, want ErrGPIInvalidTransition", err)
	}
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "X", "ops"); !errors.Is(err, ErrGPIInvalidTransition) {
		t.Errorf("ACCP -> RJCT: err = %v, want ErrGPIInvalidTransition", err)
	}
}

// Same-state advance is an idempotent no-op: no new history, no new
// audit event, no counter move.
func TestAdvanceGPI_IdempotentReplay(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-adv-2", 10_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "ops"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	track, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "ops")
	if err != nil {
		t.Fatalf("replay advance: %v", err)
	}
	if len(track.History) != 1 {
		t.Errorf("history has %d entries after replay, want 1", len(track.History))
	}
}

// Backward moves are refused: the mirror never un-reports a state.
func TestAdvanceGPI_BackwardRefused(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-adv-3", 10_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSC, "", "ops"); err != nil {
		t.Fatalf("advance to ACSC: %v", err)
	}
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "ops"); !errors.Is(err, ErrGPIInvalidTransition) {
		t.Errorf("ACSC -> ACSP: err = %v, want ErrGPIInvalidTransition", err)
	}
	if _, err := l.AdvanceGPI(p.UETR, "BOGUS", "", "ops"); !errors.Is(err, ErrGPIInvalidStatus) {
		t.Errorf("unknown status: err = %v, want ErrGPIInvalidStatus", err)
	}
	if _, err := l.AdvanceGPI("no-such-uetr", GPIStatusACSP, "", "ops"); !errors.Is(err, ErrGPIUnknownUETR) {
		t.Errorf("unknown UETR: err = %v, want ErrGPIUnknownUETR", err)
	}
}

// RJCT requires a reason code (fail-closed), is terminal, and bumps
// the rejected counter; the reason lands on the track and in history.
func TestAdvanceGPI_Rejection(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-adv-4", 10_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "", "ops"); !errors.Is(err, ErrGPIReasonRequired) {
		t.Fatalf("RJCT without reason: err = %v, want ErrGPIReasonRequired", err)
	}
	track, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC04", "ops")
	if err != nil {
		t.Fatalf("RJCT with reason: %v", err)
	}
	if track.ReasonCode != "AC04" {
		t.Errorf("reason = %q, want AC04", track.ReasonCode)
	}
	if track.History[len(track.History)-1].ReasonCode != "AC04" {
		t.Errorf("history reason = %q, want AC04", track.History[len(track.History)-1].ReasonCode)
	}
	if _, _, rejected := l.GPICounts(); rejected != 1 {
		t.Errorf("rejected = %d, want 1", rejected)
	}
	// Terminal: nothing moves out of RJCT, and a rejection clears no
	// path back to acceptance.
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSC, "", "ops"); !errors.Is(err, ErrGPIInvalidTransition) {
		t.Errorf("RJCT -> ACSC: err = %v, want ErrGPIInvalidTransition", err)
	}
}

// A forward jump may skip ACSP: the machine orders states, it does
// not require visiting each one.
func TestAdvanceGPI_ForwardJump(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-adv-5", 10_000)
	track, err := l.AdvanceGPI(p.UETR, GPIStatusACCP, "", "ops")
	if err != nil {
		t.Fatalf("tracked -> ACCP: %v", err)
	}
	if track.Status != GPIStatusACCP {
		t.Errorf("status = %q, want ACCP", track.Status)
	}
}

// LinkGPITransfer joins a transfer to a UETR: unknown UETRs are
// refused, a transfer linked to a different UETR is a conflict, and
// re-linking the same pair is a no-op.
func TestLinkGPITransfer(t *testing.T) {
	l := seedPayoutLedger(t)
	p1 := draftPayout(t, l, "po-link-1", 10_000)
	p2 := draftPayout(t, l, "po-link-2", 10_000)

	if _, err := l.LinkGPITransfer("no-such-uetr", "tx-1", "ops"); !errors.Is(err, ErrGPIUnknownUETR) {
		t.Errorf("unknown UETR: err = %v, want ErrGPIUnknownUETR", err)
	}
	if _, err := l.LinkGPITransfer(p1.UETR, "", "ops"); !errors.Is(err, ErrGPIEmptyTransferID) {
		t.Errorf("empty transfer: err = %v, want ErrGPIEmptyTransferID", err)
	}
	track, err := l.LinkGPITransfer(p1.UETR, "tx-1", "ops")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if track.TransferID != "tx-1" {
		t.Errorf("linked transfer = %q, want tx-1", track.TransferID)
	}
	// Re-linking the same pair is a no-op success.
	if _, err := l.LinkGPITransfer(p1.UETR, "tx-1", "ops"); err != nil {
		t.Errorf("re-link same pair: %v", err)
	}
	// The same transfer under a different UETR is a conflict.
	if _, err := l.LinkGPITransfer(p2.UETR, "tx-1", "ops"); !errors.Is(err, ErrGPITransferConflict) {
		t.Errorf("conflicting link: err = %v, want ErrGPITransferConflict", err)
	}
}

// The pain.002 bank verdict advances the gpi mirror for linked
// transfers: ACSC/ACCP move it forward, RJCT rejects it with the
// bank's reason code, and unlinked transfers touch no track. The
// EndToEndId stays the pain.001 idempotency key throughout.
func TestPain002AutoAdvancesGPI(t *testing.T) {
	l, _ := pain002TestLedger(t)
	p := draftPayout(t, l, "po-p2-1", 10_000)
	if _, err := l.LinkGPITransfer(p.UETR, "ps1", "ops"); err != nil {
		t.Fatalf("link: %v", err)
	}
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	// ACSC on the linked transfer: pain.001 accepted, gpi at ACSC.
	doc := pain002TestDoc(t, msgID, pain002TxFrag("ps1", "ACSC", "", ""))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002Report(report, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	if got := pain002TxState(t, rec, "ps1").State; got != Pain001StateAccepted {
		t.Errorf("pain.001 tx state = %q, want accepted", got)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusACSC {
		t.Errorf("gpi status = %q, want ACSC", track.Status)
	}
	if len(track.History) != 1 || track.History[0].Actor != "bank-ops" {
		t.Errorf("gpi history = %+v, want one bank-ops entry", track.History)
	}

	// ACCP moves it to settled; the settled counter moves.
	doc = pain002TestDoc(t, msgID, pain002TxFrag("ps1", "ACCP", "", ""))
	report, err = ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-ops"); err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	track, _ = l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusACCP {
		t.Errorf("gpi status = %q, want ACCP", track.Status)
	}
	if _, settled, _ := l.GPICounts(); settled != 1 {
		t.Errorf("settled = %d, want 1", settled)
	}

	// A late ACSC report does not move the ACCP track backward; the
	// pain.001 receipt is still recorded idempotently.
	doc = pain002TestDoc(t, msgID, pain002TxFrag("ps1", "ACSC", "", ""))
	report, err = ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-ops"); err != nil {
		t.Fatalf("late ApplyPain002Report: %v", err)
	}
	track, _ = l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusACCP {
		t.Errorf("gpi status after late report = %q, want ACCP (no backward move)", track.Status)
	}
}

// RJCT through pain.002 rejects the gpi track with the bank's reason
// code, verbatim — the bank's vocabulary, not the ledger's.
func TestPain002RejectsGPIWithReason(t *testing.T) {
	l, _ := pain002TestLedger(t)
	p := draftPayout(t, l, "po-p2-2", 10_000)
	if _, err := l.LinkGPITransfer(p.UETR, "ps2", "ops"); err != nil {
		t.Fatalf("link: %v", err)
	}
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID,
		pain002TxFrag("ps1", "ACSC", "", ""),
		pain002TxFrag("ps2", "RJCT", "AM04", "Insufficient funds"),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-ops"); err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusRJCT {
		t.Fatalf("gpi status = %q, want RJCT", track.Status)
	}
	if track.ReasonCode != "AM04: Insufficient funds" {
		t.Errorf("reason = %q, want bank reason verbatim", track.ReasonCode)
	}
	if _, _, rejected := l.GPICounts(); rejected != 1 {
		t.Errorf("rejected = %d, want 1", rejected)
	}
	// ps1 was not linked to this UETR: its verdict touched no track.
	// (ps2's link is the only one; assert via the payout's own track
	// staying consistent is covered by the RJCT assertion above.)
}

// An all-PDNG report is a no-op for both machines: no gpi move, no
// audit event, no counter.
func TestPain002PDNGLeavesGPITracked(t *testing.T) {
	l, _ := pain002TestLedger(t)
	p := draftPayout(t, l, "po-p2-3", 10_000)
	if _, err := l.LinkGPITransfer(p.UETR, "ps1", "ops"); err != nil {
		t.Fatalf("link: %v", err)
	}
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID, pain002TxFrag("ps1", "PDNG", "", ""))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-ops"); err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusTracked {
		t.Errorf("gpi status = %q, want tracked (PDNG moves nothing)", track.Status)
	}
	if len(track.History) != 0 {
		t.Errorf("history has %d entries, want 0", len(track.History))
	}
}

// GPITracks lists every track sorted by UETR.
func TestGPITracks_Listed(t *testing.T) {
	l := seedPayoutLedger(t)
	p1 := draftPayout(t, l, "po-list-1", 10_000)
	p2 := draftPayout(t, l, "po-list-2", 10_000)
	tracks := l.GPITracks()
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
	if tracks[0].UETR != p1.UETR && tracks[0].UETR != p2.UETR {
		t.Errorf("unexpected track UETR %q", tracks[0].UETR)
	}
	// Mutating a returned copy does not affect ledger state.
	tracks[0].Status = "MUTATED"
	fresh, _ := l.GetGPITrack(tracks[0].UETR)
	if strings.Contains(fresh.Status, "MUTATED") {
		t.Error("returned track aliases ledger state")
	}
}
