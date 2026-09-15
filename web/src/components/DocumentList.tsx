import { useMemo, useState } from 'react'
import type { ApiDocument, Report } from '../api'
import { triage } from '../lib/compliance'
import { formatDateTime } from '../lib/format'
import type { Verdict } from '../lib/compliance'
import { StatusBadge, VerdictBadge } from './badges'

type Filter = 'all' | Verdict

const FILTERS: { value: Filter; label: string }[] = [
  { value: 'all', label: 'Todos' },
  { value: 'sensitive', label: 'Restritos' },
  { value: 'personal', label: 'Dados pessoais' },
  { value: 'clear', label: 'Livres' },
  { value: 'unknown', label: 'Não analisados' },
]

interface Props {
  documents: ApiDocument[]
  reports: Map<string, Report>
  unreadable: ReadonlySet<string>
  selectedId: string | null
  loading: boolean
  onSelect: (id: string) => void
}

export function DocumentList({
  documents,
  reports,
  unreadable,
  selectedId,
  loading,
  onSelect,
}: Props) {
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<Filter>('all')

  const rows = useMemo(
    () =>
      documents.map((doc) => ({
        doc,
        triage: triage(
          doc,
          reports.get(doc.id) ?? null,
          unreadable.has(doc.id),
        ),
      })),
    [documents, reports, unreadable],
  )

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return rows.filter(
      ({ doc, triage }) =>
        (filter === 'all' || triage.verdict === filter) &&
        (q === '' || doc.filename.toLowerCase().includes(q)),
    )
  }, [rows, query, filter])

  if (loading && documents.length === 0) {
    return (
      <div className="skeleton-list" aria-busy="true" aria-label="Carregando">
        {[0, 1, 2].map((i) => (
          <div key={i} className="skeleton-row" />
        ))}
      </div>
    )
  }

  if (documents.length === 0) {
    return (
      <div className="empty-state">
        <p className="empty-title">Nenhum documento ainda</p>
        <p>
          Envie um contrato, formulário ou digitalização. O TaskFlow lê o
          conteúdo, encontra os dados pessoais lá dentro — CPF, cartão, IBAN,
          Steuer-ID, e-mail, telefone — e diz quais obrigações de GDPR e LGPD
          isso dispara.
        </p>
      </div>
    )
  }

  return (
    <>
      <div className="list-controls">
        <input
          type="search"
          className="search"
          placeholder="Buscar por nome do arquivo"
          aria-label="Buscar por nome do arquivo"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div
          className="filter-chips"
          role="group"
          aria-label="Filtrar por veredito"
        >
          {FILTERS.map((f) => (
            <button
              key={f.value}
              type="button"
              className={`chip ${filter === f.value ? 'chip-active' : ''}`}
              aria-pressed={filter === f.value}
              onClick={() => setFilter(f.value)}
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>

      {visible.length === 0 ? (
        <p className="muted list-empty">
          Nenhum documento corresponde a esse filtro.
        </p>
      ) : (
        <ul className="doc-list">
          {visible.map(({ doc, triage }) => (
            <li key={doc.id}>
              <button
                type="button"
                className={`doc-row ${doc.id === selectedId ? 'doc-row-selected' : ''}`}
                aria-current={doc.id === selectedId}
                onClick={() => onSelect(doc.id)}
              >
                <span className="doc-row-main">
                  <span className="doc-name">{doc.filename}</span>
                  <span className="doc-meta muted">
                    {doc.content_type} · {formatDateTime(doc.created_at)}
                  </span>
                </span>
                <span className="doc-row-badges">
                  <VerdictBadge verdict={triage.verdict} />
                  {doc.status !== 'completed' && (
                    <StatusBadge
                      status={doc.status}
                      failedStage={doc.failed_stage}
                    />
                  )}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
