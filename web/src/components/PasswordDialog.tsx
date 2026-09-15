import { useRef, useState } from 'react'
import { api, ApiError } from '../api'
import { useModalFocus } from '../hooks'
import { describeError } from '../lib/errors'

const MIN_LENGTH = 12

interface Props {
  onClose: () => void
  /** Called after a successful change: the server revoked every session. */
  onPasswordChanged: () => void
}

export function PasswordDialog({ onClose, onPasswordChanged }: Props) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const dialogRef = useRef<HTMLFormElement>(null)
  useModalFocus(dialogRef, onClose, busy)

  const mismatch = confirm !== '' && next !== confirm
  const tooShort = next !== '' && next.length < MIN_LENGTH

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.changePassword(current, next)
      onPasswordChanged()
    } catch (err) {
      // A wrong current password comes back as invalid_credentials, which the
      // generic table phrases for the login form; say it for this one.
      setError(
        err instanceof ApiError && err.code === 'invalid_credentials'
          ? 'A senha atual está incorreta.'
          : describeError(err, 'Não foi possível trocar a senha.'),
      )
      setBusy(false)
    }
  }

  return (
    <div className="dialog-backdrop" onMouseDown={() => !busy && onClose()}>
      <form
        ref={dialogRef}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="password-title"
        onMouseDown={(e) => e.stopPropagation()}
        onSubmit={submit}
      >
        <h3 id="password-title">Alterar senha</h3>
        <p>
          Trocar a senha encerra todas as sessões, inclusive esta — uma sessão
          roubada não deve sobreviver à credencial que a originou. Você entrará
          de novo em seguida.
        </p>

        {error && (
          <div className="banner banner-critical" role="alert">
            <span className="glyph" aria-hidden="true">
              ✕
            </span>
            {error}
          </div>
        )}

        <label className="field">
          Senha atual
          <input
            type="password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            autoComplete="current-password"
            required
            autoFocus
          />
        </label>

        <label className="field">
          Nova senha
          <input
            type="password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            autoComplete="new-password"
            minLength={MIN_LENGTH}
            required
          />
          <span className="muted field-hint">
            {tooShort
              ? `faltam ${MIN_LENGTH - next.length} caracteres`
              : `mínimo de ${MIN_LENGTH} caracteres — uma frase longa vale mais que símbolos`}
          </span>
        </label>

        <label className="field">
          Confirme a nova senha
          <input
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            autoComplete="new-password"
            required
          />
          {mismatch && (
            <span className="field-error">as senhas não coincidem</span>
          )}
        </label>

        <div className="dialog-actions">
          <button type="button" onClick={onClose} disabled={busy}>
            Cancelar
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || mismatch || tooShort || next === ''}
          >
            {busy ? 'Alterando…' : 'Alterar senha'}
          </button>
        </div>
      </form>
    </div>
  )
}
