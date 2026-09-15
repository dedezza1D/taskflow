import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { Role, User } from '../api'
import { useModalFocus } from '../hooks'
import { describeError } from '../lib/errors'

const ROLES: { value: Role; label: string; blurb: string }[] = [
  {
    value: 'viewer',
    label: 'Viewer',
    blurb: 'consulta documentos e relatórios',
  },
  { value: 'analyst', label: 'Analyst', blurb: 'também envia documentos' },
  { value: 'admin', label: 'Admin', blurb: 'também apaga e gerencia usuários' },
]

// A generated passphrase beats one a person invents under pressure, and it is
// the only password that exists between creating the account and the new user
// changing it.
function generatePassword(): string {
  const bytes = new Uint8Array(18)
  crypto.getRandomValues(bytes)
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
}

interface Props {
  currentUserEmail: string
  onClose: () => void
  onNotify: (kind: 'ok' | 'error', message: string) => void
}

export function UsersDialog({ currentUserEmail, onClose, onNotify }: Props) {
  const [users, setUsers] = useState<User[] | null>(null)
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('viewer')
  const [password, setPassword] = useState(generatePassword)
  const [busy, setBusy] = useState(false)
  // Both row actions are irreversible and sit side by side, repeated once per
  // user, with labels that read identically. Ask which one was meant before
  // doing it: removing an account cannot be undone, and resetting a password
  // signs that person out wherever they are working.
  const [confirming, setConfirming] = useState<{
    id: string
    kind: 'remove' | 'reset'
  } | null>(null)
  const [error, setError] = useState<string | null>(null)
  // A password just issued, shown once so the admin can pass it on. Never
  // retrievable again — the server only ever stored the bcrypt hash.
  const [issued, setIssued] = useState<{
    email: string
    password: string
    kind: 'created' | 'reset'
  } | null>(null)

  const dialogRef = useRef<HTMLDivElement>(null)
  useModalFocus(dialogRef, onClose, busy)

  useEffect(() => {
    api
      .listUsers()
      .then((r) => setUsers(r.items))
      .catch((err) =>
        setError(describeError(err, 'Não foi possível carregar os usuários.')),
      )
  }, [])

  async function addUser(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const created = await api.createUser(email.trim(), password, role)
      setUsers((prev) => [...(prev ?? []), created])
      setIssued({ email: created.email, password, kind: 'created' })
      setEmail('')
      setPassword(generatePassword())
      setRole('viewer')
    } catch (err) {
      setError(describeError(err, 'Não foi possível criar o usuário.'))
    } finally {
      setBusy(false)
    }
  }

  async function removeUser(user: User) {
    setBusy(true)
    try {
      await api.deleteUser(user.id)
      setUsers((prev) => (prev ?? []).filter((u) => u.id !== user.id))
      onNotify(
        'ok',
        `${user.email} removido. As sessões dessa conta foram encerradas.`,
      )
    } catch (err) {
      onNotify(
        'error',
        `${user.email}: ${describeError(err, 'não foi possível remover a conta.')}`,
      )
    } finally {
      setBusy(false)
    }
  }

  // Resetting keeps the user's id — and therefore any record of what they did —
  // which deleting and recreating the account would throw away.
  async function resetPassword(user: User) {
    setBusy(true)
    setError(null)
    const fresh = generatePassword()
    try {
      await api.resetUserPassword(user.id, fresh)
      setIssued({ email: user.email, password: fresh, kind: 'reset' })
    } catch (err) {
      setError(
        `${user.email}: ${describeError(err, 'não foi possível redefinir a senha.')}`,
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="dialog-backdrop" onMouseDown={() => !busy && onClose()}>
      <div
        ref={dialogRef}
        className="dialog dialog-wide"
        role="dialog"
        aria-modal="true"
        aria-labelledby="users-title"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <h3 id="users-title">Usuários da organização</h3>
        <p>
          Não existe cadastro público: contas são criadas aqui, por um admin.
          Numa ferramenta que guarda dados pessoais, quem entra é uma decisão,
          não um formulário aberto.
        </p>

        {error && (
          <div className="banner banner-critical" role="alert">
            <span className="glyph" aria-hidden="true">
              ✕
            </span>
            {error}
          </div>
        )}

        {issued && (
          <div className="credential-card">
            <p className="credential-title">
              <span className="glyph" aria-hidden="true">
                ✓
              </span>
              {issued.email}{' '}
              {issued.kind === 'created' ? 'criado' : 'com senha redefinida'}
            </p>
            <p className="muted">
              Esta senha aparece uma única vez — o servidor guardou apenas o
              hash. Entregue por um canal seguro e oriente a pessoa a trocá-la
              em <strong>Senha</strong> logo no primeiro acesso: o sistema não
              obriga essa troca.
              {issued.kind === 'reset' &&
                ' As sessões abertas dessa conta foram encerradas.'}
            </p>
            <code className="credential-value">{issued.password}</code>
            <button
              type="button"
              className="ghost"
              onClick={() => setIssued(null)}
            >
              Já anotei
            </button>
          </div>
        )}

        {users === null ? (
          <div className="skeleton-row" />
        ) : (
          <ul className="user-list">
            {users.map((u) => (
              <li key={u.id}>
                <span className="user-email">{u.email}</span>
                <span className="identity-role">{u.role}</span>
                {u.email === currentUserEmail ? (
                  <span className="muted user-self">você</span>
                ) : (
                  <>
                    {confirming?.id === u.id ? (
                      <>
                        <span className="user-confirm">
                          {confirming.kind === 'remove'
                            ? 'Remover esta conta?'
                            : 'Redefinir e encerrar as sessões?'}
                        </span>
                        <button
                          type="button"
                          className={
                            confirming.kind === 'remove'
                              ? 'ghost danger'
                              : 'ghost'
                          }
                          disabled={busy}
                          onClick={() => {
                            const kind = confirming.kind
                            setConfirming(null)
                            if (kind === 'remove') void removeUser(u)
                            else void resetPassword(u)
                          }}
                        >
                          Confirmar
                        </button>
                        <button
                          type="button"
                          className="ghost"
                          disabled={busy}
                          onClick={() => setConfirming(null)}
                        >
                          Cancelar
                        </button>
                      </>
                    ) : (
                      <>
                        <button
                          type="button"
                          className="ghost"
                          disabled={busy}
                          aria-label={`Redefinir a senha de ${u.email}`}
                          onClick={() =>
                            setConfirming({ id: u.id, kind: 'reset' })
                          }
                        >
                          Redefinir senha
                        </button>
                        <button
                          type="button"
                          className="ghost danger"
                          disabled={busy}
                          aria-label={`Remover ${u.email}`}
                          onClick={() =>
                            setConfirming({ id: u.id, kind: 'remove' })
                          }
                        >
                          Remover
                        </button>
                      </>
                    )}
                  </>
                )}
              </li>
            ))}
          </ul>
        )}

        <form className="user-form" onSubmit={addUser}>
          <h4>Adicionar usuário</h4>

          <label className="field">
            E-mail
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
              autoComplete="off"
            />
          </label>

          <fieldset className="role-picker">
            <legend>Papel</legend>
            {ROLES.map((r) => (
              <label key={r.value} className="role-option">
                <input
                  type="radio"
                  name="role"
                  value={r.value}
                  checked={role === r.value}
                  onChange={() => setRole(r.value)}
                />
                <span>
                  <strong>{r.label}</strong>
                  <span className="muted"> — {r.blurb}</span>
                </span>
              </label>
            ))}
          </fieldset>

          <label className="field">
            Senha inicial
            <span className="password-row">
              <input
                type="text"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                minLength={12}
                required
                autoComplete="off"
                spellCheck={false}
              />
              <button
                type="button"
                className="ghost"
                onClick={() => setPassword(generatePassword())}
              >
                Gerar outra
              </button>
            </span>
          </label>

          <div className="dialog-actions">
            <button type="button" onClick={onClose} disabled={busy}>
              Fechar
            </button>
            <button type="submit" className="primary" disabled={busy}>
              {busy ? 'Criando…' : 'Criar usuário'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
