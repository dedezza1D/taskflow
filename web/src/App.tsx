import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useDocuments, useReports, useToasts } from './hooks'
import { buildInventory } from './lib/compliance'
import { UploadZone } from './components/UploadZone'
import { InventoryPanel } from './components/InventoryPanel'
import { DocumentList } from './components/DocumentList'
import { DocumentDetail } from './components/DocumentDetail'
import { Toasts } from './components/Toasts'
import { ThemeToggle } from './components/ThemeToggle'
import { LoginScreen } from './components/LoginScreen'
import { ForgotPasswordScreen } from './components/ForgotPasswordScreen'
import { ResetPasswordScreen } from './components/ResetPasswordScreen'
import { UsersDialog } from './components/UsersDialog'
import { PasswordDialog } from './components/PasswordDialog'
import { api, can, ApiError, onSessionExpired } from './api'
import type { Me } from './api'
import { describeError } from './lib/errors'

type Screen = 'login' | 'forgot' | 'reset'

// Why the login screen is showing, when it is not simply the first visit.
// Landing there with no explanation reads as a crash.
const SIGNED_OUT_NOTICE = {
  expired: 'Sua sessão expirou. Entre novamente para continuar.',
  passwordChanged:
    'Senha alterada. Por segurança, todas as sessões foram encerradas — entre com a senha nova.',
} as const
type SignedOutReason = keyof typeof SIGNED_OUT_NOTICE

// The recovery link lands on /reset-password?token=… and nginx serves index.html
// for it, so the token is read straight off the URL — no router needed for one
// entry point.
function tokenFromUrl(): string | null {
  const params = new URLSearchParams(window.location.search)
  const token = params.get('token')
  return window.location.pathname.startsWith('/reset-password') && token
    ? token
    : null
}

export default function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [checkingSession, setCheckingSession] = useState(true)
  const [recoveryEnabled, setRecoveryEnabled] = useState(false)
  const [resetToken, setResetToken] = useState<string | null>(tokenFromUrl)
  const [screen, setScreen] = useState<Screen>(() =>
    tokenFromUrl() ? 'reset' : 'login',
  )
  const [signedOutReason, setSignedOutReason] =
    useState<SignedOutReason | null>(null)

  const signOut = useCallback((reason: SignedOutReason | null) => {
    setSignedOutReason(reason)
    setScreen('login')
    setMe(null)
  }, [])

  // Any call that finds the session gone lands here. Only acted on while signed
  // in: the boot-time /auth/me answers "unauthenticated" for every first visit.
  const signedIn = useRef(false)
  signedIn.current = me !== null
  useEffect(
    () =>
      onSessionExpired(() => {
        if (signedIn.current) signOut('expired')
      }),
    [signOut],
  )

  // One call answers both "is there a session?" and "does this build even have
  // login?" — the desktop build replies with the local principal.
  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setCheckingSession(false))

    api
      .authConfig()
      .then((c) => setRecoveryEnabled(c.password_recovery))
      .catch(() => setRecoveryEnabled(false))
  }, [])

  function leaveReset() {
    // Drop the token from the address bar so it does not linger in history or
    // get re-submitted on reload.
    window.history.replaceState(null, '', '/')
    setResetToken(null)
    setScreen('login')
  }

  if (screen === 'reset' && resetToken) {
    return (
      <ResetPasswordScreen
        token={resetToken}
        onDone={leaveReset}
        onRequestNewLink={() => {
          leaveReset()
          setScreen('forgot')
        }}
      />
    )
  }
  if (checkingSession) return <div className="boot" aria-busy="true" />
  if (!me) {
    if (screen === 'forgot') {
      return <ForgotPasswordScreen onBack={() => setScreen('login')} />
    }
    return (
      <LoginScreen
        onSignedIn={(signedIn) => {
          setSignedOutReason(null)
          setMe(signedIn)
        }}
        recoveryEnabled={recoveryEnabled}
        onForgotPassword={() => setScreen('forgot')}
        notice={signedOutReason ? SIGNED_OUT_NOTICE[signedOutReason] : null}
      />
    )
  }
  return <Dashboard me={me} onSignOut={signOut} />
}

function Dashboard({
  me,
  onSignOut,
}: {
  me: Me
  onSignOut: (reason: SignedOutReason | null) => void
}) {
  const { documents, apiDown, loading, addLocally, removeLocally } =
    useDocuments()
  const { reports, unreadable, remember } = useReports(documents)
  const { toasts, push, dismiss } = useToasts()
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [showUsers, setShowUsers] = useState(false)
  const [showPassword, setShowPassword] = useState(false)

  const inventory = useMemo(
    () => buildInventory(documents, reports, unreadable),
    [documents, reports, unreadable],
  )

  async function handleUpload(file: File, priority: string) {
    try {
      const { document } = await api.uploadDocument(file, priority)
      addLocally(document)
      setSelectedId(document.id)
    } catch (e) {
      // An expired session is already handled — api.ts sent the app back to
      // the login screen with an explanation. A toast on top would be noise.
      if (e instanceof ApiError && e.code === 'unauthenticated') return
      push(
        'error',
        `${file.name}: ${describeError(e, 'não foi possível enviar o arquivo.')}`,
      )
    }
  }

  async function handleSignOut() {
    await api.logout()
    onSignOut(null)
  }

  const handleErased = useCallback(
    (id: string) => {
      removeLocally(id)
      setSelectedId((cur) => (cur === id ? null : cur))
    },
    [removeLocally],
  )

  return (
    <div className="app">
      <header className="app-header">
        <div>
          <h1>
            TaskFlow <span className="accent">Compliance</span>
          </h1>
          <p className="tagline">
            Descubra que dados pessoais existem nos seus documentos, decida o
            que pode ser compartilhado, e apague tudo quando for preciso.
          </p>
        </div>
        <div className="header-actions">
          {!me.local && (
            <div className="identity">
              <span className="identity-email">{me.email}</span>
              <span className="identity-role">{me.role}</span>
              {can(me.role, 'admin') && (
                <button
                  type="button"
                  className="ghost"
                  onClick={() => setShowUsers(true)}
                >
                  Usuários
                </button>
              )}
              <button
                type="button"
                className="ghost"
                onClick={() => setShowPassword(true)}
              >
                Senha
              </button>
              <button type="button" className="ghost" onClick={handleSignOut}>
                Sair
              </button>
            </div>
          )}
          <ThemeToggle />
        </div>
      </header>

      {showUsers && (
        <UsersDialog
          currentUserEmail={me.email}
          onClose={() => setShowUsers(false)}
          onNotify={push}
        />
      )}

      {showPassword && (
        <PasswordDialog
          onClose={() => setShowPassword(false)}
          onPasswordChanged={() => onSignOut('passwordChanged')}
        />
      )}

      {apiDown && (
        <div className="banner banner-critical" role="alert">
          <span className="glyph" aria-hidden="true">
            ✕
          </span>
          Não foi possível falar com o servidor. A lista abaixo pode estar
          desatualizada — tentando de novo automaticamente.
        </div>
      )}

      {can(me.role, 'analyst') ? (
        <UploadZone
          onUpload={handleUpload}
          onReject={(msg) => push('error', msg)}
        />
      ) : (
        <p className="readonly-note muted">
          Seu papel é <strong>{me.role}</strong> — você pode consultar
          documentos e relatórios, mas não enviar novos.
        </p>
      )}

      {documents.length > 0 && <InventoryPanel inventory={inventory} />}

      <div className={`content ${selectedId ? 'content-split' : ''}`}>
        <section className="list-pane" aria-labelledby="docs-heading">
          <h2 id="docs-heading">Documentos</h2>
          <DocumentList
            documents={documents}
            reports={reports}
            unreadable={unreadable}
            selectedId={selectedId}
            loading={loading}
            onSelect={setSelectedId}
          />
        </section>

        {selectedId && (
          <section className="detail-pane" aria-label="Detalhe do documento">
            <DocumentDetail
              documentId={selectedId}
              knownReport={reports.get(selectedId) ?? null}
              onReportLoaded={remember}
              canErase={can(me.role, 'admin')}
              onErased={handleErased}
              onNotify={push}
            />
          </section>
        )}
      </div>

      <Toasts toasts={toasts} onDismiss={dismiss} />
    </div>
  )
}
