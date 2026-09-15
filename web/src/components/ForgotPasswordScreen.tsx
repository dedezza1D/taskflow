import { useState } from 'react'
import { api } from '../api'
import { describeError } from '../lib/errors'

export function ForgotPasswordScreen({ onBack }: { onBack: () => void }) {
  const [email, setEmail] = useState('')
  const [busy, setBusy] = useState(false)
  const [sent, setSent] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.forgotPassword(email)
      setSent(true)
    } catch (err) {
      setError(
        describeError(
          err,
          'A recuperação de senha não está disponível neste servidor.',
        ),
      )
    } finally {
      setBusy(false)
    }
  }

  // The server answers identically for known and unknown addresses, so this
  // screen must too: saying "we sent it" only when the account exists would
  // undo that and turn the form into an account-existence oracle.
  if (sent) {
    return (
      <div className="login-screen">
        <div className="login-card">
          <h1>Verifique seu e-mail</h1>
          <p className="tagline">
            Se existir uma conta para <strong>{email}</strong>, enviamos um link
            para escolher uma senha nova. Ele vale por uma hora e só funciona
            uma vez.
          </p>
          <p className="muted">
            Não chegou? Confira a caixa de spam e o endereço digitado. Se a
            conta não existir, nenhuma mensagem é enviada.
          </p>
          <button type="button" onClick={onBack}>
            Voltar ao login
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="login-screen">
      <form className="login-card" onSubmit={submit}>
        <h1>Recuperar senha</h1>
        <p className="tagline">
          Informe seu e-mail e enviaremos um link para escolher uma senha nova.
        </p>

        {error && (
          <div className="banner banner-critical">
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

        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Enviando…' : 'Enviar link'}
        </button>
        <button type="button" className="link" onClick={onBack}>
          Voltar ao login
        </button>
      </form>
    </div>
  )
}
