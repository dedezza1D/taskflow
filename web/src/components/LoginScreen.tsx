import { useState } from 'react'
import { api } from '../api'
import type { Me } from '../api'
import { describeError } from '../lib/errors'

interface Props {
  onSignedIn: (me: Me) => void
  /** False when the server has no mailer; the link is hidden rather than dead. */
  recoveryEnabled: boolean
  onForgotPassword: () => void
  /** Why the user is back here — an expired session, a password change. */
  notice?: string | null
}

export function LoginScreen({
  onSignedIn,
  recoveryEnabled,
  onForgotPassword,
  notice,
}: Props) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onSignedIn(await api.login(email, password))
    } catch (err) {
      // The server deliberately answers the same code for an unknown address
      // and a wrong password; translating by code keeps that property intact.
      setError(describeError(err, 'Não foi possível entrar. Tente novamente.'))
      setBusy(false)
    }
  }

  return (
    <div className="login-screen">
      <form className="login-card" onSubmit={submit}>
        <h1>
          TaskFlow <span className="accent">Compliance</span>
        </h1>
        <p className="tagline">
          Entre para ver os documentos e relatórios da sua organização.
        </p>

        {notice && !error && (
          <div className="banner banner-info" role="status">
            <span className="glyph" aria-hidden="true">
              ℹ
            </span>
            {notice}
          </div>
        )}

        {error && (
          <div className="banner banner-critical" role="alert">
            <span className="glyph" aria-hidden="true">
              ✕
            </span>
            {error}
          </div>
        )}

        <label className="field">
          E-mail
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="username"
            required
            autoFocus
          />
        </label>

        <label className="field">
          Senha
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </label>

        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Entrando…' : 'Entrar'}
        </button>

        {recoveryEnabled && (
          <button type="button" className="link" onClick={onForgotPassword}>
            Esqueci minha senha
          </button>
        )}
      </form>
    </div>
  )
}
