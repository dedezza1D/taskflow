import { describe, expect, it } from 'vitest'
import { explainFailure } from './pipeline'

describe('explainFailure', () => {
  // The exact error the worker recorded for a text file renamed to .pdf, which
  // the panel used to present as "Dead-lettered em ocr" and nothing else.
  it('explains a corrupt PDF in plain words, with a next step', () => {
    const { reason, action } = explainFailure(
      'ocr',
      'stage ocr: unreadable pdf: page count: read context: xref table: no header version available',
    )

    expect(reason).toContain('PDF está corrompido')
    expect(action).toContain('envie-o de novo')
    expect(reason + action).not.toMatch(/xref|stage ocr|dead/i)
  })

  it('names the page limit when that is what failed', () => {
    const { reason } = explainFailure(
      'ocr',
      'stage ocr: pdf has 900 pages, over the 200-page limit',
    )
    expect(reason).toContain('limite')
  })

  it('falls back to the stage, in words, when the error is unknown or missing', () => {
    expect(explainFailure('pii', 'something unexpected').reason).toContain(
      'detecção',
    )
    expect(explainFailure('ocr', null).reason).toContain('leitura (OCR)')
  })
})
