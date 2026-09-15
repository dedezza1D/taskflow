import { useState } from 'react'
import { api, ApiError } from '../api'
import { describeError } from '../lib/errors'

const MIN_LENGTH = 12

export function ResetPasswordScreen({
  token,
  onDone,
  onRequestNewLink,
}: {
  token: string
  onDone: () => void
  /** A spent or expired link is a dead end unless the screen offers the way out. */
  onRequestNewLink: () => void
}) {
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [linkDead, setLinkDead] = useState(false)
  const [done, setDone] = useState(false)

  const mismatch = confirm !== '' && next !== confirm
  const tooShort = next !== '' && next.length < MIN_LENGTH

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.resetPasswordWithToken(token, next)
      setDone(true)
    } catch (err) {
      setError(describeError(err, 'Não foi possível redefinir a senha.'))
      setLinkDead(
        err instanceof ApiError &&
          (err.code === 'token_used' || err.code === 'token_invalid'),
      )
      setBusy(false)
    }
  }

  if (done) {
    return (
      <div className="login-screen">
        <div className="login-card">
          <h1>Senha redefinida</h1>
          <p className="tagline">
            Sua senha foi trocada e todas as sessões anteriores foram
            encerradas. Entre com a senha nova.
          </p>
          <button type="button" className="primary" onClick={onDone}>
            Ir para o login
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="login-screen">
      <form className="login-card" onSubmit={submit}>
        <h1>Escolher nova senha</h1>
        <p className="tagline">
          Este link vale uma única vez. Ao concluir, todas as sessões abertas
          nesta conta serão encerradas.
        </p>

        {error && (
          <div className="banner banner-critical" role="alert">
            <span className="glyph" aria-hidden="true">
              ✕
            </span>
            {error}
          </div>
        )}

        {linkDead ? (
          <>
            <button
              type="button"
              className="primary"
              onClick={onRequestNewLink}
            >
              Pedir um novo link
            </button>
            <button type="button" className="link" onClick={onDone}>
              Voltar ao login
            </button>
          </>
        ) : (
          <>
            <label className="field">
              Nova senha
              <input
                type="password"
                value={next}
                onChange={(e) => setNext(e.target.value)}
                autoComplete="new-password"
                minLength={MIN_LENGTH}
                required
                autoFocus
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

            <button
              type="submit"
              className="primary"
              disabled={busy || mismatch || tooShort || next === ''}
            >
              {busy ? 'Salvando…' : 'Redefinir senha'}
            </button>
            <button type="button" className="link" onClick={onDone}>
              Cancelar
            </button>
          </>
        )}
      </form>
    </div>
  )
}
