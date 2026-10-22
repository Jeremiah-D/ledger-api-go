package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// resolveCancellation drives a payout cancellation through request +
// resolve: the only states with a resolution to export.
func resolveCancellation(t *testing.T, l *Ledger, uetr, resolution, code, actor string) PayoutCancellation {
	t.Helper()
	if _, err := l.RequestPayoutCancellation(uetr, "DUPL", "night-ops"); err != nil {
		t.Fatalf("RequestPayoutCancellation: %v", err)
	}
	c, err := l.ResolvePayoutCancellation(uetr, resolution, code, actor)
	if err != nil {
		t.Fatalf("ResolvePayoutCancellation: %v", err)
	}
	return c
}

// parseCamt029ExportBack parses an exported camt.029 document back
// into the export wire structs: field-level assertions instead of
// whitespace-fragile string matches (see parseCamt056Back).
func parseCamt029ExportBack(t *testing.T, doc []byte) camt029ExportDocument {
	t.Helper()
	var back camt029ExportDocument
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("unmarshal exported camt.029: %v", err)
	}
	return back
}

func TestExportCAMT029Resolution_ConfirmedRoundTrip(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-1")
	c := resolveCancellation(t, l, p.UETR, CancellationConfirmed, "ACCR: funds released", "night-ops")

	doc, err := l.ExportCAMT029Resolution(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT029Resolution: %v", err)
	}
	if body := string(doc); !strings.Contains(body, "urn:iso:std:iso:20022:tech:xsd:camt.029.001.03") {
		t.Errorf("document missing camt.029.001.03 namespace:\n%s", body)
	}
	back := parseCamt029ExportBack(t, doc)
	if back.Report.Assgnmt.ID != resolutionMsgID(p.UETR, c.Seq) {
		t.Errorf("Assgnmt/Id = %q, want %q", back.Report.Assgnmt.ID, resolutionMsgID(p.UETR, c.Seq))
	}
	rslvd := back.Report.RslvdCxl
	if rslvd.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", rslvd.OrgnlUETR, p.UETR)
	}
	if rslvd.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want %q", rslvd.OrgnlMsgID, p.ID)
	}
	if rslvd.CxlSts.Cd != camt029ACCR {
		t.Errorf("CxlSts = %q, want ACCR", rslvd.CxlSts.Cd)
	}
	if rslvd.CxlStsRsnInf == nil {
		t.Fatal("CxlStsRsnInf missing for a coded resolution")
	}
	if rslvd.CxlStsRsnInf.Rsn == nil || rslvd.CxlStsRsnInf.Rsn.Cd != "ACCR" {
		t.Errorf("Rsn = %+v, want Cd ACCR", rslvd.CxlStsRsnInf.Rsn)
	}
	if rslvd.CxlStsRsnInf.AddtlInf != "funds released" {
		t.Errorf("AddtlInf = %q, want %q", rslvd.CxlStsRsnInf.AddtlInf, "funds released")
	}
	// No BIC registered in the seed: assignor falls back to the
	// account ID, the ExportCancellationRequest convention.
	if back.Report.Assgnmt.Assgnr.Nm == "" || back.Report.Assgnmt.Assgte.Nm != p.Destination {
		t.Errorf("Assgnr/Assgte = %q/%q, want non-empty/%q",
			back.Report.Assgnmt.Assgnr.Nm, back.Report.Assgnmt.Assgte.Nm, p.Destination)
	}

	// The round trip: the exported document parses back into the same
	// resolution, and re-applying it is an idempotent no-op.
	report, err := ParseCAMT029(doc)
	if err != nil {
		t.Fatalf("ParseCAMT029(export): %v", err)
	}
	if report.Resolution != CancellationConfirmed {
		t.Errorf("resolution = %q, want confirmed", report.Resolution)
	}
	if report.OrgnlUETR != p.UETR || report.OrgnlMsgID != p.ID {
		t.Errorf("identity = %q/%q, want %q/%q", report.OrgnlUETR, report.OrgnlMsgID, p.UETR, p.ID)
	}
	if got := camt029ResolutionReason(report); got != "ACCR: funds released" {
		t.Errorf("reason = %q, want %q", got, "ACCR: funds released")
	}
	again, err := l.ApplyCAMT029Report(report, "night-ops")
	if err != nil {
		t.Fatalf("ApplyCAMT029Report(round-trip): %v", err)
	}
	if again.Status != CancellationConfirmed || again.ResolutionCode != c.ResolutionCode {
		t.Errorf("re-apply changed the record: %+v", again)
	}
}

func TestExportCAMT029Resolution_RejectedReasonRoundTrip(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-2")
	resolveCancellation(t, l, p.UETR, CancellationRejected, "RJCR: duplicate payment", "night-ops")

	doc, err := l.ExportCAMT029Resolution(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT029Resolution: %v", err)
	}
	back := parseCamt029ExportBack(t, doc)
	if back.Report.RslvdCxl.CxlSts.Cd != camt029RJCR {
		t.Errorf("CxlSts = %q, want RJCR", back.Report.RslvdCxl.CxlSts.Cd)
	}
	report, err := ParseCAMT029(doc)
	if err != nil {
		t.Fatalf("ParseCAMT029(export): %v", err)
	}
	if report.Resolution != CancellationRejected {
		t.Errorf("resolution = %q, want rejected", report.Resolution)
	}
	if got := camt029ResolutionReason(report); got != "RJCR: duplicate payment" {
		t.Errorf("reason = %q, want %q", got, "RJCR: duplicate payment")
	}
}

func TestExportCAMT029Resolution_NoReasonOmitsBlock(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-3")
	resolveCancellation(t, l, p.UETR, CancellationConfirmed, "", "night-ops")

	doc, err := l.ExportCAMT029Resolution(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT029Resolution: %v", err)
	}
	if body := string(doc); strings.Contains(body, "CxlStsRsnInf") {
		t.Errorf("document carries CxlStsRsnInf without a resolution code:\n%s", body)
	}
	report, err := ParseCAMT029(doc)
	if err != nil {
		t.Fatalf("ParseCAMT029(export): %v", err)
	}
	if report.Resolution != CancellationConfirmed {
		t.Errorf("resolution = %q, want confirmed", report.Resolution)
	}
}

func TestExportCAMT029Resolution_BareCodeRidesAddtlInf(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-4")
	resolveCancellation(t, l, p.UETR, CancellationRejected, "CUSTOMS-HOLD", "night-ops")

	doc, err := l.ExportCAMT029Resolution(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT029Resolution: %v", err)
	}
	back := parseCamt029ExportBack(t, doc)
	rsnInf := back.Report.RslvdCxl.CxlStsRsnInf
	if rsnInf == nil {
		t.Fatal("CxlStsRsnInf missing for a bare resolution code")
	}
	if rsnInf.Rsn != nil {
		t.Errorf("bare code grew a Rsn block: %+v", rsnInf.Rsn)
	}
	if rsnInf.AddtlInf != "CUSTOMS-HOLD" {
		t.Errorf("AddtlInf = %q, want %q", rsnInf.AddtlInf, "CUSTOMS-HOLD")
	}
	report, err := ParseCAMT029(doc)
	if err != nil {
		t.Fatalf("ParseCAMT029(export): %v", err)
	}
	if got := camt029ResolutionReason(report); got != "CUSTOMS-HOLD" {
		t.Errorf("reason = %q, want %q", got, "CUSTOMS-HOLD")
	}
}

func TestExportCAMT029Resolution_Unresolved(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-5")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("RequestPayoutCancellation: %v", err)
	}
	if _, err := l.ExportCAMT029Resolution(p.UETR); !errors.Is(err, ErrCancellationUnresolved) {
		t.Errorf("err = %v, want ErrCancellationUnresolved", err)
	}
}

func TestExportCAMT029Resolution_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.ExportCAMT029Resolution("no-such-uetr"); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("err = %v, want ErrNoOpenCancellation", err)
	}
}

func TestExportCAMT029Resolution_ReadOnly(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c029-6")
	resolveCancellation(t, l, p.UETR, CancellationConfirmed, "ACCR", "night-ops")

	versionBefore := l.version
	entriesBefore := len(l.entries)
	if _, err := l.ExportCAMT029Resolution(p.UETR); err != nil {
		t.Fatalf("ExportCAMT029Resolution: %v", err)
	}
	if _, err := l.ExportCAMT029Resolution(p.UETR); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if l.version != versionBefore {
		t.Errorf("version = %d, want %d (export must not bump)", l.version, versionBefore)
	}
	if len(l.entries) != entriesBefore {
		t.Errorf("entries = %d, want %d (export must not book)", len(l.entries), entriesBefore)
	}
}
