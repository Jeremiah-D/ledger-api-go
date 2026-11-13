package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pain012Fixture builds a ledger with an audit log and one active
// mandate (m-012, cust -> merchant), ready for a pain.012 report.
func pain012Fixture(t *testing.T) (*Ledger, *AuditLog, Mandate) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	day := pain008Day()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	m, err := l.CreateMandate(Mandate{
		ID:             "m-012",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerMonth,
		ValidFrom:      day,
		ValidUntil:     day.AddDate(1, 0, 0),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, al, m
}

// pain012DetailFrag renders one UndrlygAccptncDtls fragment: the
// original message reference, the acceptance result, and the original
// mandate identification. accepted=true renders <Accptd>true</Accptd>;
// accepted=false renders <Accptd>false</Accptd> with the bank's
// rejection reason. Pass accepted as nil to omit Accptd entirely
// (an empty verdict).
func pain012DetailFrag(msgNmID, orgnlMsgID, mandateID string, accepted *bool, reasonCode, reasonInfo string) string {
	var sb strings.Builder
	sb.WriteString("  <UndrlygAccptncDtls>\n")
	sb.WriteString("    <OrgnlMsgInf>\n")
	sb.WriteString("      <MsgId>" + orgnlMsgID + "</MsgId>\n")
	sb.WriteString("      <MsgNmId>" + msgNmID + "</MsgNmId>\n")
	sb.WriteString("    </OrgnlMsgInf>\n")
	sb.WriteString("    <AccptncRslt>\n")
	if accepted != nil {
		if *accepted {
			sb.WriteString("      <Accptd>true</Accptd>\n")
		} else {
			sb.WriteString("      <Accptd>false</Accptd>\n")
		}
	}
	if reasonCode != "" {
		sb.WriteString("      <RjctRsn><Cd>" + reasonCode + "</Cd></RjctRsn>\n")
	}
	if reasonInfo != "" {
		sb.WriteString("      <AddtlRjctRsnInf>" + reasonInfo + "</AddtlRjctRsnInf>\n")
	}
	sb.WriteString("    </AccptncRslt>\n")
	sb.WriteString("    <OrgnlMndt>\n")
	sb.WriteString("      <OrgnlMndtId>" + mandateID + "</OrgnlMndtId>\n")
	sb.WriteString("    </OrgnlMndt>\n")
	sb.WriteString("  </UndrlygAccptncDtls>")
	return sb.String()
}

// pain012TestDoc renders a pain.012.001.03 test document with the
// given report MsgId and detail fragments.
func pain012TestDoc(reportMsgID string, details ...string) []byte {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.012.001.03">` + "\n")
	sb.WriteString("  <MndtAccptncRpt>\n")
	sb.WriteString("    <GrpHdr>\n")
	sb.WriteString("      <MsgId>" + reportMsgID + "</MsgId>\n")
	sb.WriteString("      <CreDtTm>2026-11-13T09:00:00Z</CreDtTm>\n")
	sb.WriteString("    </GrpHdr>\n")
	sb.WriteString(strings.Join(details, "\n"))
	sb.WriteString("\n  </MndtAccptncRpt>\n")
	sb.WriteString("</Document>\n")
	return []byte(sb.String())
}

func pain012Bool(b bool) *bool { return &b }

// The pinned version is pain.012.001.03: no V02 was ever registered
// (see the version note in pain012.go), and the root is
// <MndtAccptncRpt>.
func TestParsePain012_XmlnsAndRoot(t *testing.T) {
	if pain012XMLNS != "urn:iso:std:iso:20022:tech:xsd:pain.012.001.03" {
		t.Errorf("pain012XMLNS = %q, want the registered .001.03 namespace", pain012XMLNS)
	}
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain009-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(true), "", ""))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	if report.MsgID != "RPT-012" {
		t.Errorf("MsgID = %q, want RPT-012", report.MsgID)
	}
	if len(report.Verdicts) != 1 {
		t.Fatalf("len(Verdicts) = %d, want 1", len(report.Verdicts))
	}
	if !strings.Contains(string(doc), "<MndtAccptncRpt>") {
		t.Error("test document is missing the <MndtAccptncRpt> root")
	}
}

// Each of the three original message families parses, with its own
// MsgId prefix: pain009-/pain010-/pain011-.
func TestParsePain012_OriginalPrefixes(t *testing.T) {
	cases := []struct {
		name       string
		msgNmID    string
		orgnlMsgID string
	}{
		{"pain009", "pain.009.001.02", "pain009-m-012-DE98ZZZ09999999999"},
		{"pain010", "pain.010.001.02", "pain010-m-012-2026-11-13"},
		{"pain011", "pain.011.001.03", "pain011-m-012-DE98ZZZ09999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := pain012TestDoc("RPT-012-"+tc.name,
				pain012DetailFrag(tc.msgNmID, tc.orgnlMsgID, "m-012", pain012Bool(true), "", ""))
			report, err := ParsePain012(doc)
			if err != nil {
				t.Fatalf("ParsePain012: %v", err)
			}
			v := report.Verdicts[0]
			if v.MandateID != "m-012" || v.OrgnlMsgID != tc.orgnlMsgID || v.OrgnlMsgNmID != tc.msgNmID {
				t.Errorf("verdict = %+v, want mandate m-012 answering %s / %s", v, tc.orgnlMsgID, tc.msgNmID)
			}
			if v.Verdict != Pain012VerdictAccepted {
				t.Errorf("verdict = %q, want accepted", v.Verdict)
			}
		})
	}
}

// A report answering a message outside the pain.009/010/011 family
// is refused: applying it would misattribute the bank's verdict.
func TestParsePain012_UnexpectedMsgName(t *testing.T) {
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.008.001.02", "pain008-m-012", "m-012", pain012Bool(true), "", ""))
	if _, err := ParsePain012(doc); !errors.Is(err, ErrPain012UnexpectedMsgName) {
		t.Errorf("ParsePain012 err = %v, want ErrPain012UnexpectedMsgName", err)
	}
}

// A report whose OrgnlMsgId prefix does not match its OrgnlMsgNmId
// is refused: one family's verdict must never land on another
// family's request.
func TestParsePain012_MsgNameMismatch(t *testing.T) {
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain011-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(true), "", ""))
	if _, err := ParsePain012(doc); !errors.Is(err, ErrPain012MsgNameMismatch) {
		t.Errorf("ParsePain012 err = %v, want ErrPain012MsgNameMismatch", err)
	}
}

// An empty verdict — AccptncRslt with no Accptd — is fail-closed,
// never read as an acceptance or a rejection.
func TestParsePain012_EmptyVerdict(t *testing.T) {
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain009-m-012-DE98ZZZ09999999999", "m-012", nil, "MD01", "No mandate"))
	if _, err := ParsePain012(doc); !errors.Is(err, ErrInvalidPain012Verdict) {
		t.Errorf("ParsePain012 err = %v, want ErrInvalidPain012Verdict", err)
	}
	if _, err := ParsePain012(nil); !errors.Is(err, ErrInvalidPain012) {
		t.Errorf("ParsePain012(nil) err = %v, want ErrInvalidPain012", err)
	}
}

// A report for a mandate the ledger never registered is a client
// error, not a silent drop.
func TestApplyPain012_UnknownMandate(t *testing.T) {
	l, al, _ := pain012Fixture(t)
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain009-m-ghost-DE98ZZZ09999999999", "m-ghost", pain012Bool(true), "", ""))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	if _, err := l.ApplyPain012Report(report, "bank-ops"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("ApplyPain012Report err = %v, want ErrMandateNotFound", err)
	}
	// The refused batch records nothing.
	events := readAuditOps(t, al)
	if countOp(events, "pain012_mandate_accepted") != 0 || countOp(events, "pain012_mandate_rejected") != 0 {
		t.Error("refused report emitted a pain012 audit event")
	}
}

// An accepted verdict lands in the receipt, the registry, and the
// hash-chained audit trail — and the mandate row is untouched.
func TestApplyPain012_Accepted(t *testing.T) {
	l, al, m := pain012Fixture(t)
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain009-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(true), "", ""))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	receipt, err := l.ApplyPain012Report(report, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyPain012Report: %v", err)
	}
	if receipt.MsgID != "RPT-012" || receipt.Replayed {
		t.Errorf("receipt = %+v, want msg_id RPT-012, replayed false", receipt)
	}
	if len(receipt.Verdicts) != 1 || receipt.Verdicts[0].Verdict != Pain012VerdictAccepted {
		t.Errorf("receipt verdicts = %+v, want one accepted verdict", receipt.Verdicts)
	}
	acc, ok := l.MandateAcceptance("m-012")
	if !ok {
		t.Fatal("MandateAcceptance: no acceptance recorded")
	}
	if acc.ReportMsgID != "RPT-012" || acc.Verdict != Pain012VerdictAccepted ||
		acc.OrgnlMsgNmID != "pain.009.001.02" || acc.Actor != "bank-ops" || acc.At.IsZero() {
		t.Errorf("acceptance = %+v, want the RPT-012 accepted verdict by bank-ops", acc)
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Status != MandateStatusActive {
		t.Errorf("mandate status = %q after acceptance, want active (a report records, never moves the row)", got.Status)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "pain012_mandate_accepted")
	if ev == nil {
		t.Fatal("pain012_mandate_accepted audit event missing")
	}
	if countOp(events, "pain012_mandate_accepted") != 1 {
		t.Errorf("pain012_mandate_accepted count = %d, want 1", countOp(events, "pain012_mandate_accepted"))
	}
	if ev.TraceID != "RPT-012" {
		t.Errorf("trace_id = %q, want the report MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != "RPT-012" ||
		ev.Details["mandate_id"] != "m-012" ||
		ev.Details["orgnl_msg_nm_id"] != "pain.009.001.02" ||
		ev.Details["verdict"] != "accepted" ||
		ev.Details["reason_code"] != "" {
		t.Errorf("audit details = %v, want msg_id/mandate_id/orgnl_msg_nm_id/verdict/reason_code", ev.Details)
	}
}

// A rejected verdict carries the bank's reason verbatim into the
// receipt, the registry, and the audit event — code plus free text,
// never interpreted.
func TestApplyPain012_Rejected(t *testing.T) {
	l, al, _ := pain012Fixture(t)
	doc := pain012TestDoc("RPT-012-RJCT",
		pain012DetailFrag("pain.011.001.03", "pain011-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(false), "MD01", "No mandate"))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	receipt, err := l.ApplyPain012Report(report, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyPain012Report: %v", err)
	}
	v := receipt.Verdicts[0]
	if v.Verdict != Pain012VerdictRejected {
		t.Errorf("verdict = %q, want rejected", v.Verdict)
	}
	if v.ReasonCode != "MD01" || v.ReasonInfo != "No mandate" || v.Reason != "MD01: No mandate" {
		t.Errorf("reason = code %q info %q rendered %q, want MD01 / No mandate / MD01: No mandate",
			v.ReasonCode, v.ReasonInfo, v.Reason)
	}
	acc, ok := l.MandateAcceptance("m-012")
	if !ok {
		t.Fatal("MandateAcceptance: no acceptance recorded")
	}
	if acc.Verdict != Pain012VerdictRejected || acc.Reason != "MD01: No mandate" {
		t.Errorf("acceptance = %+v, want the rejection with the bank's reason verbatim", acc)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "pain012_mandate_rejected")
	if ev == nil {
		t.Fatal("pain012_mandate_rejected audit event missing")
	}
	if ev.Details["reason_code"] != "MD01" || ev.Details["verdict"] != "rejected" {
		t.Errorf("audit details = %v, want verdict rejected and reason_code MD01", ev.Details)
	}
}

// Re-applying the same report is a no-op: the original receipt comes
// back flagged as a replay, and no second audit event is emitted.
func TestApplyPain012_ReplayIdempotent(t *testing.T) {
	l, al, _ := pain012Fixture(t)
	doc := pain012TestDoc("RPT-012",
		pain012DetailFrag("pain.009.001.02", "pain009-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(true), "", ""))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	if _, err := l.ApplyPain012Report(report, "bank-ops"); err != nil {
		t.Fatalf("first ApplyPain012Report: %v", err)
	}
	receipt, err := l.ApplyPain012Report(report, "bank-ops")
	if err != nil {
		t.Fatalf("second ApplyPain012Report: %v", err)
	}
	if !receipt.Replayed {
		t.Error("replay receipt has replayed=false, want true")
	}
	if receipt.MsgID != "RPT-012" || len(receipt.Verdicts) != 1 || receipt.Verdicts[0].Verdict != Pain012VerdictAccepted {
		t.Errorf("replay receipt = %+v, want the original accepted receipt", receipt)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain012_mandate_accepted") != 1 {
		t.Errorf("pain012_mandate_accepted count = %d, want 1 (replay must add none)",
			countOp(events, "pain012_mandate_accepted"))
	}
}

// A rejection is a record, not a cancellation: the mandate stays
// active until an operator decides through CancelMandate.
func TestApplyPain012_RejectionKeepsMandateStatus(t *testing.T) {
	l, _, m := pain012Fixture(t)
	doc := pain012TestDoc("RPT-012-RJCT",
		pain012DetailFrag("pain.011.001.03", "pain011-m-012-DE98ZZZ09999999999", "m-012", pain012Bool(false), "MD01", "No mandate"))
	report, err := ParsePain012(doc)
	if err != nil {
		t.Fatalf("ParsePain012: %v", err)
	}
	if _, err := l.ApplyPain012Report(report, "bank-ops"); err != nil {
		t.Fatalf("ApplyPain012Report: %v", err)
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Status != MandateStatusActive {
		t.Errorf("mandate status = %q after RJCT, want active (rejection never cancels)", got.Status)
	}
}
