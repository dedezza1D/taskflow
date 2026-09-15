import type { Toast } from '../hooks'

export function Toasts({
  toasts,
  onDismiss,
}: {
  toasts: Toast[]
  onDismiss: (id: number) => void
}) {
  if (toasts.length === 0) return null

  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={`toast toast-${t.kind}`}>
          <span className="glyph" aria-hidden="true">
            {t.kind === 'ok' ? '✓' : '✕'}
          </span>
          <span>{t.message}</span>
          <button
            type="button"
            className="toast-close"
            aria-label="Dispensar"
            onClick={() => onDismiss(t.id)}
          >
            ×
          </button>
        </div>
      ))}
    </div>
  )
}
