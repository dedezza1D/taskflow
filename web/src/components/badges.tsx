import type { DocumentStatus } from '../api'
import type { Verdict } from '../lib/compliance'
import { verdictGlyph, verdictShort, verdictTone } from '../lib/compliance'
import { stageLabel } from '../lib/pipeline'

// Status and verdict colours are a reserved scale, and two of them fall under
// 3:1 on the light surface by design — so every badge ships a glyph and a text
// label. Colour never carries the meaning on its own.

const STATUS_META: Record<
  DocumentStatus,
  { tone: string; glyph: string; label: string }
> = {
  uploaded: { tone: 'neutral', glyph: '○', label: 'na fila' },
  processing: { tone: 'warning', glyph: '◐', label: 'processando' },
  completed: { tone: 'good', glyph: '✓', label: 'analisado' },
  failed: { tone: 'critical', glyph: '✕', label: 'falhou' },
  erased: { tone: 'neutral', glyph: '⌫', label: 'apagado' },
}

// Short enough for a badge, and each with its own preposition.
const FAILED_AT: Record<string, string> = {
  ocr: 'falhou na leitura',
  pii: 'falhou na detecção',
  report: 'falhou no relatório',
}

export function StatusBadge({
  status,
  failedStage,
}: {
  status: DocumentStatus
  failedStage?: string
}) {
  const meta = STATUS_META[status]
  const label =
    status === 'failed' && failedStage
      ? (FAILED_AT[failedStage] ?? `falhou em ${stageLabel(failedStage)}`)
      : meta.label

  return (
    <span className={`badge badge-${meta.tone}`}>
      <span className="glyph" aria-hidden="true">
        {meta.glyph}
      </span>
      {label}
    </span>
  )
}

export function VerdictBadge({
  verdict,
  label,
}: {
  verdict: Verdict
  label?: string
}) {
  return (
    <span className={`badge badge-${verdictTone(verdict)}`}>
      <span className="glyph" aria-hidden="true">
        {verdictGlyph(verdict)}
      </span>
      {label ?? verdictShort(verdict)}
    </span>
  )
}
