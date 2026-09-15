import type { ApiDocument, Report, CategoryAssessment } from '../api'
import { stageLabel } from './pipeline'

// Categories whose presence the report itself singles out for heightened
// security duties (GDPR Art. 32 / LGPD Art. 46). Kept in sync with the
// obligations the backend emits for financial identifiers.
const FINANCIAL = new Set(['credit_card', 'iban'])

export type Verdict = 'clear' | 'personal' | 'sensitive' | 'unknown' | 'pending'

export interface Triage {
  verdict: Verdict
  title: string
  detail: string
}

function categoriesOf(report: Report): CategoryAssessment[] {
  // Every regulation section scores the same findings, so one section is
  // enough to decide the verdict.
  return report.regulations?.[0]?.categories ?? []
}

/** The "can this document leave the building?" answer, derived from the report. */
export function triage(
  doc: ApiDocument,
  report: Report | null,
  // The document finished, but its report cannot be fetched. Without this the
  // absence of a report reads as "still working", and a permanently broken
  // document sits in the list claiming to be in progress forever.
  unreadable = false,
): Triage {
  if (doc.status === 'failed') {
    return {
      verdict: 'unknown',
      title: 'Não analisado',
      detail: `A análise parou na etapa de ${stageLabel(doc.failed_stage)}. Sem relatório, a exposição é desconhecida — trate o documento como não verificado.`,
    }
  }
  if (!report && unreadable) {
    return {
      verdict: 'unknown',
      title: 'Sem relatório',
      detail:
        'O documento consta como analisado, mas o relatório não pôde ser lido. A exposição é desconhecida — trate como não verificado.',
    }
  }
  if (!report) {
    return {
      verdict: 'pending',
      title: 'Analisando',
      detail:
        'O documento ainda está passando pelos estágios de OCR, detecção e relatório.',
    }
  }

  const categories = categoriesOf(report)

  if (report.total_findings === 0) {
    return {
      verdict: 'clear',
      title: 'Nenhum dado pessoal encontrado',
      detail:
        'Os detectores não acharam identificadores. Compartilhamento sem restrição de dados pessoais.',
    }
  }

  const special = categories.filter((c) => c.special)
  const financial = categories.filter((c) => FINANCIAL.has(c.category))

  if (special.length > 0 || financial.length > 0) {
    const why =
      special.length > 0
        ? 'Contém dados sensíveis'
        : 'Contém identificadores financeiros'
    return {
      verdict: 'sensitive',
      title: 'Restrito',
      detail: `${why}. Exige base legal e as medidas de segurança reforçadas (GDPR Art. 32 / LGPD Art. 46) antes de qualquer compartilhamento.`,
    }
  }

  return {
    verdict: 'personal',
    title: 'Contém dados pessoais',
    detail:
      'Compartilhar exige base legal e cumprimento dos deveres de transparência. Veja as obrigações abaixo.',
  }
}

export interface CategoryExposure {
  category: string
  /** How many documents contain at least one finding of this category. */
  documents: number
  /** Total occurrences across the corpus. */
  occurrences: number
}

export interface Inventory {
  analysed: number
  withPersonalData: number
  restricted: number
  /** Failed in the pipeline, or analysed but with an unreadable report. Either
   * way the exposure is unknown, which is the only thing the reader needs. */
  unanalysed: number
  /** Genuinely still in flight. Never includes the two counts above -- they
   * used to overlap, and the tiles added up to more than the corpus. */
  pending: number
  exposure: CategoryExposure[]
}

/**
 * Corpus-level view: what personal data the collection holds, which is the
 * Art. 30 / Art. 37 record-keeping question the dashboard leads with.
 */
export function buildInventory(
  documents: ApiDocument[],
  reports: Map<string, Report>,
  // Documents whose report the API will not hand over. They are "completed"
  // but their exposure is unknowable, so they belong with the failures rather
  // than sitting in "processing" forever.
  unreadable: ReadonlySet<string> = new Set(),
): Inventory {
  const byCategory = new Map<string, CategoryExposure>()
  let analysed = 0
  let withPersonalData = 0
  let restricted = 0
  let unanalysed = 0

  for (const doc of documents) {
    if (doc.status === 'failed') {
      unanalysed++
      continue
    }
    if (unreadable.has(doc.id)) {
      unanalysed++
      continue
    }
    const report = reports.get(doc.id)
    if (!report) continue

    analysed++
    const t = triage(doc, report)
    if (t.verdict === 'personal' || t.verdict === 'sensitive')
      withPersonalData++
    if (t.verdict === 'sensitive') restricted++

    for (const c of categoriesOf(report)) {
      const entry = byCategory.get(c.category) ?? {
        category: c.category,
        documents: 0,
        occurrences: 0,
      }
      entry.documents++
      entry.occurrences += c.count
      byCategory.set(c.category, entry)
    }
  }

  const exposure = [...byCategory.values()].sort(
    (a, b) => b.documents - a.documents || a.category.localeCompare(b.category),
  )

  return {
    analysed,
    withPersonalData,
    restricted,
    unanalysed,
    pending: documents.length - analysed - unanalysed,
    exposure,
  }
}

const VERDICT_META: Record<
  Verdict,
  { tone: string; glyph: string; short: string }
> = {
  clear: { tone: 'good', glyph: '✓', short: 'Livre' },
  personal: { tone: 'warning', glyph: '!', short: 'Dados pessoais' },
  sensitive: { tone: 'critical', glyph: '▲', short: 'Restrito' },
  unknown: { tone: 'serious', glyph: '?', short: 'Não analisado' },
  pending: { tone: 'neutral', glyph: '◐', short: 'Analisando' },
}

export function verdictTone(verdict: Verdict): string {
  return VERDICT_META[verdict].tone
}

export function verdictGlyph(verdict: Verdict): string {
  return VERDICT_META[verdict].glyph
}

export function verdictShort(verdict: Verdict): string {
  return VERDICT_META[verdict].short
}

const CATEGORY_LABELS: Record<string, string> = {
  credit_card: 'Cartão de crédito',
  iban: 'IBAN',
  cpf_br: 'CPF',
  cnpj_br: 'CNPJ',
  steuer_id: 'Steuer-ID (DE)',
  email: 'E-mail',
  phone: 'Telefone',
}

export function categoryLabel(category: string): string {
  return CATEGORY_LABELS[category] ?? category.replace(/_/g, ' ')
}
