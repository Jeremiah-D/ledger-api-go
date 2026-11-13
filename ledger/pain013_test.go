package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// pain013Fixture builds a ledger with one issued RfP (merchant
// asks customer for USD 50.00, expiring in an hour) and registered
// IBANs with BICs on both sides — the full pain.013.001.07 input.
func pain013Fixture(t *testing.T) (*Ledger, RfP) {
	t.Helper()
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	r, dup, err := l.CreateRfP(ctx, RfP{
		ID:          "rfp-013",
		Creditor:    "merchant",
		Debtor:      "customer",
		AmountCents: 5000,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil || dup {
		t.Fatalf("CreateRfP: %v dup=%v", err, dup)
	}
	return l, r
}

func TestExportPain013_HappyPath(t *testing.T) {
	l, r := pain013Fixture(t)

	doc, err := l.ExportPain013(r.ID)
	if err != nil {
		t.Fatalf("ExportPain013: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.013.001.07">`,
		"<CdtrPmtActvtnReq>",
		"<MsgId>pain013-rfp-013</MsgId>",
		"<NbOfTxs>1</NbOfTxs>",
		"<InitgPty><Nm>LEDGER</Nm></InitgPty>",
		"<PmtInfId>pain013-rfp-013</PmtInfId>",
		"<PmtMtd>TRF</PmtMtd>",
		// ReqdExctnDt is a DateAndDateTime2Choice: the RfP's
		// creation date as <Dt>.
		"<ReqdExctnDt><Dt>" + r.CreatedAt.UTC().Format("2006-01-02") + "</Dt></ReqdExctnDt>",
		// XpryDt is a DateAndDateTime2Choice: the full ExpiresAt
		// timestamp as <DtTm>, second-precise.
		"<XpryDt><DtTm>" + r.ExpiresAt.UTC().Format(time.RFC3339) + "</DtTm></XpryDt>",
		"<Dbtr><Nm>customer</Nm></Dbtr>",
		"<DbtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></DbtrAcct>",
		"<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>",
		"<PmtId><EndToEndId>rfp-013</EndToEndId></PmtId>",
		`<Amt><InstdAmt Ccy="USD">50.00</InstdAmt></Amt>`,
		"<ChrgBr>SLEV</ChrgBr>",
		"<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>",
		"<Cdtr><Nm>merchant</Nm></Cdtr>",
		"<CdtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></CdtrAcct>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	// The verified V07 nesting: Cdtr/CdtrAcct and Amt live inside
	// CdtTrfTx, not as direct PmtInf children; XpryDt and
	// Dbtr/DbtrAcct are direct PmtInf children. PmtTpInf is
	// deliberately omitted (see pain013.go).
	if idx := strings.Index(s, "<CdtTrfTx>"); idx < 0 {
		t.Fatal("document missing <CdtTrfTx>")
	} else {
		head, tail := s[:idx], s[idx:]
		for _, pmtInfOnly := range []string{"<XpryDt>", "<Dbtr>", "<DbtrAcct>", "<DbtrAgt>"} {
			if !strings.Contains(head, pmtInfOnly) {
				t.Errorf("PmtInf-level %s missing before <CdtTrfTx>", pmtInfOnly)
			}
			if strings.Contains(tail, pmtInfOnly) {
				t.Errorf("PmtInf-level %s leaked into CdtTrfTx", pmtInfOnly)
			}
		}
		for _, txOnly := range []string{"<Cdtr>", "<CdtrAcct>", "<Amt>", "<EndToEndId>"} {
			if strings.Contains(head, txOnly) {
				t.Errorf("transaction-level %s rendered at PmtInf level", txOnly)
			}
			if !strings.Contains(tail, txOnly) {
				t.Errorf("transaction-level %s missing inside <CdtTrfTx>", txOnly)
			}
		}
	}
	if strings.Contains(s, "<PmtTpInf>") {
		t.Error("document carries a PmtTpInf block: the ledger asserts no scheme service level")
	}
	if strings.Contains(s, "<InstrId>") || strings.Contains(s, "<UltmtDbtr>") {
		t.Error("document carries an element the ledger holds no value for (InstrId/UltmtDbtr)")
	}
	// XSD element order inside PmtInf and inside CdtTrfTx.
	ordered := func(block string, elems ...string) {
		t.Helper()
		prev := -1
		for _, e := range elems {
			i := strings.Index(block, e)
			if i < 0 {
				t.Errorf("document missing %q", e)
				return
			}
			if i <= prev {
				t.Errorf("element %q out of XSD order", e)
				return
			}
			prev = i
		}
	}
	ordered(s, "<PmtInfId>", "<PmtMtd>", "<ReqdExctnDt>", "<XpryDt>", "<Dbtr>", "<DbtrAcct>", "<DbtrAgt>", "<CdtTrfTx>")
	ordered(s, "<PmtId>", "<Amt>", "<ChrgBr>", "<CdtrAgt>", "<Cdtr>", "<CdtrAcct>")
	if pain013MsgID(r.ID) != "pain013-rfp-013" {
		t.Errorf("pain013MsgID = %q, want pain013-rfp-013", pain013MsgID(r.ID))
	}
}

func TestExportPain013_FailClosed(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)

	// Unknown RfP.
	if _, err := l.ExportPain013("rfp-ghost"); !errors.Is(err, ErrRfPNotFound) {
		t.Errorf("unknown RfP: err = %v, want ErrRfPNotFound", err)
	}

	// A paid RfP is terminal: the request has already been
	// answered, and re-rendering it would ask for money twice.
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	if _, err := l.ExportPain013(r.ID); !errors.Is(err, ErrRfPNotIssued) {
		t.Errorf("paid RfP: err = %v, want ErrRfPNotIssued", err)
	}

	// A rejected RfP is terminal too.
	l2, r2 := pain013Fixture(t)
	if _, err := l2.RejectRfP(ctx, r2.ID, "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}
	if _, err := l2.ExportPain013(r2.ID); !errors.Is(err, ErrRfPNotIssued) {
		t.Errorf("rejected RfP: err = %v, want ErrRfPNotIssued", err)
	}

	// A swept-expired RfP refuses with the expiry error.
	l3, r3 := pain013Fixture(t)
	if n := l3.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireRfPs = %d, want 1", n)
	}
	if _, err := l3.ExportPain013(r3.ID); !errors.Is(err, ErrRfPExpired) {
		t.Errorf("swept-expired RfP: err = %v, want ErrRfPExpired", err)
	}

	// Lazy expiry: an issued RfP whose deadline has passed reads
	// as expired even before any sweep runs.
	l4, r4 := pain013Fixture(t)
	l4.mu.Lock()
	stale := l4.rfps[r4.ID]
	stale.ExpiresAt = time.Now().Add(-time.Second)
	l4.rfps[r4.ID] = stale
	l4.mu.Unlock()
	if _, err := l4.ExportPain013(r4.ID); !errors.Is(err, ErrRfPExpired) {
		t.Errorf("lazily expired RfP: err = %v, want ErrRfPExpired", err)
	}
}

func TestExportPain013_MissingIBANOrBIC(t *testing.T) {
	ctx := context.Background()

	// No registered IBANs on either side: the accounts cannot be
	// identified on the wire.
	l := New()
	r, _, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-013-noiban", Creditor: "merchant", Debtor: "customer",
		AmountCents: 5000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}
	if _, err := l.ExportPain013(r.ID); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("no IBANs: err = %v, want ErrMissingAccountIBAN", err)
	}

	// IBANs without BICs: V07 makes DbtrAgt/CdtrAgt mandatory,
	// so the export refuses rather than emitting an agent block
	// with an invented identifier.
	mustSetIBAN(t, l, "customer", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.ExportPain013(r.ID); !errors.Is(err, ErrMissingPain013AgentBIC) {
		t.Errorf("no BICs: err = %v, want ErrMissingPain013AgentBIC", err)
	}
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	if _, err := l.ExportPain013(r.ID); !errors.Is(err, ErrMissingPain013AgentBIC) {
		t.Errorf("creditor BIC missing: err = %v, want ErrMissingPain013AgentBIC", err)
	}
}

func TestExportPain013_DeterministicExceptCreDtTm(t *testing.T) {
	l, r := pain013Fixture(t)

	first, err := l.ExportPain013(r.ID)
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain013(r.ID)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	stripCreDtTm := func(doc []byte) string {
		var lines []string
		for _, line := range strings.Split(string(doc), "\n") {
			if !strings.HasPrefix(line, "<CreDtTm>") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if stripCreDtTm(first) != stripCreDtTm(second) {
		t.Errorf("repeat exports differ apart from CreDtTm:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(second), "<MsgId>pain013-rfp-013</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPain013_ReadOnly(t *testing.T) {
	l, r := pain013Fixture(t)

	before := l.Version()
	if _, err := l.ExportPain013(r.ID); err != nil {
		t.Fatalf("ExportPain013: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetRfP(r.ID)
	if !ok {
		t.Fatal("GetRfP: RfP vanished")
	}
	if got.Status != RfPStatusIssued {
		t.Errorf("RfP status = %q after export, want issued (export is a request, not an acceptance)", got.Status)
	}
}

func TestExportPain013_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	ctx := context.Background()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	if _, _, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-013-audit", Creditor: "merchant", Debtor: "customer",
		AmountCents: 5000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}

	if _, err := l.ExportPain013("rfp-013-audit"); err != nil {
		t.Fatalf("ExportPain013: %v", err)
	}
	events := readAuditOps(t, al)
	ev := findOp(events, "pain013_exported")
	if ev == nil {
		t.Fatal("pain013_exported audit event missing")
	}
	if countOp(events, "pain013_exported") != 1 {
		t.Errorf("pain013_exported count = %d, want 1", countOp(events, "pain013_exported"))
	}
	if ev.TraceID != pain013MsgID("rfp-013-audit") {
		t.Errorf("trace_id = %q, want the deterministic MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != pain013MsgID("rfp-013-audit") ||
		ev.Details["rfp_id"] != "rfp-013-audit" {
		t.Errorf("audit details = %v, want msg_id/rfp_id", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("audit version moved %d -> %d on a read-only export", ev.VersionBefore, ev.VersionAfter)
	}
}
