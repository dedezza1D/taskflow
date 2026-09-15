import { describe, expect, it } from 'vitest'
import type { ApiDocument, CategoryAssessment, Report } from '../api'
import { buildInventory, triage } from './compliance'

function doc(id: string, over: Partial<ApiDocument> = {}): ApiDocument {
  return {
    id,
    filename: `${id}.txt`,
    content_type: 'text/plain',
    storage_uri: `fs://documents/${id}/original`,
    status: 'completed',
    created_at: '2026-09-11T12:00:00Z',
    updated_at: '2026-09-11T12:00:00Z',
    version: 1,
    ...over,
  }
}

function report(categories: CategoryAssessment[]): Report {
  return {
    document_id: 'x',
    generated_at: '2026-09-11T12:00:00Z',
    detector_version: 'regex-v2',
    total_findings: categories.reduce((n, c) => n + c.count, 0),
    regulations: [{ regulation: 'gdpr', categories, obligations: [] }],
  }
}

const category = (
  name: string,
  over: Partial<CategoryAssessment> = {},
): CategoryAssessment => ({
  category: name,
  count: 1,
  class: 'personal data',
  special: false,
  notes: '',
  ...over,
})

describe('buildInventory', () => {
  // The tiles once added up to more than the corpus: a failed document was
  // counted as unanalysed AND as still processing, so 41 documents reported 50.
  // Whatever else changes, the three buckets must partition the corpus.
  it('partitions the corpus — no document is counted twice or left out', () => {
    const documents = [
      doc('analysed'),
      doc('failed', { status: 'failed', failed_stage: 'ocr' }),
      doc('unreadable'),
      doc('working', { status: 'processing' }),
    ]
    const reports = new Map([['analysed', report([category('email')])]])

    const inv = buildInventory(documents, reports, new Set(['unreadable']))

    expect(inv.analysed).toBe(1)
    expect(inv.unanalysed).toBe(2) // the failure and the unreadable report
    expect(inv.pending).toBe(1) // only the one genuinely in flight
    expect(inv.analysed + inv.unanalysed + inv.pending).toBe(documents.length)
  })

  it('counts a completed document with an unreadable report as unanalysed', () => {
    const documents = [doc('a')]
    const inv = buildInventory(documents, new Map(), new Set(['a']))

    expect(inv.unanalysed).toBe(1)
    expect(inv.pending).toBe(0)
  })

  it('leaves a document with no report yet in processing', () => {
    const inv = buildInventory([doc('a')], new Map(), new Set())

    expect(inv.pending).toBe(1)
    expect(inv.unanalysed).toBe(0)
  })

  it('separates restricted from merely personal', () => {
    const documents = [doc('personal'), doc('financial')]
    const reports = new Map([
      ['personal', report([category('email')])],
      ['financial', report([category('credit_card')])],
    ])

    const inv = buildInventory(documents, reports, new Set())

    expect(inv.withPersonalData).toBe(2)
    expect(inv.restricted).toBe(1)
  })

  it('sums exposure per category across the corpus', () => {
    const documents = [doc('a'), doc('b')]
    const reports = new Map([
      ['a', report([category('email', { count: 2 })])],
      ['b', report([category('email')])],
    ])

    const inv = buildInventory(documents, reports, new Set())
    const email = inv.exposure.find((e) => e.category === 'email')

    expect(email).toMatchObject({ documents: 2, occurrences: 3 })
  })
})

describe('triage', () => {
  it('reports a pipeline failure with the stage it stopped at', () => {
    const t = triage(doc('a', { status: 'failed', failed_stage: 'ocr' }), null)

    expect(t.verdict).toBe('unknown')
    // In words a reader recognises, not the internal stage id.
    expect(t.detail).toContain('leitura (OCR)')
  })

  // This is the case that used to read "Analisando" forever, next to a green
  // "analisado" badge and three completed stages on the same screen.
  it('distinguishes an unreadable report from work still in progress', () => {
    const stillWorking = triage(doc('a'), null, false)
    const broken = triage(doc('a'), null, true)

    expect(stillWorking.verdict).toBe('pending')
    expect(broken.verdict).toBe('unknown')
    expect(broken.title).not.toBe(stillWorking.title)
  })

  it('calls a document with no findings clear', () => {
    expect(triage(doc('a'), report([])).verdict).toBe('clear')
  })

  it('treats financial identifiers as restricted', () => {
    expect(triage(doc('a'), report([category('credit_card')])).verdict).toBe(
      'sensitive',
    )
  })

  it('treats a special category as restricted', () => {
    expect(
      triage(doc('a'), report([category('health', { special: true })])).verdict,
    ).toBe('sensitive')
  })
})
