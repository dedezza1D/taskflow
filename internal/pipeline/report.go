package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dedezza1D/taskflow/internal/pii"
	"github.com/dedezza1D/taskflow/internal/store"
)

// FindingsArtifact is the PII stage's checkpoint payload. Findings carry
// category and location, NEVER the matched value — the pipeline's own artifacts
// must not become a secondary PII store.
type FindingsArtifact struct {
	DocumentID      string         `json:"document_id"`
	DetectorVersion string         `json:"detector_version"`
	Counts          map[string]int `json:"counts"`
	Findings        []pii.Finding  `json:"findings"`
}

// runPII detects PII in the OCR text (checkpoint-first).
func (p *Pipeline) runPII(ctx context.Context, doc *store.Document, text string) ([]pii.Finding, error) {
	if data, ok, err := p.loadCheckpoint(ctx, doc.ID, StagePII, KindFindings); err != nil {
		return nil, err
	} else if ok {
		var fa FindingsArtifact
		if err := json.Unmarshal(data, &fa); err == nil {
			return fa.Findings, nil
		}
		// Unreadable checkpoint: recompute (idempotent) rather than fail.
	}

	findings := pii.Detect(text)
	fa := FindingsArtifact{
		DocumentID:      doc.ID.String(),
		DetectorVersion: pii.DetectorVersion,
		Counts:          pii.Counts(findings),
		Findings:        findings,
	}
	data, err := json.MarshalIndent(fa, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal findings: %w", err)
	}
	if err := p.saveCheckpoint(ctx, doc.ID, StagePII, KindFindings, objectKey(doc.ID, "findings.json"), data); err != nil {
		return nil, err
	}
	return findings, nil
}

// ---- report ---------------------------------------------------------------

// Regulations the report assesses. Detection is jurisdiction-independent (a
// CPF in a document is a CPF wherever the controller answers for it); what
// varies per regulation is classification and obligations, so those live in
// per-regulation sections over the same findings.
const (
	RegGDPR = "gdpr"
	RegLGPD = "lgpd"
)

// CategoryAssessment classifies one detected category under one regulation.
type CategoryAssessment struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	// Class is the regulation-specific classification: "personal_data"
	// (GDPR Art. 4(1) / LGPD Art. 5º I), "special_category" (GDPR Art. 9 /
	// LGPD Art. 5º II), or "context_dependent" for identifiers that are
	// personal data only in some contexts (e.g. a sole proprietor's CNPJ).
	Class   string `json:"class"`
	Special bool   `json:"special"`
	Notes   string `json:"notes"`
}

// RegulationSection is one regulation's view of the findings.
type RegulationSection struct {
	Regulation  string               `json:"regulation"`
	Categories  []CategoryAssessment `json:"categories,omitempty"`
	Obligations []string             `json:"obligations"`
}

// Report is the pipeline's final artifact.
type Report struct {
	DocumentID      string              `json:"document_id"`
	GeneratedAt     time.Time           `json:"generated_at"`
	DetectorVersion string              `json:"detector_version"`
	TotalFindings   int                 `json:"total_findings"`
	Regulations     []RegulationSection `json:"regulations"`
}

// Per-regulation category framing. No current detector yields special-category
// (GDPR Art. 9) / sensitive (LGPD Art. 5º II) data; the Special flag is
// structural so the Presidio NER upgrade (health terms, etc.) slots into the
// same report shape without changing consumers.
//
// Notes and obligations are written in Portuguese, like the recovery email: they
// are read by the people deciding what to do with a document, in the product's
// language, both on screen and in the exported JSON. Class stays a stable
// machine value; the interface labels it.
var regulationMeta = map[string]map[string]CategoryAssessment{
	RegGDPR: {
		pii.CategoryEmail:      {Class: "personal_data", Notes: "identificador de contato (Art. 4(1))"},
		pii.CategoryPhone:      {Class: "personal_data", Notes: "identificador de contato (Art. 4(1)); detecção de melhor esforço, sem dígito verificador"},
		pii.CategoryCreditCard: {Class: "personal_data", Notes: "dado financeiro (validado pelo algoritmo de Luhn); exige segurança reforçada (Art. 32)"},
		pii.CategoryIBAN:       {Class: "personal_data", Notes: "dado financeiro (validado por mod-97); exige segurança reforçada (Art. 32)"},
		pii.CategoryTaxIDDE:    {Class: "personal_data", Notes: "identificador nacional (Steuer-ID alemão, dígito verificador validado)"},
		pii.CategoryCPF:        {Class: "personal_data", Notes: "identificador nacional (CPF brasileiro, dígito verificador validado)"},
		pii.CategoryCNPJ:       {Class: "context_dependent", Notes: "identificador de pessoa jurídica (CNPJ); só é dado pessoal quando identifica uma pessoa natural (ex.: empresário individual)"},
	},
	RegLGPD: {
		pii.CategoryEmail:      {Class: "personal_data", Notes: "identificador de contato (Art. 5º, I)"},
		pii.CategoryPhone:      {Class: "personal_data", Notes: "identificador de contato (Art. 5º, I); detecção de melhor esforço, sem dígito verificador"},
		pii.CategoryCreditCard: {Class: "personal_data", Notes: "dado financeiro (validado pelo algoritmo de Luhn); exige segurança reforçada (Art. 46)"},
		pii.CategoryIBAN:       {Class: "personal_data", Notes: "dado financeiro (validado por mod-97); exige segurança reforçada (Art. 46)"},
		pii.CategoryTaxIDDE:    {Class: "personal_data", Notes: "identificador nacional estrangeiro (Steuer-ID alemão, dígito verificador validado)"},
		pii.CategoryCPF:        {Class: "personal_data", Notes: "identificador nacional (CPF, dígito verificador validado)"},
		pii.CategoryCNPJ:       {Class: "context_dependent", Notes: "identificador de pessoa jurídica (CNPJ); só é dado pessoal quando identifica uma pessoa natural (ex.: MEI ou empresário individual)"},
	},
}

var financialCategories = map[string]bool{
	pii.CategoryCreditCard: true,
	pii.CategoryIBAN:       true,
}

// Deterministic category order ≈ detector precedence order.
var reportCategoryOrder = []string{
	pii.CategoryIBAN, pii.CategoryCNPJ, pii.CategoryCreditCard,
	pii.CategoryCPF, pii.CategoryTaxIDDE, pii.CategoryPhone, pii.CategoryEmail,
}

// BuildReport turns findings into the compliance report: one section per
// regulation over the same counts. Pure function — tested directly.
func BuildReport(documentID string, findings []pii.Finding, now time.Time) Report {
	counts := pii.Counts(findings)

	rep := Report{
		DocumentID:      documentID,
		GeneratedAt:     now.UTC(),
		DetectorVersion: pii.DetectorVersion,
		TotalFindings:   len(findings),
	}

	hasFinancial := counts[pii.CategoryCreditCard] > 0 || counts[pii.CategoryIBAN] > 0

	for _, reg := range []string{RegGDPR, RegLGPD} {
		sec := RegulationSection{Regulation: reg}
		for _, cat := range reportCategoryOrder {
			n, ok := counts[cat]
			if !ok {
				continue
			}
			a := regulationMeta[reg][cat]
			a.Category = cat
			a.Count = n
			sec.Categories = append(sec.Categories, a)
		}
		sec.Obligations = obligationsFor(reg, rep.TotalFindings > 0, hasFinancial)
		rep.Regulations = append(rep.Regulations, sec)
	}

	return rep
}

func obligationsFor(regulation string, hasFindings, hasFinancial bool) []string {
	var out []string
	switch regulation {
	case RegGDPR:
		if !hasFindings {
			return []string{"Nenhum dado pessoal detectado pelo detector " + pii.DetectorVersion + "; mantenha o original apenas pelo tempo necessário (limitação da conservação, Art. 5(1)(e))."}
		}
		out = append(out,
			"O tratamento deve respeitar os princípios do Art. 5 e ter uma base legal do Art. 6.",
			"Aplicam-se os deveres de transparência (Art. 13/14): informe os titulares sobre este tratamento.",
			"Aplicam-se os direitos do titular, incluindo acesso (Art. 15) e apagamento (Art. 17) — o apagamento é feito pela ação Apagar (Art. 17) desta ferramenta.",
			"Registre esta atividade de tratamento (Art. 30).",
		)
		if hasFinancial {
			out = append(out,
				"Há identificadores financeiros: aplique com cuidado redobrado a segurança do tratamento (Art. 32).",
				"Esteja pronto para notificar violações de dados (Art. 33/34).",
			)
		}
	case RegLGPD:
		if !hasFindings {
			return []string{"Nenhum dado pessoal detectado pelo detector " + pii.DetectorVersion + "; mantenha o original apenas pelo tempo necessário à sua finalidade (Art. 15/16)."}
		}
		out = append(out,
			"O tratamento deve respeitar os princípios do Art. 6 e ter uma base legal do Art. 7.",
			"Aplicam-se os direitos do titular (Art. 18), incluindo confirmação, acesso e eliminação — a eliminação é feita pela ação Apagar desta ferramenta.",
			"Registre esta atividade de tratamento (Art. 37).",
		)
		if hasFinancial {
			out = append(out,
				"Há identificadores financeiros: aplique com cuidado redobrado as medidas de segurança do Art. 46.",
				"Esteja pronto para comunicar incidentes à ANPD e aos titulares (Art. 48).",
			)
		}
	}
	return out
}

// runReport writes the report artifact (checkpoint-first).
func (p *Pipeline) runReport(ctx context.Context, doc *store.Document, findings []pii.Finding) error {
	if _, ok, err := p.loadCheckpoint(ctx, doc.ID, StageReport, KindReport); err != nil {
		return err
	} else if ok {
		return nil
	}

	rep := BuildReport(doc.ID.String(), findings, time.Now())
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return p.saveCheckpoint(ctx, doc.ID, StageReport, KindReport, objectKey(doc.ID, "report.json"), data)
}
