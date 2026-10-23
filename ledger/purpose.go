package ledger

import (
	"errors"
	"fmt"
)

// ISO 20022 purpose codes (LG-180): a curated subset of the ISO 20022
// ExternalPurpose1Code list, attached to transfers and payouts so the
// payment's business purpose rides the ledger end to end — validated
// at Post/PostTransfer/CreatePayout time, stamped on the journaled
// principal entry, disclosed in the reconciliation report, and mapped
// onto the wire as <Purp><Cd> in the pacs.008 (LG-176) and pacs.009
// (LG-172) outbound exports.
//
// The subset is curated, not exhaustive: the full external list runs
// to hundreds of codes, most of which a payment ledger never emits.
// Every code below is a genuine ExternalPurpose1Code entry with its
// ISO 20022 name; an instruction naming a code outside this set is
// rejected fail-closed with ErrInvalidPurposeCode — the machine only
// emits real codes, it never invents them. An empty purpose code
// means "no purpose stated": behavior without a code is unchanged
// (omitempty everywhere, no Purp element on the wire).

// PurposeCodeInfo describes one admitted ISO 20022 purpose code: the
// 4-letter ExternalPurpose1Code, its ISO 20022 code name, and a
// plain-language description for compliance tooling.
type PurposeCodeInfo struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// purposeCodeTable is the admitted ISO 20022 ExternalPurpose1Code
// subset, sorted by code. Each entry is a genuine ISO 20022
// ExternalPurpose1Code with its official code name.
var purposeCodeTable = []PurposeCodeInfo{
	{Code: "ACCT", Name: "AccountManagement", Description: "Account management instruction."},
	{Code: "CASH", Name: "CashManagementTransfer", Description: "Cash management transfer."},
	{Code: "CHAR", Name: "CharityPayment", Description: "Payment for charity reasons."},
	{Code: "COLL", Name: "CollectionPayment", Description: "Collection of funds."},
	{Code: "COMM", Name: "Commission", Description: "Payment of commission."},
	{Code: "DIVI", Name: "Dividend", Description: "Payment of dividends."},
	{Code: "GDDS", Name: "PurchaseSaleOfGoods", Description: "Purchase and sale of goods."},
	{Code: "GDSV", Name: "PurchaseSaleOfServices", Description: "Purchase and sale of services."},
	{Code: "GOVT", Name: "GovernmentPayment", Description: "Payment to or from a government department."},
	{Code: "HLRP", Name: "HomeLoanRepayment", Description: "Repayment of a home loan."},
	{Code: "INSU", Name: "InsurancePremium", Description: "Payment of an insurance premium."},
	{Code: "INTE", Name: "Interest", Description: "Payment of interest."},
	{Code: "IVPT", Name: "InvoicePayment", Description: "Payment of an invoice."},
	{Code: "LOAN", Name: "Loan", Description: "Payment of a loan."},
	{Code: "NETT", Name: "Netting", Description: "Netting payment."},
	{Code: "OTHR", Name: "Other", Description: "Other payment purpose."},
	{Code: "PENS", Name: "PensionPayment", Description: "Payment of pension."},
	{Code: "RENT", Name: "Rent", Description: "Payment of rent."},
	{Code: "SALA", Name: "SalaryPayment", Description: "Payment of salaries."},
	{Code: "SUPP", Name: "SupplierPayment", Description: "Payment to a supplier."},
	{Code: "TAXS", Name: "TaxPayment", Description: "Payment of taxes."},
	{Code: "TRAD", Name: "TradeServices", Description: "Payment related to trade services."},
	{Code: "VATX", Name: "ValueAddedTaxPayment", Description: "Payment of value added tax."},
}

// purposeCodeIndex maps each admitted code to its table row.
var purposeCodeIndex = func() map[string]PurposeCodeInfo {
	m := make(map[string]PurposeCodeInfo, len(purposeCodeTable))
	for _, p := range purposeCodeTable {
		m[p.Code] = p
	}
	return m
}()

// ErrInvalidPurposeCode is returned by Post, PostTransfer, and
// CreatePayout for a purpose code outside the admitted ISO 20022
// subset: the machine only emits real ExternalPurpose1Code entries,
// never invented ones. It is a 422-class error.
var ErrInvalidPurposeCode = errors.New("ledger: invalid ISO 20022 purpose code")

// IsValidPurposeCode reports whether code is in the admitted ISO 20022
// purpose-code subset. The match is exact and case-sensitive: ISO
// 20022 codes are 4 uppercase letters, and a near-miss is not a code.
func IsValidPurposeCode(code string) bool {
	_, ok := purposeCodeIndex[code]
	return ok
}

// LookupPurposeCode returns the table row for an admitted code, or
// false for a code outside the subset.
func LookupPurposeCode(code string) (PurposeCodeInfo, bool) {
	p, ok := purposeCodeIndex[code]
	return p, ok
}

// AdmittedPurposeCodes returns the admitted purpose-code table, sorted
// by code: the full vocabulary the ledger validates against.
func AdmittedPurposeCodes() []PurposeCodeInfo {
	out := make([]PurposeCodeInfo, len(purposeCodeTable))
	copy(out, purposeCodeTable)
	return out
}

// checkPurposeCode enforces the purpose-code contract: an empty code
// means "no purpose stated" and is always admitted; anything else must
// be an exact, case-sensitive match in the admitted subset, or the
// instruction is rejected fail-closed with ErrInvalidPurposeCode.
// Callers: validateJournalEntry (Post and PostBatch entries),
// validateTransferFields (PostTransfer), CreatePayout.
func checkPurposeCode(code string) error {
	if code == "" {
		return nil
	}
	if !IsValidPurposeCode(code) {
		return fmt.Errorf("%w: %q", ErrInvalidPurposeCode, code)
	}
	return nil
}
