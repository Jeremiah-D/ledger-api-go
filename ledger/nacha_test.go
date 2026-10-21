package ledger

import (
	"strings"
	"testing"
	"time"
)

func testNACHAFileConfig() NACHAFileConfig {
	return NACHAFileConfig{
		ImmediateDestination: "021000089",
		ImmediateOrigin:      "121042882",
		DestinationName:      "FEDERAL RESERVE",
		OriginName:           "FIRST NATIONAL",
		CreationTime:         time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC),
		FileIDModifier:       "A",
		ReferenceCode:        "PAYROLL",
	}
}

func testNACHABatch() NACHABatch {
	return NACHABatch{
		Config: NACHABatchConfig{
			ServiceClassCode:        "220",
			CompanyName:             "ACME CORP",
			CompanyID:               "1122334455",
			EntryClassCode:          "CCD",
			CompanyEntryDescription: "VENDOR PAY",
			EffectiveEntryDate:      time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
			OriginatingDFI:          "12104288",
			BatchNumber:             1,
		},
		Entries: []NACHAEntry{
			{
				TransactionCode: "22",
				Routing:         "021000089",
				AccountNumber:   "987654321",
				AmountCents:     150000, // $1,500.00
				IndividualID:    "INV-1001",
				IndividualName:  "JOHN DOE",
				Addenda:         []string{"Invoice 1001 paid in full"},
			},
			{
				TransactionCode: "22",
				Routing:         "123456780",
				AccountNumber:   "11223344",
				AmountCents:     25000, // $250.00
				IndividualID:    "INV-1002",
				IndividualName:  "JANE SMITH",
			},
		},
	}
}

func TestNACHACheckDigit(t *testing.T) {
	if d, err := NACHACheckDigit("021000089"); err != nil || d != '9' {
		t.Fatalf("check digit of 021000089: got %c, %v", d, err)
	}
	if d, err := NACHACheckDigit("123456780"); err != nil || d != '0' {
		t.Fatalf("check digit of 123456780: got %c, %v", d, err)
	}
	for _, bad := range []string{"021000080", "02100008", "0210000891", "02100008a", ""} {
		if _, err := NACHACheckDigit(bad); err == nil {
			t.Fatalf("expected error for routing %q", bad)
		}
	}
}

func TestNACHAEntryDirection(t *testing.T) {
	for code, want := range map[string]NACHAEntryDirection{"22": NACHAEntryCredit, "32": NACHAEntryCredit, "27": NACHAEntryDebit, "37": NACHAEntryDebit} {
		got, err := (NACHAEntry{TransactionCode: code}).Direction()
		if err != nil || got != want {
			t.Fatalf("direction of %q: got %q, %v", code, got, err)
		}
	}
	if _, err := (NACHAEntry{TransactionCode: "99"}).Direction(); err == nil {
		t.Fatal("expected error for transaction code 99")
	}
}

func TestNACHAFileLayout(t *testing.T) {
	doc, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{testNACHABatch()})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if len(l) != 94 {
			t.Fatalf("line %d has length %d, want 94: %q", i, len(l), l)
		}
	}
	if len(lines)%10 != 0 {
		t.Fatalf("file has %d lines, not a multiple of 10", len(lines))
	}
	// Record sequence: 1, 5, 6, 7, 6, 8, 9, then 9-pads.
	wantTypes := []byte{'1', '5', '6', '7', '6', '8', '9'}
	for i, w := range wantTypes {
		if lines[i][0] != w {
			t.Fatalf("line %d: record type %q, want %q", i, lines[i][0], w)
		}
	}
	// File header fields.
	fh := lines[0]
	if fh[1:3] != "01" {
		t.Fatalf("priority code: %q", fh[1:3])
	}
	if fh[3:13] != " 021000089" {
		t.Fatalf("immediate destination: %q", fh[3:13])
	}
	if fh[13:23] != " 121042882" {
		t.Fatalf("immediate origin: %q", fh[13:23])
	}
	if fh[23:29] != "261020" {
		t.Fatalf("file creation date: %q", fh[23:29])
	}
	if fh[29:33] != "1430" {
		t.Fatalf("file creation time: %q", fh[29:33])
	}
	if fh[33:34] != "A" || fh[34:37] != "094" || fh[37:39] != "10" || fh[39:40] != "1" {
		t.Fatalf("file header constants: %q", fh[33:40])
	}
	if fh[40:63] != "FEDERAL RESERVE        " {
		t.Fatalf("destination name: %q", fh[40:63])
	}
	if fh[63:86] != "FIRST NATIONAL         " {
		t.Fatalf("origin name: %q", fh[63:86])
	}
	if fh[86:94] != "PAYROLL " {
		t.Fatalf("reference code: %q", fh[86:94])
	}
	// Batch header fields.
	bh := lines[1]
	if bh[1:4] != "220" || bh[50:53] != "CCD" {
		t.Fatalf("batch header codes: %q %q", bh[1:4], bh[50:53])
	}
	if bh[4:20] != "ACME CORP       " {
		t.Fatalf("company name: %q", bh[4:20])
	}
	if bh[40:50] != "1122334455" {
		t.Fatalf("company id: %q", bh[40:50])
	}
	if bh[69:75] != "261021" {
		t.Fatalf("effective entry date: %q", bh[69:75])
	}
	if bh[75:78] != "   " || bh[78:79] != "1" {
		t.Fatalf("settlement/originator status: %q", bh[75:79])
	}
	if bh[79:87] != "12104288" || bh[87:94] != "0000001" {
		t.Fatalf("odfi/batch number: %q", bh[79:94])
	}
	// Entry 1: transaction code, RDFI, check digit, amount, addenda flag, trace.
	e1 := lines[2]
	if e1[1:3] != "22" {
		t.Fatalf("entry1 transaction code: %q", e1[1:3])
	}
	if e1[3:11] != "02100008" || e1[11:12] != "9" {
		t.Fatalf("entry1 rdfi+check: %q", e1[3:12])
	}
	if e1[29:39] != "0000150000" {
		t.Fatalf("entry1 amount: %q", e1[29:39])
	}
	if e1[39:54] != "INV-1001       " {
		t.Fatalf("entry1 individual id: %q", e1[39:54])
	}
	if e1[54:76] != "JOHN DOE              " {
		t.Fatalf("entry1 individual name: %q", e1[54:76])
	}
	if e1[78:79] != "1" {
		t.Fatalf("entry1 addenda indicator: %q", e1[78:79])
	}
	if e1[79:94] != "121042880000001" {
		t.Fatalf("entry1 trace: %q", e1[79:94])
	}
	// Addenda record for entry 1.
	ad := lines[3]
	if ad[1:3] != "05" {
		t.Fatalf("addenda type code: %q", ad[1:3])
	}
	if !strings.HasPrefix(ad[3:83], "Invoice 1001 paid in full") {
		t.Fatalf("addenda info: %q", ad[3:83])
	}
	if ad[83:87] != "0001" || ad[87:94] != "0000001" {
		t.Fatalf("addenda seq numbers: %q", ad[83:94])
	}
	// Entry 2: no addenda.
	e2 := lines[4]
	if e2[3:12] != "123456780" {
		t.Fatalf("entry2 rdfi+check: %q", e2[3:12])
	}
	if e2[29:39] != "0000025000" {
		t.Fatalf("entry2 amount: %q", e2[29:39])
	}
	if e2[78:79] != "0" || e2[79:94] != "121042880000002" {
		t.Fatalf("entry2 addenda/trace: %q", e2[78:94])
	}
	// Batch control: 3 entry/addenda records, hash, debit 0, credit 175000.
	bc := lines[5]
	if bc[1:4] != "220" {
		t.Fatalf("batch control service class: %q", bc[1:4])
	}
	if bc[4:10] != "000003" {
		t.Fatalf("batch entry/addenda count: %q", bc[4:10])
	}
	// Entry hash: 02100008 + 12345678 = 14445686.
	if bc[10:20] != "0014445686" {
		t.Fatalf("batch entry hash: %q", bc[10:20])
	}
	if bc[20:32] != "000000000000" {
		t.Fatalf("batch debit total: %q", bc[20:32])
	}
	if bc[32:44] != "000000175000" {
		t.Fatalf("batch credit total: %q", bc[32:44])
	}
	if bc[44:54] != "1122334455" {
		t.Fatalf("batch control company id: %q", bc[44:54])
	}
	if bc[79:87] != "12104288" || bc[87:94] != "0000001" {
		t.Fatalf("batch control odfi/batch: %q", bc[79:94])
	}
	// File control.
	fc := lines[6]
	if fc[1:7] != "000001" {
		t.Fatalf("file batch count: %q", fc[1:7])
	}
	if fc[7:13] != "000001" {
		t.Fatalf("file block count: %q", fc[7:13])
	}
	if fc[13:21] != "00000003" {
		t.Fatalf("file entry/addenda count: %q", fc[13:21])
	}
	if fc[21:31] != "0014445686" {
		t.Fatalf("file entry hash: %q", fc[21:31])
	}
	if fc[31:43] != "000000000000" || fc[43:55] != "000000175000" {
		t.Fatalf("file totals: %q", fc[31:55])
	}
	// Trailing pads.
	for i := 7; i < len(lines); i++ {
		if lines[i] != strings.Repeat("9", 94) {
			t.Fatalf("line %d is not a 9-pad: %q", i, lines[i])
		}
	}
}

func TestNACHAServiceClassDirection(t *testing.T) {
	debit := NACHAEntry{TransactionCode: "27", Routing: "021000089", AccountNumber: "1", AmountCents: 100, IndividualName: "A"}
	credit := NACHAEntry{TransactionCode: "22", Routing: "021000089", AccountNumber: "1", AmountCents: 100, IndividualName: "A"}
	mk := func(class string, e NACHAEntry) NACHABatch {
		b := testNACHABatch()
		b.Config.ServiceClassCode = class
		b.Entries = []NACHAEntry{e}
		return b
	}
	// 220 credits only: debit rejected.
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{mk("220", debit)}); err == nil {
		t.Fatal("expected debit-in-220 to fail")
	}
	// 225 debits only: credit rejected.
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{mk("225", credit)}); err == nil {
		t.Fatal("expected credit-in-225 to fail")
	}
	// 200 mixed: both accepted.
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{mk("200", debit), mk("200", credit)}); err != nil {
		t.Fatalf("mixed batch failed: %v", err)
	}
	// Bad service class / SEC code.
	bad := mk("221", credit)
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{bad}); err == nil {
		t.Fatal("expected bad service class to fail")
	}
	bad2 := mk("200", credit)
	bad2.Config.EntryClassCode = "XYZ"
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{bad2}); err == nil {
		t.Fatal("expected bad SEC code to fail")
	}
}

func TestNACHAValidationFailures(t *testing.T) {
	good := testNACHABatch()
	cases := []struct {
		name   string
		mutate func(*NACHABatch)
	}{
		{"bad check digit", func(b *NACHABatch) { b.Entries[0].Routing = "021000080" }},
		{"short routing", func(b *NACHABatch) { b.Entries[0].Routing = "02100008" }},
		{"bad tx code", func(b *NACHABatch) { b.Entries[0].TransactionCode = "99" }},
		{"negative amount", func(b *NACHABatch) { b.Entries[0].AmountCents = -1 }},
		{"huge amount", func(b *NACHABatch) { b.Entries[0].AmountCents = 10000000000 }},
		{"long name", func(b *NACHABatch) { b.Entries[0].IndividualName = strings.Repeat("X", 23) }},
		{"long account", func(b *NACHABatch) { b.Entries[0].AccountNumber = strings.Repeat("1", 18) }},
		{"non-ascii name", func(b *NACHABatch) { b.Entries[0].IndividualName = "JOSÉ" }},
		{"long addenda", func(b *NACHABatch) { b.Entries[0].Addenda = []string{strings.Repeat("x", 81)} }},
		{"nonzero prenote", func(b *NACHABatch) { b.Entries[0].TransactionCode = "23" }},
	}
	for _, c := range cases {
		b := testNACHABatch()
		c.mutate(&b)
		if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{b}); err == nil {
			t.Fatalf("case %q: expected error", c.name)
		}
		_ = good
	}
	// Empty batches / entries.
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), nil); err == nil {
		t.Fatal("expected error for no batches")
	}
	empty := testNACHABatch()
	empty.Entries = nil
	if _, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{empty}); err == nil {
		t.Fatal("expected error for empty batch")
	}
}

func TestNACHAPayoutConversion(t *testing.T) {
	p := PayoutInstruction{ID: "po-1", PayeeAccount: "ops", AmountCents: 9999, Currency: "USD"}
	e, err := PayoutToNACHAEntry(p, NACHAReceiver{Routing: "021000089", AccountNumber: "555", IndividualName: "ACME SUPPLIER"})
	if err != nil {
		t.Fatal(err)
	}
	if e.TransactionCode != "22" || e.AmountCents != 9999 || e.IndividualID != "po-1" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	sav, err := PayoutToNACHAEntry(p, NACHAReceiver{Routing: "021000089", AccountNumber: "555", IndividualName: "X", Savings: true})
	if err != nil || sav.TransactionCode != "32" {
		t.Fatalf("savings entry: %+v, %v", sav, err)
	}
	if _, err := PayoutToNACHAEntry(PayoutInstruction{ID: "po-2", AmountCents: 0}, NACHAReceiver{Routing: "021000089"}); err == nil {
		t.Fatal("expected error for zero payout amount")
	}
	if _, err := PayoutToNACHAEntry(p, NACHAReceiver{Routing: "021000080", AccountNumber: "1", IndividualName: "X"}); err == nil {
		t.Fatal("expected error for bad routing in payout conversion")
	}
}

func TestNACHAMultiBatchBlockMath(t *testing.T) {
	// Batch 1: 1 entry, no addenda. Batch 2: 3 entries. Records: 1 file
	// header + (1+1+1) + (1+3+1) + 1 file control = 10 -> exactly one
	// block, no padding.
	cfg := testNACHAFileConfig()
	b1 := testNACHABatch()
	b1.Entries = b1.Entries[:1]
	b1.Entries[0].Addenda = nil
	b2 := testNACHABatch()
	b2.Config.BatchNumber = 2
	b2.Entries = append([]NACHAEntry{}, b1.Entries[0], b1.Entries[0], b1.Entries[0])
	doc, err := GenerateNACHAFile(cfg, []NACHABatch{b1, b2})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(doc, "\n")
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(lines))
	}
	fc := lines[9]
	if fc[0] != '9' {
		t.Fatalf("file control not at line 9: %q", fc[:10])
	}
	if fc[1:7] != "000002" || fc[7:13] != "000001" || fc[13:21] != "00000004" {
		t.Fatalf("file control counts: %q", fc[1:21])
	}
	// Trace numbers restart per batch.
	if lines[2][79:94] != "121042880000001" || lines[5][79:94] != "121042880000001" || lines[7][79:94] != "121042880000003" {
		t.Fatalf("trace numbers: %q %q %q", lines[2][79:94], lines[5][79:94], lines[7][79:94])
	}
	// Entry hash crosses batches: batch1 02100008 + batch2 3x02100008.
	if fc[21:31] != "0008400032" {
		t.Fatalf("file entry hash: %q", fc[21:31])
	}
}

// TestNACHAFileFixture pins the exact bytes of the canonical fixture
// file: any layout drift changes these bytes. The fixture was verified
// field-by-field against the NACHA record layouts (see
// TestNACHAFileLayout) before freezing.
func TestNACHAFileFixture(t *testing.T) {
	doc, err := GenerateNACHAFile(testNACHAFileConfig(), []NACHABatch{testNACHABatch()})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"101 021000089 1210428822610201430A094101FEDERAL RESERVE        FIRST NATIONAL         PAYROLL ",
		"5220ACME CORP                           1122334455CCDVENDOR PAY      261021   1121042880000001",
		"622021000089987654321        0000150000INV-1001       JOHN DOE                1121042880000001",
		"705Invoice 1001 paid in full                                                       00010000001",
		"62212345678011223344         0000025000INV-1002       JANE SMITH              0121042880000002",
		"822000000300144456860000000000000000001750001122334455                         121042880000001",
		"9000001000001000000030014445686000000000000000000175000                                       ",
		strings.Repeat("9", 94),
		strings.Repeat("9", 94),
		strings.Repeat("9", 94),
	}, "\n")
	if doc != want {
		t.Fatalf("fixture mismatch:\n got %q\nwant %q", doc, want)
	}
}
