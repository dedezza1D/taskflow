import { useEffect, useState } from 'react'
import { api } from '../api'
import type {
  ApiDocument,
  DocumentArtifact,
  Report,
  TaskExecution,
} from '../api'
import { useDocumentDetail } from '../hooks'
import { triage, verdictGlyph, verdictTone } from '../lib/compliance'
import { describeError } from '../lib/errors'
import { formatDateTime, formatDuration } from '../lib/format'
import { explainFailure } from '../lib/pipeline'
import { StatusBadge } from './badges'
import { ReportView } from './ReportView'
import { ErasureDialog } from './ErasureDialog'

const STAGES = [
  { id: 'ocr', label: 'OCR', hint: 'extrai o texto' },
  { id: 'pii', label: 'Detecção', hint: 'encontra identificadores' },
  { id: 'report', label: 'Relatório', hint: 'mapeia obrigações' },
] as const

function StageTimeline({
  doc,
  artifacts,
}: {
  doc: ApiDocument
  artifacts: DocumentArtifact[]
}) {
  const byStage = new Map(artifacts.map((a) => [a.stage, a]))
  let previous = doc.created_at

  return (
    <ol className="timeline">
      {STAGES.map((stage) => {
        const artifact = byStage.get(stage.id)
        const state = artifact
          ? 'done'
          : doc.failed_stage === stage.id
            ? 'failed'
            : doc.status === 'processing' || doc.status === 'uploaded'
              ? 'running'
              : 'pending'

        const took = artifact
          ? formatDuration(previous, artifact.created_at)
          : ''
        if (artifact) previous = artifact.created_at

        return (
          <li key={stage.id} className={`tl tl-${state}`}>
            <span className="tl-dot" aria-hidden="true">
              {state === 'done' ? '✓' : state === 'failed' ? '✕' : ''}
            </span>
            <span className="tl-body">
              <span className="tl-label">{stage.label}</span>
              <span className="tl-hint muted">{stage.hint}</span>
            </span>
            {took && <span className="tl-time muted">{took}</span>}
          </li>
        )
      })}
    </ol>
  )
}

const EXECUTION_STATUS: Record<TaskExecution['status'], string> = {
  started: 'em andamento',
  succeeded: 'concluída',
  failed: 'falhou',
}

/**
 * The attempt ledger the worker keeps for every document. Collapsed by
 * default: the reason above is what most people need, and the recorded error is
 * technical detail for whoever has to diagnose it.
 */
function AttemptHistory({ executions }: { executions: TaskExecution[] }) {
  if (executions.length === 0) return null
  const ordered = [...executions].sort((a, b) => a.attempt - b.attempt)

  return (
    <details className="attempts">
      <summary>
        Histórico de tentativas{' '}
        <span className="muted">({ordered.length})</span>
      </summary>
      <ol className="attempt-list">
        {ordered.map((e) => (
          <li key={e.id} className={`attempt attempt-${e.status}`}>
            <span className="attempt-head">
              Tentativa {e.attempt} · {EXECUTION_STATUS[e.status]}
              <span className="muted">
                {' '}
                · {formatDateTime(e.started_at)}
                {e.finished_at &&
                  ` · ${formatDuration(e.started_at, e.finished_at)}`}
              </span>
            </span>
            {e.error && <code className="attempt-error">{e.error}</code>}
          </li>
        ))}
      </ol>
    </details>
  )
}

function FailurePanel({
  doc,
  executions,
  canErase,
}: {
  doc: ApiDocument
  executions: TaskExecution[] | null
  canErase: boolean
}) {
  const lastError = [...(executions ?? [])]
    .sort((a, b) => b.attempt - a.attempt)
    .find((e) => e.error)?.error
  const { reason, action } = explainFailure(doc.failed_stage, lastError)

  return (
    <div className="failure">
      <p className="failure-reason">
        <strong>Motivo:</strong> {reason}
      </p>
      <p className="failure-action">
        <strong>O que fazer:</strong> {action}
        {canErase &&
          ' Este registro com falha pode ser removido com Apagar (Art. 17).'}
      </p>
    </div>
  )
}

function Minimisation({ doc }: { doc: ApiDocument }) {
  let text: string
  let done = false
  if (doc.raw_shredded_at) {
    done = true
    text = `Original e texto extraído destruídos em ${formatDateTime(doc.raw_shredded_at)}. O que resta são os achados e o relatório, que não contêm nenhum valor bruto.`
  } else if (doc.status === 'failed') {
    // No report will ever be generated for a failure, so "destroyed once the
    // report exists" was a promise that could not be kept. The retention sweep
    // is what removes it.
    text =
      'O original ainda está guardado para permitir o diagnóstico da falha. Ele é destruído automaticamente ao fim do prazo de retenção (24 horas na configuração padrão), ou já, com Apagar (Art. 17).'
  } else {
    text =
      'O original e o texto extraído ficam guardados só enquanto a análise roda, e são destruídos assim que o relatório é gerado.'
  }

  return (
    <p className={`minimisation ${done ? '' : 'minimisation-pending'}`}>
      <span className="glyph" aria-hidden="true">
        {done ? '✓' : '○'}
      </span>
      {text}
    </p>
  )
}

interface Props {
  documentId: string
  /** The corpus pool's copy of the report, when it already has one. */
  knownReport: Report | null
  onReportLoaded: (id: string, report: Report) => void
  /** Erasure is admin-only; the server enforces it, this hides the affordance. */
  canErase: boolean
  onErased: (id: string) => void
  onNotify: (kind: 'ok' | 'error', message: string) => void
}

export function DocumentDetail({
  documentId,
  knownReport,
  onReportLoaded,
  canErase,
  onErased,
  onNotify,
}: Props) {
  const { doc, artifacts, report, gone, reportUnreadable, executions } =
    useDocumentDetail(documentId, knownReport, onReportLoaded)
  const [confirming, setConfirming] = useState(false)
  const [erasing, setErasing] = useState(false)

  // The document vanished underneath us (erased elsewhere, or a stale link).
  useEffect(() => {
    if (gone) onErased(documentId)
  }, [gone, documentId, onErased])

  if (gone) return null

  if (!doc) {
    return (
      <div className="detail-panel">
        <div className="skeleton-row" />
        <div className="skeleton-row" />
      </div>
    )
  }

  const t = triage(doc, report, reportUnreadable)

  async function erase() {
    setErasing(true)
    try {
      await api.eraseDocument(documentId)
      onNotify(
        'ok',
        'Documento apagado: arquivos, relatório e histórico removidos.',
      )
      onErased(documentId)
    } catch (e) {
      onNotify(
        'error',
        describeError(e, 'Não foi possível apagar o documento.'),
      )
    } finally {
      setErasing(false)
      setConfirming(false)
    }
  }

  return (
    <div className="detail-panel">
      <div className="detail-header">
        <div className="detail-title">
          <h3>{doc.filename}</h3>
          <StatusBadge status={doc.status} failedStage={doc.failed_stage} />
        </div>
        {canErase && (
          <button
            type="button"
            className="danger"
            onClick={() => setConfirming(true)}
          >
            Apagar (Art. 17)
          </button>
        )}
      </div>

      <div className={`verdict verdict-${verdictTone(t.verdict)}`}>
        <span className="verdict-glyph" aria-hidden="true">
          {verdictGlyph(t.verdict)}
        </span>
        <div>
          <p className="verdict-title">{t.title}</p>
          <p className="verdict-detail">{t.detail}</p>
        </div>
      </div>

      {doc.status === 'failed' && (
        <FailurePanel doc={doc} executions={executions} canErase={canErase} />
      )}

      <StageTimeline doc={doc} artifacts={artifacts} />

      {executions && <AttemptHistory executions={executions} />}

      <Minimisation doc={doc} />

      {report && <ReportView report={report} filename={doc.filename} />}

      {confirming && (
        <ErasureDialog
          filename={doc.filename}
          artifacts={artifacts}
          rawShreddedAt={doc.raw_shredded_at}
          busy={erasing}
          onConfirm={erase}
          onCancel={() => setConfirming(false)}
        />
      )}
    </div>
  )
}
