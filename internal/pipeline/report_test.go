package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/pii"
)

func section(t *testing.T, rep Report, regulation string) RegulationSection {
	t.Helper()
	for _, sec := range rep.Regulations {
		if sec.Regulation == regulation {
			return sec
		}
	}
	t.Fatalf("report missing %s section: %+v", regulation, rep.Regulations)
	return RegulationSection{}
}

func TestBuildReportClassifiesAndObligates(t *testing.T) {
	findings := []pii.Finding{
		{Category: pii.CategoryEmail, Line: 1, Start: 0, End: 10, Length: 10},
		{Category: pii.CategoryCreditCard, Line: 2, Start: 0, End: 19, Length: 19},
		{Category: pii.CategoryCreditCard, Line: 3, Start: 0, End: 19, Length: 19},
	}

	rep := BuildReport("doc-1", findings, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	if rep.TotalFindings != 3 {
		t.Fatalf("total findings = %d, want 3", rep.TotalFindings)
	}
	if rep.DetectorVersion != pii.DetectorVersion {
		t.Fatalf("report must state its detector version")
	}

	gdpr := section(t, rep, RegGDPR)

	var card, email *CategoryAssessment
	for i := range gdpr.Categories {
		switch gdpr.Categories[i].Category {
		case pii.CategoryCreditCard:
			card = &gdpr.Categories[i]
		case pii.CategoryEmail:
			email = &gdpr.Categories[i]
		}
	}
	if card == nil || card.Count != 2 || card.Class != "personal_data" || card.Special {
		t.Fatalf("credit_card assessment wrong: %+v", card)
	}
	if email == nil || email.Count != 1 {
		t.Fatalf("email assessment wrong: %+v", email)
	}

	joined := strings.Join(gdpr.Obligations, " | ")
	for _, want := range []string{"Art. 6", "Art. 17", "Art. 32", "Art. 33"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("gdpr obligations should mention %s (financial data present): %s", want, joined)
		}
	}
}

func TestBuildReportLGPDSection(t *testing.T) {
	findings := []pii.Finding{
		{Category: pii.CategoryCPF, Line: 1, Start: 0, End: 14, Length: 14},
		{Category: pii.CategoryCNPJ, Line: 2, Start: 0, End: 18, Length: 18},
		{Category: pii.CategoryCreditCard, Line: 3, Start: 0, End: 19, Length: 19},
	}

	rep := BuildReport("doc-br", findings, time.Now())
	lgpd := section(t, rep, RegLGPD)

	var cpf, cnpj *CategoryAssessment
	for i := range lgpd.Categories {
		switch lgpd.Categories[i].Category {
		case pii.CategoryCPF:
			cpf = &lgpd.Categories[i]
		case pii.CategoryCNPJ:
			cnpj = &lgpd.Categories[i]
		}
	}
	if cpf == nil || cpf.Class != "personal_data" {
		t.Fatalf("cpf assessment wrong: %+v", cpf)
	}
	if cnpj == nil || cnpj.Class != "context_dependent" {
		t.Fatalf("cnpj must be context_dependent (legal-entity identifier): %+v", cnpj)
	}

	joined := strings.Join(lgpd.Obligations, " | ")
	for _, want := range []string{"Art. 7", "Art. 18", "Art. 37", "Art. 46", "Art. 48"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lgpd obligations should mention %s (financial data present): %s", want, joined)
		}
	}

	// Both regulations assess the SAME findings.
	gdpr := section(t, rep, RegGDPR)
	if len(gdpr.Categories) != len(lgpd.Categories) {
		t.Fatalf("sections diverge on categories: gdpr=%d lgpd=%d", len(gdpr.Categories), len(lgpd.Categories))
	}
}

func TestBuildReportNoFindings(t *testing.T) {
	rep := BuildReport("doc-2", nil, time.Now())
	if rep.TotalFindings != 0 {
		t.Fatalf("empty findings should produce zero total: %+v", rep)
	}
	for _, reg := range []string{RegGDPR, RegLGPD} {
		sec := section(t, rep, reg)
		if len(sec.Categories) != 0 {
			t.Fatalf("%s: empty findings should produce empty categories: %+v", reg, sec)
		}
		if len(sec.Obligations) == 0 || !strings.Contains(sec.Obligations[0], "mantenha o original apenas pelo tempo necessário") {
			t.Fatalf("%s: no-findings report still carries the retention note: %+v", reg, sec.Obligations)
		}
	}
}

// The report is read by the people deciding what to do with a document, not by
// API clients. It used to tell them their erasure right was "served by
// DELETE /api/v1/documents/{id}".
func TestReportSpeaksToPeopleNotToTheAPI(t *testing.T) {
	findings := []pii.Finding{{Category: pii.CategoryCreditCard}, {Category: pii.CategoryEmail}}
	rep := BuildReport("doc-4", findings, time.Now())
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, jargon := range []string{"/api/", "DELETE ", "served by"} {
		if strings.Contains(string(data), jargon) {
			t.Fatalf("report text leaks API jargon %q: %s", jargon, data)
		}
	}
}

func TestReportJSONCarriesNoValues(t *testing.T) {
	// The report aggregates counts; findings' locations stay in findings.json.
	// Guard: the serialized report must not even have a place for raw values.
	rep := BuildReport("doc-3", []pii.Finding{{Category: pii.CategoryIBAN, Line: 1, Start: 0, End: 22, Length: 22}}, time.Now())
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"value", "match", "raw"} {
		if strings.Contains(strings.ToLower(string(data)), `"`+forbidden+`"`) {
			t.Fatalf("report JSON exposes a %q field: %s", forbidden, data)
		}
	}
}
