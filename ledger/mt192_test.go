package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// mt192ExpectedRef is the deterministic :20: for a payout ID:
// "MT192-" plus the first 10 lowercase hex characters of the
// SHA-256 of the payout ID.
func mt192ExpectedRef(payoutID string) string {
	sum := sha256.Sum256([]byte(payoutID))
	return "MT192-" + hex.EncodeToString(sum[:])[:10]
}

func TestExportMT192_HappyPath(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-1", 250_000)

	msg, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
		Reason:     "Duplicate payment, please cancel",
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	// Block 1: sender LT is the registered BIC padded to 12.
	if !strings.HasPrefix(msg, "{1:F01BANKUS33XXX0000000000}\n") {
		t.Errorf("block 1 = %q, want sender LT BANKUS33XXX", strings.SplitN(msg, "\n", 2)[0])
	}
	// Block 2: application header, input to SWIFT, I192.
	if !strings.Contains(msg, "{2:I192BANKDEFFXXXXU3003}\n") {
		t.Error("block 2 is not {2:I192BANKDEFFXXXXU3003}")
	}
	// Block 3: the MUR is the :20: reference.
	ref := mt192ExpectedRef(p.ID)
	if !strings.Contains(msg, "{3:{108:"+ref+"}}\n") {
		t.Errorf("block 3 does not carry {108:%s}", ref)
	}
	// Block 5 is omitted: the network computes CHK.
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	// No trailing newline (the nacha.go convention).
	if strings.HasSuffix(msg, "\n") {
		t.Error("message must not end with a newline")
	}

	f := parseMT103Back(t, msg)
	// :20: is exactly 16 characters, the deterministic derivation.
	if got := f[":20:"]; len(got) != 1 || got[0] != ref {
		t.Errorf(":20: = %v, want [%s]", got, ref)
	}
	if len(ref) != 16 {
		t.Errorf(":20: = %q, want exactly 16 characters", ref)
	}
	if hexPart := ref[len("MT192-"):]; hexPart != strings.ToLower(hexPart) {
		t.Errorf(":20: hex part = %q, want lowercase hex", hexPart)
	}
	// :21: is the payout ID — the original MT103's :20:.
	if got := f[":21:"]; len(got) != 1 || got[0] != p.ID {
		t.Errorf(":21: = %v, want [%s]", got, p.ID)
	}
	// :11S: is "103" plus the original value date (default: the
	// submit time).
	want11S := "103" + p.SubmittedAt.UTC().Format("060102")
	if got := f[":11S:"]; len(got) != 1 || got[0] != want11S {
		t.Errorf(":11S: = %v, want [%s]", got, want11S)
	}
	// :79: carries the reason verbatim.
	if got := f[":79:"]; len(got) != 1 || got[0] != "Duplicate payment, please cancel" {
		t.Errorf(":79: = %v, want [Duplicate payment, please cancel]", got)
	}

	// Repeat exports are byte-identical: deterministic per payout
	// and options.
	again, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
		Reason:     "Duplicate payment, please cancel",
	})
	if err != nil {
		t.Fatalf("ExportMT192 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT192_ExplicitValueDate(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-2", 100_000)

	// An explicit original value date wins over the default rule.
	explicit := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	msg, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:          p.ID,
		ReceiverLT:        "BANKDEFFXXXX",
		OriginalValueDate: explicit,
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	f := parseMT103Back(t, msg)
	if got := f[":11S:"]; len(got) != 1 || got[0] != "103261110" {
		t.Errorf(":11S: = %v, want [103261110]", got)
	}
}

func TestExportMT192_DefaultValueDate(t *testing.T) {
	l := mt103TestLedger(t)

	// Approved but not submitted: the default falls back to the
	// approval time (submit → approval → creation).
	draftPayout(t, l, "po-mt192-3", 100_000)
	p, err := l.ApprovePayout("po-mt192-3", "bob")
	if err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	msg, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	f := parseMT103Back(t, msg)
	want11S := "103" + p.ApprovedAt.UTC().Format("060102")
	if got := f[":11S:"]; len(got) != 1 || got[0] != want11S {
		t.Errorf(":11S: = %v, want [%s] (approval fallback)", got, want11S)
	}
	if !p.SubmittedAt.IsZero() {
		t.Error("approved-only payout must have a zero SubmittedAt")
	}

	// Submitted: the submit time wins over approval and creation.
	p2 := mt103SubmittedPayout(t, l, "po-mt192-4", 100_000)
	msg, err = l.ExportMT192(MT192ExportOptions{
		PayoutID:   p2.ID,
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	f = parseMT103Back(t, msg)
	want11S = "103" + p2.SubmittedAt.UTC().Format("060102")
	if got := f[":11S:"]; len(got) != 1 || got[0] != want11S {
		t.Errorf(":11S: = %v, want [%s] (submit wins)", got, want11S)
	}
}

func TestExportMT192_NoReason(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-5", 100_000)

	msg, err := l.ExportMT192(MT192ExportOptions{
		PayoutID:   p.ID,
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	f := parseMT103Back(t, msg)
	if _, ok := f[":79:"]; ok {
		t.Error(":79: must be omitted without a reason")
	}
}

func TestExportMT192_BadStatus(t *testing.T) {
	l := mt103TestLedger(t)
	// Draft: nothing was ever sent, so there is nothing to cancel.
	draftPayout(t, l, "po-mt192-draft", 100_000)
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: "po-mt192-draft", ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT192BadStatus) {
		t.Errorf("draft: err = %v, want ErrMT192BadStatus", err)
	}
	// Settled: the money already moved — cancel via pacs.007.
	p := settlePayoutForReturn(t, l, "po-mt192-settled", 100_000)
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID, ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT192BadStatus) {
		t.Errorf("settled: err = %v, want ErrMT192BadStatus", err)
	}
	// Cancelled: the instruction is already withdrawn.
	draftPayout(t, l, "po-mt192-cancelled", 100_000)
	if _, err := l.CancelPayout("po-mt192-cancelled"); err != nil {
		t.Fatalf("CancelPayout: %v", err)
	}
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: "po-mt192-cancelled", ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT192BadStatus) {
		t.Errorf("cancelled: err = %v, want ErrMT192BadStatus", err)
	}
	// Unknown payout: the MT103 lookup.
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: "no-such-payout", ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrMT103PayoutNotFound", err)
	}
}

func TestExportMT192_BadReceiver(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-6", 100_000)

	// Missing receiver: the ledger keeps no routing table.
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID}); !errors.Is(err, ErrInvalidMT192Export) {
		t.Errorf("missing receiver: err = %v, want ErrInvalidMT192Export", err)
	}
	// Garbage LT addresses are refused.
	for _, bad := range []string{"TOOLONGFORALTBIC12X", "BANK US33", "BANKUS3!"} {
		_, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID, ReceiverLT: bad})
		if !errors.Is(err, ErrInvalidMT192Export) {
			t.Errorf("receiver %q: err = %v, want ErrInvalidMT192Export", bad, err)
		}
	}
}

func TestExportMT192_BadReason(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt192-7", 100_000)

	// A reason outside the SWIFT `x` set is refused, never truncated.
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID, ReceiverLT: "BANKDEFFXXXX", Reason: "café"}); !errors.Is(err, ErrInvalidMT192Export) {
		t.Errorf("non-x reason: err = %v, want ErrInvalidMT192Export", err)
	}
	// A single :79: line over 35 characters is refused.
	if _, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID, ReceiverLT: "BANKDEFFXXXX", Reason: strings.Repeat("x", 36)}); !errors.Is(err, ErrInvalidMT192Export) {
		t.Errorf("over-long reason: err = %v, want ErrInvalidMT192Export", err)
	}
	// A multi-line reason renders as continuation lines.
	msg, err := l.ExportMT192(MT192ExportOptions{PayoutID: p.ID, ReceiverLT: "BANKDEFFXXXX", Reason: "Duplicate payment\nPlease cancel"})
	if err != nil {
		t.Fatalf("ExportMT192: %v", err)
	}
	f := parseMT103Back(t, msg)
	if got := f[":79:"]; len(got) != 2 || got[0] != "Duplicate payment" || got[1] != "Please cancel" {
		t.Errorf(":79: = %v, want [Duplicate payment Please cancel]", got)
	}
}
