package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// camt026Doc renders one inbound camt.026.001.07 Unable to Apply
// report: Assgnmt/Id msgID, the Undrlyg arm named by arm
// ("initn" | "intrbk" | "stmtntry") carrying uetr and/or endToEndID
// (empty strings are omitted), the underlying amount when amount is
// non-empty, and a MssngOrIncrrctInf justification with the first
// MssngInf Cd reasonCd and AddtlMssngInf reasonInfo.
func camt026Doc(msgID, arm, uetr, endToEndID, amount, ccy, reasonCd, reasonInfo string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.026.001.07">`)
	sb.WriteString(`<UblToApply><Assgnmt><Id>` + msgID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>BANKDEFFXXX</Nm></Assgnr><Assgne><Nm>our-bank</Nm></Assgne>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Case><Id>case-` + msgID + `</Id></Case>`)
	sb.WriteString(`<Undrlyg>`)
	switch arm {
	case "intrbk":
		sb.WriteString(`<IntrBk><OrgnlGrpInf><OrgnlMsgId>orig-msg-1</OrgnlMsgId>`)
		sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.08</OrgnlMsgNmId></OrgnlGrpInf>`)
		if endToEndID != "" {
			sb.WriteString(`<OrgnlEndToEndId>` + endToEndID + `</OrgnlEndToEndId>`)
		}
		if uetr != "" {
			sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
		}
		if amount != "" {
			sb.WriteString(`<OrgnlIntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</OrgnlIntrBkSttlmAmt>`)
		}
		sb.WriteString(`</IntrBk>`)
	case "stmtntry":
		sb.WriteString(`<StmtNtry><OrgnlGrpInf><OrgnlMsgId>orig-msg-1</OrgnlMsgId>`)
		sb.WriteString(`<OrgnlMsgNmId>camt.053.001.08</OrgnlMsgNmId></OrgnlGrpInf>`)
		sb.WriteString(`<OrgnlStmtId>stmt-1</OrgnlStmtId><OrgnlNtryId>ntry-1</OrgnlNtryId>`)
		if uetr != "" {
			sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
		}
		sb.WriteString(`</StmtNtry>`)
	default: // initn
		sb.WriteString(`<Initn><OrgnlGrpInf><OrgnlMsgId>orig-msg-1</OrgnlMsgId>`)
		sb.WriteString(`<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId></OrgnlGrpInf>`)
		if endToEndID != "" {
			sb.WriteString(`<OrgnlEndToEndId>` + endToEndID + `</OrgnlEndToEndId>`)
		}
		if uetr != "" {
			sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
		}
		if amount != "" {
			sb.WriteString(`<OrgnlInstdAmt Ccy="` + ccy + `">` + amount + `</OrgnlInstdAmt>`)
		}
		sb.WriteString(`</Initn>`)
	}
	sb.WriteString(`</Undrlyg>`)
	sb.WriteString(`<Justfn><MssngOrIncrrctInf><MssngInf><Cd>` + reasonCd + `</Cd>`)
	if reasonInfo != "" {
		sb.WriteString(`<AddtlMssngInf>` + reasonInfo + `</AddtlMssngInf>`)
	}
	sb.WriteString(`</MssngInf></MssngOrIncrrctInf></Justfn>`)
	sb.WriteString(`</UblToApply></Document>`)
	return sb.String()
}

// camt026JustfnDoc renders a camt.026 report whose Justfn arm is
// justKind: "anyinf" (AnyInf true) or "dup" (PssblDplctInstr true).
func camt026JustfnDoc(msgID, justKind string) string {
	doc := camt026Doc(msgID, "initn", "uetr-justfn", "", "10.00", "USD", "MS01", "")
	switch justKind {
	case "anyinf":
		start := strings.Index(doc, "<Justfn>")
		end := strings.Index(doc, "</Justfn>") + len("</Justfn>")
		doc = doc[:start] + `<Justfn><AnyInf>true</AnyInf></Justfn>` + doc[end:]
	case "dup":
		start := strings.Index(doc, "<Justfn>")
		end := strings.Index(doc, "</Justfn>") + len("</Justfn>")
		doc = doc[:start] + `<Justfn><PssblDplctInstr>true</PssblDplctInstr></Justfn>` + doc[end:]
	}
	return doc
}

// camt026Fixture builds the payout ledger (funded saver, payout
// clearing configured) with an audit log attached, so the inbound
// audit marker can be asserted precisely.
func camt026Fixture(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	return l, al
}

// Parse carries the wire through: the Initn arm's references,
// amount, case, assignor, and the justification's coded reason.
func TestParseCAMT026_Initn(t *testing.T) {
	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-p-1", "initn", "uetr-1", "e2e-1", "2500.00", "USD", "MS01", "missing creditor account")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if rep.MsgID != "camt026-p-1" {
		t.Errorf("msg_id = %q, want camt026-p-1", rep.MsgID)
	}
	if rep.CaseID != "case-camt026-p-1" {
		t.Errorf("case_id = %q, want case-camt026-p-1", rep.CaseID)
	}
	if rep.Assignor != "BANKDEFFXXX" || rep.Assignee != "our-bank" {
		t.Errorf("assignor/assignee = %q/%q, want BANKDEFFXXX/our-bank", rep.Assignor, rep.Assignee)
	}
	if rep.UnderlyingKind != CAMT026UnderlyingInitiation {
		t.Errorf("kind = %q, want initiation", rep.UnderlyingKind)
	}
	if rep.UETR != "uetr-1" || rep.EndToEndID != "e2e-1" {
		t.Errorf("reference = %q/%q, want uetr-1/e2e-1", rep.UETR, rep.EndToEndID)
	}
	if rep.OrgnlMsgID != "orig-msg-1" || rep.OrgnlMsgNmID != "pain.001.001.03" {
		t.Errorf("original = %q/%q, want orig-msg-1/pain.001.001.03", rep.OrgnlMsgID, rep.OrgnlMsgNmID)
	}
	if !rep.HasAmount || rep.AmountCents != 250000 || rep.Currency != "USD" {
		t.Errorf("amount = %v %d %q, want has 250000 USD", rep.HasAmount, rep.AmountCents, rep.Currency)
	}
	if rep.ReasonCode != "MS01" || rep.ReasonInfo != "missing creditor account" {
		t.Errorf("reason = %q/%q, want MS01/missing creditor account", rep.ReasonCode, rep.ReasonInfo)
	}
	if rep.Justification != CAMT026JustificationMissingOrIncorrect {
		t.Errorf("justification = %q, want missing-or-incorrect", rep.Justification)
	}
}

// The other two Undrlyg arms parse: IntrBk with its settlement
// amount, StmtNtry with its statement/entry references.
func TestParseCAMT026_OtherArms(t *testing.T) {
	intr, err := ParseCAMT026([]byte(camt026Doc("camt026-p-2", "intrbk", "uetr-2", "e2e-2", "100.50", "EUR", "AM09", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026 intrbk: %v", err)
	}
	if intr.UnderlyingKind != CAMT026UnderlyingInterbank {
		t.Errorf("kind = %q, want interbank", intr.UnderlyingKind)
	}
	if !intr.HasAmount || intr.AmountCents != 10050 || intr.Currency != "EUR" {
		t.Errorf("amount = %v %d %q, want has 10050 EUR", intr.HasAmount, intr.AmountCents, intr.Currency)
	}
	stmt, err := ParseCAMT026([]byte(camt026Doc("camt026-p-3", "stmtntry", "uetr-3", "", "", "", "MS02", "unexpected entry")))
	if err != nil {
		t.Fatalf("ParseCAMT026 stmtntry: %v", err)
	}
	if stmt.UnderlyingKind != CAMT026UnderlyingStatementEntry {
		t.Errorf("kind = %q, want statement-entry", stmt.UnderlyingKind)
	}
	if stmt.HasAmount {
		t.Error("statement-entry report reports an amount")
	}
	if stmt.UETR != "uetr-3" || stmt.EndToEndID != "" {
		t.Errorf("reference = %q/%q, want uetr-3/empty", stmt.UETR, stmt.EndToEndID)
	}
}

// Justification arms without a code parse: AnyInf asks for all
// available information, PssblDplctInstr flags a possible duplicate.
func TestParseCAMT026_Justifications(t *testing.T) {
	any, err := ParseCAMT026([]byte(camt026JustfnDoc("camt026-p-4", "anyinf")))
	if err != nil {
		t.Fatalf("ParseCAMT026 anyinf: %v", err)
	}
	if any.Justification != CAMT026JustificationAnyInformation || any.ReasonCode != "" {
		t.Errorf("justification = %q reason %q, want any-information/empty", any.Justification, any.ReasonCode)
	}
	dup, err := ParseCAMT026([]byte(camt026JustfnDoc("camt026-p-5", "dup")))
	if err != nil {
		t.Fatalf("ParseCAMT026 dup: %v", err)
	}
	if dup.Justification != CAMT026JustificationPossibleDuplicate || dup.ReasonCode != "" {
		t.Errorf("justification = %q reason %q, want possible-duplicate/empty", dup.Justification, dup.ReasonCode)
	}
}

// Illegal documents are refused as a whole: the wrong schema
// version, no underlying arm (or two — the choice is exclusive), no
// reference, no justification (or two), malformed XML, and an
// unreadable amount never reach the apply.
func TestParseCAMT026_IllegalDocuments(t *testing.T) {
	good := camt026Doc("camt026-il-1", "initn", "uetr-il", "e2e-il", "10.00", "USD", "MS01", "")
	twoArms := strings.Replace(good, "</Initn>", `</Initn><IntrBk><OrgnlUETR>uetr-il</OrgnlUETR></IntrBk>`, 1)
	// Two justification arms inside one Justfn block: the choice is
	// exclusive, and a single block carrying both is detectable.
	twoJustfn := strings.Replace(good, "<MssngOrIncrrctInf>", `<AnyInf>true</AnyInf><MssngOrIncrrctInf>`, 1)
	// An empty Undrlyg block: no arm at all.
	noUndrlyg := good[:strings.Index(good, "<Undrlyg>")] + "<Undrlyg></Undrlyg>" + good[strings.Index(good, "</Undrlyg>")+len("</Undrlyg>"):]
	noJustfn := good[:strings.Index(good, "<Justfn>")] + "</UblToApply></Document>"
	cases := []struct {
		name string
		doc  string
	}{
		{"empty document", ""},
		{"malformed XML", "<Document><broken"},
		{"wrong schema version", strings.Replace(good, "camt.026.001.07", "camt.026.001.10", 1)},
		{"no underlying arm", noUndrlyg},
		{"two underlying arms", twoArms},
		{"no reference", camt026Doc("camt026-il-3", "initn", "", "", "10.00", "USD", "MS01", "")},
		{"no justification", noJustfn},
		{"two justifications", twoJustfn},
		{"missing MsgId", strings.Replace(good, "<Id>camt026-il-1</Id>", "", 1)},
		{"unreadable amount", camt026Doc("camt026-il-4", "initn", "uetr-il", "e2e-il", "ten", "USD", "MS01", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCAMT026([]byte(tc.doc)); !errors.Is(err, ErrInvalidCAMT026) {
				t.Errorf("ParseCAMT026: %v, want ErrInvalidCAMT026", err)
			}
		})
	}
}

// Apply to a submitted payout via UETR: the report is recorded,
// the audit marker is precise, and the payout itself is untouched.
func TestApplyCAMT026_Payout(t *testing.T) {
	l, al := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-1")

	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-1", "initn", p.UETR, "", "2500.00", "USD", "MS01", "missing creditor account")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res, err := l.ApplyCAMT026(rep, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if res.Kind != CAMT026AssociatedPayout || res.TargetID != p.ID {
		t.Errorf("association = %q/%q, want payout/po-026-1", res.Kind, res.TargetID)
	}
	if res.Terminal {
		t.Error("submitted payout reports Terminal, want false")
	}
	if res.Duplicate {
		t.Error("first apply reports Duplicate")
	}
	if res.ReasonCode != "MS01" || res.Justification != CAMT026JustificationMissingOrIncorrect {
		t.Errorf("reason = %q/%q, want MS01/missing-or-incorrect", res.ReasonCode, res.Justification)
	}
	// The payout is untouched: still submitted, same amount.
	if got, _ := l.GetPayout(p.ID); got.Status != PayoutSubmitted {
		t.Errorf("payout status = %q, want submitted", got.Status)
	}
	// The record round-trips through the getter.
	if got, ok := l.GetCAMT026Report("camt026-a-1"); !ok || got.TargetID != p.ID {
		t.Errorf("GetCAMT026Report = %+v, want the recorded payout report", got)
	}

	events := readAuditOps(t, al)
	if countOp(events, "camt026_unable_to_apply") != 1 {
		t.Errorf("camt026_unable_to_apply count = %d, want 1", countOp(events, "camt026_unable_to_apply"))
	}
	ev := findOp(events, "camt026_unable_to_apply")
	if ev == nil {
		t.Fatal("camt026_unable_to_apply event missing")
	}
	if ev.TraceID != "camt026-a-1" || ev.Actor != "bank-ops" {
		t.Errorf("trace/actor = %q/%q, want camt026-a-1/bank-ops", ev.TraceID, ev.Actor)
	}
	if ev.Details["kind"] != "payout" || ev.Details["target_id"] != "po-026-1" {
		t.Errorf("details = %v, want kind payout target po-026-1", ev.Details)
	}
	if ev.Details["reason_code"] != "MS01" || ev.Details["terminal"] != false {
		t.Errorf("details = %v, want reason MS01 terminal false", ev.Details)
	}
}

// Apply to a journaled transfer via EndToEndId: the transfer is
// posted, so the report lands as a terminal no-op.
func TestApplyCAMT026_Transfer(t *testing.T) {
	l, al := camt026Fixture(t)
	if _, err := l.PostTransfer(Transfer{ID: "tx-026-1", From: "saver", To: "merchant", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-2", "intrbk", "", "tx-026-1", "10.00", "USD", "AM09", "wrong amount")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res, err := l.ApplyCAMT026(rep, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if res.Kind != CAMT026AssociatedTransfer || res.TargetID != "tx-026-1" {
		t.Errorf("association = %q/%q, want transfer/tx-026-1", res.Kind, res.TargetID)
	}
	if !res.Terminal {
		t.Error("posted transfer reports Terminal false, want true")
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000 (no money moves)", got)
	}
	ev := findOp(readAuditOps(t, al), "camt026_unable_to_apply")
	if ev == nil || ev.Details["kind"] != "transfer" || ev.Details["terminal"] != true {
		t.Errorf("audit event = %+v, want kind transfer terminal true", ev)
	}
}

// Apply to an RfP via EndToEndId: an issued RfP records live, a
// paid RfP records terminal.
func TestApplyCAMT026_RfP(t *testing.T) {
	l, al := camt026Fixture(t)
	ctx := context.Background()
	r, dup, err := l.CreateRfP(ctx, RfP{ID: "rfp-026-1", Creditor: "merchant", Debtor: "saver",
		AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil || dup {
		t.Fatalf("CreateRfP: %v dup=%v", err, dup)
	}
	_ = r

	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-3", "initn", "", "rfp-026-1", "10.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res, err := l.ApplyCAMT026(rep, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if res.Kind != CAMT026AssociatedRfP || res.TargetID != "rfp-026-1" {
		t.Errorf("association = %q/%q, want rfp/rfp-026-1", res.Kind, res.TargetID)
	}
	if res.Terminal {
		t.Error("issued RfP reports Terminal, want false")
	}

	// Pay the RfP, then a second report about it is terminal.
	if _, err := l.AcceptRfP(ctx, "rfp-026-1", "saver"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	rep2, err := ParseCAMT026([]byte(camt026Doc("camt026-a-4", "initn", "", "rfp-026-1", "10.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res2, err := l.ApplyCAMT026(rep2, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if !res2.Terminal {
		t.Error("paid RfP reports Terminal false, want true")
	}
	if got, _ := l.GetRfP("rfp-026-1"); got.Status != RfPStatusPaid {
		t.Errorf("RfP status = %q, want paid (never revived)", got.Status)
	}
	if countOp(readAuditOps(t, al), "camt026_unable_to_apply") != 2 {
		t.Error("expected exactly 2 camt026_unable_to_apply events")
	}
}

// Unknown reference: refused fail-closed, the refusal is audited,
// and it blocks nothing — a later valid report still applies.
func TestApplyCAMT026_UnknownReference(t *testing.T) {
	l, al := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-2")

	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-5", "initn", "no-such-uetr", "", "10.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if _, err := l.ApplyCAMT026(rep, "bank-ops"); !errors.Is(err, ErrCAMT026UnknownReference) {
		t.Errorf("unknown UETR: %v, want ErrCAMT026UnknownReference", err)
	}
	// The refusal is on the audit trail, and no record was stored.
	events := readAuditOps(t, al)
	if countOp(events, "camt026_unable_to_apply_rejected") != 1 {
		t.Errorf("rejected count = %d, want 1", countOp(events, "camt026_unable_to_apply_rejected"))
	}
	if _, ok := l.GetCAMT026Report("camt026-a-5"); ok {
		t.Error("refused report left a record behind")
	}
	// A later valid report still applies: the refusal blocked nothing.
	rep2, err := ParseCAMT026([]byte(camt026Doc("camt026-a-6", "initn", p.UETR, "", "2500.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if _, err := l.ApplyCAMT026(rep2, "bank-ops"); err != nil {
		t.Fatalf("valid apply after refusal: %v", err)
	}
}

// Two references that disagree are refused: the report cannot name
// one instruction.
func TestApplyCAMT026_ReferenceMismatch(t *testing.T) {
	l, _ := camt026Fixture(t)
	a := submittedPayout(t, l, "po-026-3a")
	b := submittedPayout(t, l, "po-026-3b")

	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-7", "initn", a.UETR, b.ID, "2500.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	if _, err := l.ApplyCAMT026(rep, "bank-ops"); !errors.Is(err, ErrCAMT026UnknownReference) {
		t.Errorf("mismatched references: %v, want ErrCAMT026UnknownReference", err)
	}
}

// Terminal targets are recorded, audited, and left alone: a
// cancelled payout stays cancelled, and a gpi-ACCP track's payout
// is terminal even though its row still says submitted.
func TestApplyCAMT026_TerminalNoOp(t *testing.T) {
	l, al := camt026Fixture(t)

	// Cancelled payout (row-terminal).
	p := draftPayout(t, l, "po-026-4", 250_000)
	if _, err := l.CancelPayout(p.ID); err != nil {
		t.Fatalf("CancelPayout: %v", err)
	}
	rep, err := ParseCAMT026([]byte(camt026Doc("camt026-a-8", "initn", p.UETR, "", "2500.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res, err := l.ApplyCAMT026(rep, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if !res.Terminal {
		t.Error("cancelled payout reports Terminal false, want true")
	}
	if got, _ := l.GetPayout(p.ID); got.Status != PayoutCancelled {
		t.Errorf("payout status = %q, want cancelled", got.Status)
	}

	// Settled track (bank-terminal, row still submitted).
	p2 := submittedPayout(t, l, "po-026-5")
	if _, err := l.AdvanceGPI(p2.UETR, GPIStatusACCP, "", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to ACCP: %v", err)
	}
	rep2, err := ParseCAMT026([]byte(camt026Doc("camt026-a-9", "initn", p2.UETR, "", "2500.00", "USD", "MS01", "")))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	res2, err := l.ApplyCAMT026(rep2, "bank-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT026: %v", err)
	}
	if !res2.Terminal {
		t.Error("ACCP-track payout reports Terminal false, want true")
	}
	if got, _ := l.GetPayout(p2.ID); got.Status != PayoutSubmitted {
		t.Errorf("payout status = %q, want submitted (report never moves it)", got.Status)
	}
	if countOp(readAuditOps(t, al), "camt026_unable_to_apply") != 2 {
		t.Error("expected exactly 2 camt026_unable_to_apply events")
	}
}

// Replay idempotency: the same MsgId re-applied returns the stored
// result with Duplicate set — no second record, no second audit.
func TestApplyCAMT026_ReplayIdempotent(t *testing.T) {
	l, al := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-6")

	doc := camt026Doc("camt026-a-10", "initn", p.UETR, "", "2500.00", "USD", "MS01", "")
	rep, err := ParseCAMT026([]byte(doc))
	if err != nil {
		t.Fatalf("ParseCAMT026: %v", err)
	}
	first, err := l.ApplyCAMT026(rep, "bank-ops")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	rep2, err := ParseCAMT026([]byte(doc))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	second, err := l.ApplyCAMT026(rep2, "bank-ops")
	if err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	if !second.Duplicate || second.TargetID != first.TargetID {
		t.Errorf("replay = %+v, want Duplicate with the stored target", second)
	}
	if countOp(readAuditOps(t, al), "camt026_unable_to_apply") != 1 {
		t.Error("replay emitted a second camt026_unable_to_apply event")
	}
}

// A MsgId reused for a different report is a conflict, not a replay.
func TestApplyCAMT026_MsgIDConflict(t *testing.T) {
	l, _ := camt026Fixture(t)
	p := submittedPayout(t, l, "po-026-7")

	rep, _ := ParseCAMT026([]byte(camt026Doc("camt026-a-11", "initn", p.UETR, "", "2500.00", "USD", "MS01", "")))
	if _, err := l.ApplyCAMT026(rep, "bank-ops"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Same MsgId, different reason code.
	other, _ := ParseCAMT026([]byte(camt026Doc("camt026-a-11", "initn", p.UETR, "", "2500.00", "USD", "MS02", "")))
	if _, err := l.ApplyCAMT026(other, "bank-ops"); !errors.Is(err, ErrCAMT026MsgIDConflict) {
		t.Errorf("conflicting reuse: %v, want ErrCAMT026MsgIDConflict", err)
	}
}
