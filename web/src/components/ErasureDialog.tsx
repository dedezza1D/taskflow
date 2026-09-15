import { useRef, useState } from 'react'
import type { DocumentArtifact } from '../api'
import { useModalFocus } from '../hooks'
import { formatDateTime } from '../lib/format'

const ARTIFACT_LABELS: Record<string, string> = {
  ocr: 'o texto extraído do arquivo',
  pii: 'os achados de dados pessoais (tipo e posição, sem os valores)',
  report: 'o relatório de conformidade',
}

interface Props {
  filename: string
  artifacts: DocumentArtifact[]
  /** When the original and the OCR text were already destroyed, if they were. */
  rawShreddedAt?: string
  busy: boolean
  onConfirm: () => void
  onCancel: () => void
}

/**
 * Erasure confirmation. A native confirm() cannot show what is about to be
 * destroyed, and is unreachable to any automated check — this lists what the
 * erasure removes and requires the filename to be typed.
 *
 * It lists what still EXISTS. After a report is generated the original and the
 * OCR text are already gone, and promising to delete them again overstated what
 * this action does — in a dialog whose whole job is to be exact.
 */
export function ErasureDialog({
  filename,
  artifacts,
  rawShreddedAt,
  busy,
  onConfirm,
  onCancel,
}: Props) {
  const [typed, setTyped] = useState('')
  const dialogRef = useRef<HTMLDivElement>(null)
  const matches = typed.trim() === filename
  useModalFocus(dialogRef, onCancel, busy)

  const remaining = artifacts.filter(
    (a) => !(rawShreddedAt && a.stage === 'ocr'),
  )

  return (
    <div className="dialog-backdrop" onMouseDown={() => !busy && onCancel()}>
      <div
        ref={dialogRef}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="erase-title"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <h3 id="erase-title">Apagar definitivamente</h3>
        <p>
          Direito ao esquecimento (GDPR Art. 17 / LGPD Art. 18). Serão removidos
          da base e do armazenamento:
        </p>

        <ul className="erase-list">
          {!rawShreddedAt && <li>o arquivo original enviado</li>}
          {remaining.map((a) => (
            <li key={a.id}>{ARTIFACT_LABELS[a.stage] ?? a.kind}</li>
          ))}
          <li>
            o registro do documento e o histórico de tentativas da análise
          </li>
        </ul>

        {rawShreddedAt && (
          <p className="muted">
            O arquivo original e o texto extraído já foram destruídos em{' '}
            {formatDateTime(rawShreddedAt)}, logo após a análise.
          </p>
        )}

        <p className="muted">
          Não pode ser desfeito. Se a exclusão for interrompida no meio, basta
          repetir.
        </p>

        <label className="erase-confirm">
          <span>
            Digite <strong>{filename}</strong> para confirmar
          </span>
          <input
            type="text"
            value={typed}
            disabled={busy}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            autoFocus
          />
        </label>

        <div className="dialog-actions">
          <button type="button" onClick={onCancel} disabled={busy}>
            Cancelar
          </button>
          <button
            type="button"
            className="danger"
            onClick={onConfirm}
            disabled={!matches || busy}
          >
            {busy ? 'Apagando…' : 'Apagar tudo'}
          </button>
        </div>
      </div>
    </div>
  )
}
