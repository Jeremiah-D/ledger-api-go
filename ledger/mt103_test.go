package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// parseMT103Back extracts the text-block fields of an exported MT103
// into tag → content lines: the honest round-trip check that the FIN
// text the ledger emits is well-framed and carries the fields the
// export claims. Field tags keep their option letter (":32A:", ":50K:").
func parseMT103Back(t *testing.T, msg string) map[string][]string {
	t.Helper()
	start := strings.Index(msg, "{4:\n")
	if start < 0 {
		t.Fatalf("message has no {4: text block")
	}
	end := strings.Index(msg[start:], "\n-}")
	if end < 0 {
		t.Fatalf("message text block is not terminated with -}")
	}
	body := msg[start+len("{4:\n") : start+end]
	fields := map[string][]string{}
	var cur string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, ":") {
			if i := strings.Index(ln[1:], ":"); i >= 0 {
				cur = ln[:i+2] // ":20:", ":32A:", ...
				fields[cur] = append(fields[cur], ln[i+2:])
				continue
			}
		}
		if cur == "" {
			t.Fatalf("text block line outside any field: %q", ln)
		}
		fields[cur] = append(fields[cur], ln)
	}
	return fields
}

// mt103TestLedger seeds a payout ledger with the sender bank's BIC
// registered on the payee account (the SetAccountIBAN convention).
func mt103TestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := seedPayoutLedger(t)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "BANKUS33"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	return l
}

func mt103SubmittedPayout(t *testing.T, l *Ledger, id string, amount int64) PayoutInstruction {
	t.Helper()
	draftPayout(t, l, id, amount)
	return approveSubmitPayout(t, l, id, "bob")
}

func TestExportMT103_HappyPath(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-1", 250_000)

	msg, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	// Block 1: sender LT is the registered BIC padded to 12.
	if !strings.HasPrefix(msg, "{1:F01BANKUS33XXX0000000000}\n") {
		t.Errorf("block 1 = %q, want sender LT BANKUS33XXX", strings.SplitN(msg, "\n", 2)[0])
	}
	// Block 2: application header with the receiver LT.
	if !strings.Contains(msg, "{2:I103BANKDEFFXXXXU3003}\n") {
		t.Error("block 2 is not {2:I103BANKDEFFXXXXU3003}")
	}
	// Block 3: the MUR ties the message to the payout.
	if !strings.Contains(msg, "{3:{108:po-mt103-1}}\n") {
		t.Error("block 3 does not carry {108:po-mt103-1}")
	}
	// Block 5 is omitted: the network computes CHK.
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}

	f := parseMT103Back(t, msg)
	if got := f[":20:"]; len(got) != 1 || got[0] != "po-mt103-1" {
		t.Errorf(":20: = %v, want [po-mt103-1]", got)
	}
	if got := f[":23B:"]; len(got) != 1 || got[0] != "CRED" {
		t.Errorf(":23B: = %v, want [CRED]", got)
	}
	want32A := p.SubmittedAt.UTC().Format("060102") + "USD2500,00"
	if got := f[":32A:"]; len(got) != 1 || got[0] != want32A {
		t.Errorf(":32A: = %v, want [%s]", got, want32A)
	}
	if got := f[":50K:"]; len(got) != 1 || got[0] != "/saver" {
		t.Errorf(":50K: = %v, want [/saver]", got)
	}
	// No :52A: by default (direct-credit path).
	if _, ok := f[":52A:"]; ok {
		t.Error(":52A: must be omitted without OrderingInstitutionBIC")
	}
	// :59: defaults to the destination with the scheme prefix stripped.
	if got := f[":59:"]; len(got) != 1 || got[0] != "/6222000012345678" {
		t.Errorf(":59: = %v, want [/6222000012345678]", got)
	}
	if got := f[":71A:"]; len(got) != 1 || got[0] != "SHA" {
		t.Errorf(":71A: = %v, want [SHA]", got)
	}
	// No :70: by default.
	if _, ok := f[":70:"]; ok {
		t.Error(":70: must be omitted without RemittanceInfo")
	}
	// Repeat exports are byte-identical: deterministic per payout.
	again, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"})
	if err != nil {
		t.Fatalf("ExportMT103 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT103_LTNormalization(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-2", 100_000)

	// A BIC11 receiver normalizes with an X branch pad.
	msg, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "CREDAGRAAAX"})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	if !strings.Contains(msg, "{2:I103CREDAGRAAAXXU3003}") {
		t.Error("BIC11 receiver was not normalized to a 12-char LT")
	}
	// An explicit 12-char sender LT is used verbatim.
	msg, err = l.ExportMT103(p.ID, MT103ExportOptions{
		SenderLT:   "BANKUS33ABCD",
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	if !strings.HasPrefix(msg, "{1:F01BANKUS33ABCD0000000000}") {
		t.Error("explicit SenderLT was not used verbatim")
	}
	// Garbage LT addresses are refused.
	for _, bad := range []string{"", "TOOLONGFORALTBIC12X", "BANK US33", "BANKUS3!"} {
		_, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: bad})
		if !errors.Is(err, ErrMT103LT) && !errors.Is(err, ErrMT103Config) {
			t.Errorf("receiver %q: err = %v, want ErrMT103LT/ErrMT103Config", bad, err)
		}
	}
}

func TestExportMT103_BadStatus(t *testing.T) {
	l := mt103TestLedger(t)
	// Draft: not yet authorized.
	draftPayout(t, l, "po-mt103-draft", 100_000)
	if _, err := l.ExportMT103("po-mt103-draft", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103BadStatus) {
		t.Errorf("draft: err = %v, want ErrMT103BadStatus", err)
	}
	// Settled: the money already moved — a second instruction would
	// double-pay.
	p := settlePayoutForReturn(t, l, "po-mt103-settled", 100_000)
	if _, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103BadStatus) {
		t.Errorf("settled: err = %v, want ErrMT103BadStatus", err)
	}
	// Unknown payout.
	if _, err := l.ExportMT103("no-such-payout", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrMT103PayoutNotFound", err)
	}
}

func TestExportMT103_SenderBIC(t *testing.T) {
	// No registry, no option: refused, never guessed.
	l := seedPayoutLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-nobic", 100_000)
	if _, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103NoSenderBIC) {
		t.Errorf("no BIC: err = %v, want ErrMT103NoSenderBIC", err)
	}
	// Explicit option works without a registry entry.
	msg, err := l.ExportMT103(p.ID, MT103ExportOptions{
		SenderBIC:  "CHASUS33",
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT103 with SenderBIC: %v", err)
	}
	if !strings.HasPrefix(msg, "{1:F01CHASUS33XXX0000000000}") {
		t.Error("explicit SenderBIC was not honored")
	}
	// Explicit option wins over the registry.
	l2 := mt103TestLedger(t)
	p2 := mt103SubmittedPayout(t, l2, "po-mt103-bicwin", 100_000)
	msg, err = l2.ExportMT103(p2.ID, MT103ExportOptions{
		SenderBIC:  "CHASUS33",
		ReceiverLT: "BANKDEFFXXXX",
	})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	if !strings.HasPrefix(msg, "{1:F01CHASUS33XXX0000000000}") {
		t.Error("explicit SenderBIC must win over the registry")
	}
}

func TestExportMT103_Charset(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-cs", 100_000)
	base := MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}

	// Non-x characters (emoji) are refused, never mangled.
	opts := base
	opts.OrderingCustomerName = []string{"Müller GmbH 😀"}
	if _, err := l.ExportMT103(p.ID, opts); !errors.Is(err, ErrMT103Charset) {
		t.Errorf("emoji name: err = %v, want ErrMT103Charset", err)
	}
	// Braces would break FIN framing.
	opts = base
	opts.RemittanceInfo = "ref {123}"
	if _, err := l.ExportMT103(p.ID, opts); !errors.Is(err, ErrMT103Charset) {
		t.Errorf("brace in :70:: err = %v, want ErrMT103Charset", err)
	}
	// Umlauts are outside the x set too — the caller transliterates.
	opts = base
	opts.BeneficiaryName = []string{"Müller"}
	if _, err := l.ExportMT103(p.ID, opts); !errors.Is(err, ErrMT103Charset) {
		t.Errorf("umlaut: err = %v, want ErrMT103Charset", err)
	}
	// The full x set passes.
	opts = base
	opts.OrderingCustomerName = []string{"Mueller GmbH", "12 Main St. (HQ)", "ref: A/B-C?D,E.'F+G"}
	opts.RemittanceInfo = "inv *123* = !\"%_#&$@<> done"
	if _, err := l.ExportMT103(p.ID, opts); err != nil {
		t.Errorf("full x set: ExportMT103: %v", err)
	}
}

func TestExportMT103_Options(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-opt", 1_234_567)

	msg, err := l.ExportMT103(p.ID, MT103ExportOptions{
		ReceiverLT:               "BANKDEFFXXXX",
		BankOperationCode:        "SPAY",
		OrderingInstitutionBIC:   "BANKUS33",
		BeneficiaryInstitutionBIC: "CREDAGRA",
		BeneficiaryAccount:       "DE75512108001245126199",
		BeneficiaryName:          []string{"ACME Corp", "Berlin"},
		RemittanceInfo:           "invoice 42\nref ABC",
		DetailsOfCharges:         "OUR",
		ValueDate:                time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	f := parseMT103Back(t, msg)
	if got := f[":23B:"]; len(got) != 1 || got[0] != "SPAY" {
		t.Errorf(":23B: = %v, want [SPAY]", got)
	}
	if got := f[":32A:"]; len(got) != 1 || got[0] != "261022USD12345,67" {
		t.Errorf(":32A: = %v, want [261022USD12345,67]", got)
	}
	if got := f[":52A:"]; len(got) != 1 || got[0] != "BANKUS33" {
		t.Errorf(":52A: = %v, want [BANKUS33]", got)
	}
	if got := f[":57A:"]; len(got) != 1 || got[0] != "CREDAGRA" {
		t.Errorf(":57A: = %v, want [CREDAGRA]", got)
	}
	if got := f[":59:"]; len(got) != 3 || got[0] != "/DE75512108001245126199" || got[1] != "ACME Corp" || got[2] != "Berlin" {
		t.Errorf(":59: = %v", got)
	}
	if got := f[":70:"]; len(got) != 2 || got[0] != "invoice 42" || got[1] != "ref ABC" {
		t.Errorf(":70: = %v", got)
	}
	if got := f[":71A:"]; len(got) != 1 || got[0] != "OUR" {
		t.Errorf(":71A: = %v, want [OUR]", got)
	}

	// Unknown vocabularies are refused.
	for _, tc := range []struct {
		name string
		opts MT103ExportOptions
	}{
		{"bank op", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX", BankOperationCode: "FAST"}},
		{"charges", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX", DetailsOfCharges: "SPLIT"}},
		{"ordering BIC", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX", OrderingInstitutionBIC: "XX"}},
		{"remit lines", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX", RemittanceInfo: "1\n2\n3\n4\n5"}},
		{"name lines", MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX", BeneficiaryName: []string{"1", "2", "3", "4"}}},
	} {
		if _, err := l.ExportMT103(p.ID, tc.opts); !errors.Is(err, ErrMT103Config) {
			t.Errorf("%s: err = %v, want ErrMT103Config", tc.name, err)
		}
	}
}

func TestExportMT103_Amount(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt103-amt", 1)

	msg, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"})
	if err != nil {
		t.Fatalf("ExportMT103: %v", err)
	}
	f := parseMT103Back(t, msg)
	if got := f[":32A:"][0]; !strings.HasSuffix(got, "USD0,01") {
		t.Errorf(":32A: = %q, want suffix USD0,01", got)
	}
	// The comma is mandatory: amounts never render with a dot.
	if strings.Contains(f[":32A:"][0], ".") {
		t.Errorf(":32A: must use a comma decimal separator: %q", f[":32A:"][0])
	}
}

func TestExportMT103_ReferenceRules(t *testing.T) {
	l := mt103TestLedger(t)
	// Payout IDs are operator-chosen; over-16 :20: references are
	// refused rather than truncated.
	p := mt103SubmittedPayout(t, l, "po-mt103-reference-too-long", 100_000)
	if _, err := l.ExportMT103(p.ID, MT103ExportOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT103Reference) {
		t.Errorf("long ID: err = %v, want ErrMT103Reference", err)
	}
	p2err := mt103SenderReference("/leading-slash")
	if !errors.Is(p2err, ErrMT103Reference) {
		t.Errorf("leading slash: err = %v, want ErrMT103Reference", p2err)
	}
	if err := mt103SenderReference("ref:20-ok"); err != nil {
		t.Errorf("colon inside reference: unexpected err = %v", err)
	}
	if err := mt103SenderReference("a//b"); !errors.Is(err, ErrMT103Reference) {
		t.Errorf("double slash: err = %v, want ErrMT103Reference", err)
	}
}
