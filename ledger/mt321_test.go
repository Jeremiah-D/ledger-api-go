package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt321TestLedger builds a ledger with a funded depositor ("saver",
// registered BIC DEUTDEFF, IBAN DE89370400440532013000) and one
// 90-day, 500 bps term deposit opened 2026-11-01, so
// :98A::TRAD//:98A::VALU// = 20261101 and :98A::MATU// =
// 20270130. The deposit ID ends in 4 digits (the :20C::LDCR//
// derivation needs them) and stays within the 16x :20C: limit.
func mt321TestLedger(t *testing.T) (*Ledger, TermDeposit) {
	t.Helper()
	l := seedTermDepositLedger(t)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	opened := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-mt321-1001",
		Account:     "saver",
		AmountCents: 400_000,
		Currency:    "USD",
		TermDays:    90,
		RateBps:     500,
		OpenedAt:    opened,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return l, td
}

// mt321TestOptions is the standard export call for the fixture:
// custodian CITIUS33 (the instruction's receiver), counterparty
// NWBKGB2L (the deposit-receiving institution).
func mt321TestOptions(td TermDeposit) MT321ExportOptions {
	return MT321ExportOptions{
		DepositID:       td.ID,
		CustodianBIC:    "CITIUS33",
		CounterpartyBIC: "NWBKGB2L",
	}
}

func TestExportMT321ExactTags(t *testing.T) {
	// The four 4!c indicator codes are pinned to the SWIFT UHB
	// MT321 field rules (verified 2026-11-12 against the UHB as
	// mirrored by iotafinance's MT321 structure table): TLDE
	// CONF = instruction to settle a first confirmation, DPLO
	// LEND = the sender is the lender, MICO A009 = Actual/365
	// (fixed), PRIT APFM = settlement instructions for amounts
	// payable by the investor.
	if mt321TLDEFirstConfirmation != "CONF" {
		t.Errorf("mt321TLDEFirstConfirmation = %q, want CONF", mt321TLDEFirstConfirmation)
	}
	if mt321DPLOLender != "LEND" {
		t.Errorf("mt321DPLOLender = %q, want LEND", mt321DPLOLender)
	}
	if mt321MICOActual365Fixed != "A009" {
		t.Errorf("mt321MICOActual365Fixed = %q, want A009", mt321MICOActual365Fixed)
	}
	if mt321PRITAmountsPayableByInvestor != "APFM" {
		t.Errorf("mt321PRITAmountsPayableByInvestor = %q, want APFM", mt321PRITAmountsPayableByInvestor)
	}

	l, td := mt321TestLedger(t)
	doc, err := l.ExportMT321(mt321TestOptions(td))
	if err != nil {
		t.Fatalf("ExportMT321: %v", err)
	}
	// Block framing: sender LT (depositor BIC, normalized),
	// receiver LT (custodian BIC), MUR = :20C::SEME//.
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I321CITIUS33XXXU3003}",
		"{3:{108:td-mt321-1001}}",
		"{4:",
		":16R:GENL",
		":20C::SEME//td-mt321-1001",
		// :20C::LDCR// re-derives the MT320 :22C: = DEUT + NW
		// + 1001 (the MT300 common reference) + NWBK + DE —
		// 16 characters, the key the custodian matches this
		// instruction against the MT320 confirmation with.
		":20C::LDCR//DEUTNW1001NWBKDE",
		":23G:NEWM",
		":22H::TLDE//CONF",
		":16S:GENL",
		":16R:TRADDET",
		":20C::CONU//td-mt321-1001",
		":22H::DPLO//LEND",
		":22H::MICO//A009",
		// Dates are 8!n YYYYMMDD: opened 2026-11-01, matures
		// 2027-01-30 (90 days on).
		":98A::TRAD//20261101",
		":98A::VALU//20261101",
		":98A::MATU//20270130",
		// :19A::SETT// is the principal alone — interest
		// settles separately at maturity.
		":19A::PRIN//USD4000,00",
		":19A::SETT//USD4000,00",
		":92A::INTR//0,05",
		":95P::LDCO//NWBKGB2L",
		":95P::INVE//DEUTDEFF",
		":97A::SAFE//DE89370400440532013000",
		":16S:TRADDET",
		":16R:SETT",
		":22H::PRIT//APFM",
		":95P::ACCW//NWBKGB2L",
		":95P::CDEA//DEUTDEFF",
		":16S:SETT",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// The omitted fields must stay omitted: no FIN :15A:
	// structure (this is ISO 15022 syntax), no linked-instruction
	// numbering, no pool/previous/related references, no
	// current/next interest or rollover amounts, no accrued days,
	// no executing broker, no separate cash account, no contact
	// narrative.
	for _, banned := range []string{
		":15A:", ":99B:", ":13A:", ":20C::POOL", ":19A::CINT:",
		":19A::NINT:", ":19A::RODI:", ":99B::DAAC:", ":95a::EXBO:",
		":97A::CASH:", ":70C:",
	} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
}

func TestExportMT321Amounts(t *testing.T) {
	l, td := mt321TestLedger(t)
	doc, err := l.ExportMT321(mt321TestOptions(td))
	if err != nil {
		t.Fatalf("ExportMT321: %v", err)
	}
	// :19A::PRIN// is pinned to the stored principal, and
	// :19A::SETT// equals it — the amount to settle now is the
	// principal alone.
	wantPRIN := ":19A::PRIN//" + td.Currency + mt940Amount(td.AmountCents)
	if !strings.Contains(doc, wantPRIN) {
		t.Errorf("export missing %q, body:\n%s", wantPRIN, doc)
	}
	wantSETT := ":19A::SETT//" + td.Currency + mt940Amount(td.AmountCents)
	if !strings.Contains(doc, wantSETT) {
		t.Errorf("export missing %q, body:\n%s", wantSETT, doc)
	}
	// :92A::INTR// is pinned to the flat rate: 500 bps = 0,05.
	rate, err := mt321RateBps(td.RateBps)
	if err != nil {
		t.Fatalf("mt321RateBps: %v", err)
	}
	if !strings.Contains(doc, ":92A::INTR//"+rate) {
		t.Errorf("export missing :92A::INTR//%s for %d bps, body:\n%s", rate, td.RateBps, doc)
	}
	if rate != "0,05" {
		t.Errorf("mt321RateBps(500) = %q, want 0,05", rate)
	}
}

func TestExportMT321LDCRMatchesMT320(t *testing.T) {
	l, td := mt321TestLedger(t)
	opts := mt321TestOptions(td)
	doc321, err := l.ExportMT321(opts)
	if err != nil {
		t.Fatalf("ExportMT321: %v", err)
	}
	doc320, err := l.ExportMT320(MT320ExportOptions{DepositID: td.ID, CounterpartyBIC: opts.CounterpartyBIC})
	if err != nil {
		t.Fatalf("ExportMT320: %v", err)
	}
	// Extract the :22C: line from the MT320 confirmation and the
	// :20C::LDCR// line from the MT321 instruction: the two must
	// agree exactly, so the custodian can match the instruction
	// against the confirmation.
	ldcr := ""
	for _, line := range strings.Split(doc321, "\n") {
		if strings.HasPrefix(line, ":20C::LDCR//") {
			ldcr = strings.TrimPrefix(line, ":20C::LDCR//")
		}
	}
	common := ""
	for _, line := range strings.Split(doc320, "\n") {
		if strings.HasPrefix(line, ":22C:") {
			common = strings.TrimPrefix(line, ":22C:")
		}
	}
	if ldcr == "" {
		t.Fatalf("MT321 export has no :20C::LDCR// line, body:\n%s", doc321)
	}
	if common == "" {
		t.Fatalf("MT320 export has no :22C: line, body:\n%s", doc320)
	}
	if ldcr != common {
		t.Errorf(":20C::LDCR// = %q, want the MT320 :22C: %q", ldcr, common)
	}
}

func TestExportMT321MaturedExports(t *testing.T) {
	l, td := mt321TestLedger(t)
	// Mature the deposit (opened 2026-11-01 for 90 days): a
	// matured deposit still instructs — only an early withdrawal
	// closes the settlement window.
	if _, err := l.MatureTermDeposit(td.ID, time.Date(2027, 1, 30, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.ExportMT321(mt321TestOptions(td)); err != nil {
		t.Errorf("ExportMT321 matured: %v", err)
	}
}

func TestExportMT321WithdrawnEarly(t *testing.T) {
	l, td := mt321TestLedger(t)
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now().UTC()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	_, err := l.ExportMT321(mt321TestOptions(td))
	if !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestExportMT321UnknownDeposit(t *testing.T) {
	l, td := mt321TestLedger(t)
	opts := mt321TestOptions(td)
	opts.DepositID = "td-nope"
	_, err := l.ExportMT321(opts)
	if !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("err = %v, want ErrTermDepositNotFound", err)
	}
}

func TestExportMT321BadBICs(t *testing.T) {
	l, td := mt321TestLedger(t)
	for _, bic := range []string{"", "CITI", "CITIUS33XXXX", "CITIUS3!"} {
		opts := mt321TestOptions(td)
		opts.CustodianBIC = bic
		_, err := l.ExportMT321(opts)
		if !errors.Is(err, ErrInvalidMT321Export) {
			t.Errorf("custodian BIC %q: err = %v, want ErrInvalidMT321Export", bic, err)
		}
	}
	for _, bic := range []string{"", "NWBK", "NWBKGB2LXXXX", "NWBKGB2!"} {
		opts := mt321TestOptions(td)
		opts.CounterpartyBIC = bic
		_, err := l.ExportMT321(opts)
		if !errors.Is(err, ErrInvalidMT321Export) {
			t.Errorf("counterparty BIC %q: err = %v, want ErrInvalidMT321Export", bic, err)
		}
	}
}

func TestExportMT321TieredDepositRejected(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt321-tier", Account: "saver", AmountCents: 400_000,
		Currency: "USD", TermDays: 30,
		RateTiers: []InterestTier{{MinCents: 0, RateBps: 300}, {MinCents: 200_000, RateBps: 500}},
	}); err != nil {
		t.Fatalf("OpenTermDeposit tiered: %v", err)
	}
	_, err := l.ExportMT321(MT321ExportOptions{DepositID: "td-mt321-tier", CustodianBIC: "CITIUS33", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT321Export) {
		t.Errorf("err = %v, want ErrInvalidMT321Export (no single :92A::INTR:)", err)
	}
}

func TestExportMT321BadDepositID(t *testing.T) {
	l := seedTermDepositLedger(t)
	// A 17+ character ID cannot fit :20C: (16x).
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt321-1001-long", Account: "saver", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("OpenTermDeposit long ID: %v", err)
	}
	_, err := l.ExportMT321(MT321ExportOptions{DepositID: "td-mt321-1001-long", CustodianBIC: "CITIUS33", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT321Export) {
		t.Errorf("long ID: err = %v, want ErrInvalidMT321Export", err)
	}
	// Braces are outside the SWIFT `x` set for :20C:.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td{mt321}1001", Account: "saver", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("OpenTermDeposit brace ID: %v", err)
	}
	_, err = l.ExportMT321(MT321ExportOptions{DepositID: "td{mt321}1001", CustodianBIC: "CITIUS33", CounterpartyBIC: "NWBKGB2L"})
	if !errors.Is(err, ErrInvalidMT321Export) {
		t.Errorf("brace ID: err = %v, want ErrInvalidMT321Export", err)
	}
}

func TestExportMT321NoBICFallsBackToQOption(t *testing.T) {
	l := seedTermDepositLedger(t)
	// "SAVER001" is an LT-form account ID (8 alphanumeric
	// characters): no registered BIC and no registered IBAN, so
	// the investor and the cash delivery agent render the Q
	// option name-and-address line, the safekeeping account
	// falls back to the account ID, and block 1 takes the
	// normalized LT.
	seedBalance(t, l, "SAVER001", 1_000_000)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-mt321-2002", Account: "SAVER001", AmountCents: 100_000,
		Currency: "USD", TermDays: 30, RateBps: 500,
		OpenedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	doc, err := l.ExportMT321(MT321ExportOptions{DepositID: "td-mt321-2002", CustodianBIC: "CITIUS33", CounterpartyBIC: "NWBKGB2L"})
	if err != nil {
		t.Fatalf("ExportMT321: %v", err)
	}
	for _, want := range []string{
		"{1:F01SAVER001XXX0000000000}",
		// :20C::LDCR// = SAVE + NW + 2002 + NWBK + SA.
		":20C::LDCR//SAVENW2002NWBKSA",
		":95Q::INVE//SAVER001",
		":97A::SAFE//SAVER001",
		":95Q::CDEA//SAVER001",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	for _, banned := range []string{":95P::INVE//", ":95P::CDEA//"} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s without a registered BIC, body:\n%s", banned, doc)
		}
	}
}

func TestExportMT321RepeatIdentical(t *testing.T) {
	l, td := mt321TestLedger(t)
	opts := mt321TestOptions(td)
	first, err := l.ExportMT321(opts)
	if err != nil {
		t.Fatalf("first ExportMT321: %v", err)
	}
	second, err := l.ExportMT321(opts)
	if err != nil {
		t.Fatalf("second ExportMT321: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
