// Typed client for the TaskFlow API. In dev, Vite proxies /api to :8080
// (see vite.config.ts), so all paths here are same-origin relative.

export type DocumentStatus =
  'uploaded' | 'processing' | 'completed' | 'failed' | 'erased'

export interface ApiDocument {
  id: string
  filename: string
  content_type: string
  storage_uri: string
  status: DocumentStatus
  failed_stage?: string
  task_id?: string
  created_at: string
  updated_at: string
  version: number
  /** When the original and OCR text were destroyed; absent while still held. */
  raw_shredded_at?: string
}

export interface DocumentArtifact {
  id: string
  document_id: string
  stage: string
  kind: string
  storage_uri: string
  created_at: string
}

export interface CategoryAssessment {
  category: string
  count: number
  class: string
  special: boolean
  notes: string
}

export interface RegulationSection {
  regulation: string // "gdpr" | "lgpd"
  categories?: CategoryAssessment[] | null
  obligations: string[]
}

export interface Report {
  document_id: string
  generated_at: string
  detector_version: string
  total_findings: number
  regulations: RegulationSection[]
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, details?: string) {
    super(details || code)
    this.status = status
    this.code = code
  }
}

export interface TaskExecution {
  id: string
  task_id: string
  attempt: number
  status: 'started' | 'succeeded' | 'failed'
  error?: string
  started_at: string
  finished_at?: string
}

// A session can end underneath an open tab: it expires, the password changes
// elsewhere, an admin resets it. Every call that gets "unauthenticated" back
// says so here, once, so the app returns to the login screen instead of each
// caller guessing — the list poll used to report it as "API fora do ar".
type SessionListener = () => void
const sessionListeners = new Set<SessionListener>()

export function onSessionExpired(listener: SessionListener): () => void {
  sessionListeners.add(listener)
  return () => {
    sessionListeners.delete(listener)
  }
}

// Only `unauthenticated` means "no session". A 401 `invalid_credentials` is a
// wrong password typed into a form that is still signed in (or signing in).
async function send(path: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(path, init)
  if (res.ok) return res

  let code = 'http_error'
  let details = res.statusText
  try {
    const body = await res.json()
    code = body.error ?? code
    details = body.details ?? details
  } catch {
    // non-JSON error body (an nginx 502 page, say); keep statusText
  }
  if (res.status === 401 && code === 'unauthenticated') {
    for (const listener of sessionListeners) listener()
  }
  throw new ApiError(res.status, code, details)
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await send(path, init)
  return res.json() as Promise<T>
}

function jsonBody(body: unknown): RequestInit {
  return {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }
}

export type Role = 'viewer' | 'analyst' | 'admin'

export interface Me {
  email: string
  role: Role
  org_id: string
  /** True when the server runs without authentication (the desktop build). */
  local: boolean
}

const ROLE_RANK: Record<Role, number> = { viewer: 1, analyst: 2, admin: 3 }

export function can(role: Role | undefined, need: Role): boolean {
  return role !== undefined && ROLE_RANK[role] >= ROLE_RANK[need]
}

export interface User {
  id: string
  org_id: string
  email: string
  role: Role
  created_at: string
}

export interface AuthConfig {
  auth_enabled: boolean
  password_recovery: boolean
}

export const api = {
  me: () => request<Me>('/api/v1/auth/me'),

  authConfig: () => request<AuthConfig>('/api/v1/auth/config'),

  // Always 204 — the server refuses to say whether the address is registered.
  forgotPassword: async (email: string) => {
    await send('/api/v1/auth/forgot-password', jsonBody({ email }))
  },

  resetPasswordWithToken: async (token: string, newPassword: string) => {
    await send(
      '/api/v1/auth/reset-password',
      jsonBody({ token, new_password: newPassword }),
    )
  },

  listUsers: () => request<{ items: User[] }>('/api/v1/users'),

  createUser: (email: string, password: string, role: Role) =>
    request<User>('/api/v1/users', jsonBody({ email, password, role })),

  deleteUser: async (id: string) => {
    await send(`/api/v1/users/${id}`, { method: 'DELETE' })
  },

  resetUserPassword: async (id: string, password: string) => {
    await send(`/api/v1/users/${id}/password`, jsonBody({ password }))
  },

  changePassword: async (currentPassword: string, newPassword: string) => {
    await send(
      '/api/v1/auth/password',
      jsonBody({
        current_password: currentPassword,
        new_password: newPassword,
      }),
    )
  },

  login: (email: string, password: string) =>
    request<Me>('/api/v1/auth/login', jsonBody({ email, password })),

  logout: () =>
    fetch('/api/v1/auth/logout', { method: 'POST' }).then(() => undefined),

  listDocuments: () =>
    request<{ items: ApiDocument[] }>('/api/v1/documents?limit=100'),

  getDocument: (id: string) =>
    request<{ document: ApiDocument; artifacts: DocumentArtifact[] }>(
      `/api/v1/documents/${id}`,
    ),

  getReport: (id: string) => request<Report>(`/api/v1/documents/${id}/report`),

  listExecutions: (taskId: string) =>
    request<{ items: TaskExecution[] }>(
      `/api/v1/tasks/${taskId}/executions?limit=50`,
    ),

  uploadDocument: (file: File, priority: string) => {
    const form = new FormData()
    form.append('file', file)
    if (priority !== 'normal') form.append('priority', priority)
    return request<{ document: ApiDocument; task_id: string }>(
      '/api/v1/documents',
      { method: 'POST', body: form },
    )
  },

  eraseDocument: (id: string) =>
    request<{ document_id: string; status: string }>(
      `/api/v1/documents/${id}`,
      { method: 'DELETE' },
    ),
}
