import { useState } from 'react'
import type { Report } from '../api'
import { categoryLabel } from '../lib/compliance'
import { formatDateTime } from '../lib/format'

const SPECIAL_LABEL: Record<string, string> = {
  gdpr: 'Art. 9',
  lgpd: 'Art. 5º II',
}

const REGULATION_LABEL: Record<string, string> = {
  gdpr: 'GDPR',
  lgpd: 'LGPD',
}

// The report keeps the classification as a stable machine value.
const CLASS_LABEL: Record<string, string> = {
  personal_data: 'dado pessoal',
  special_category: 'categoria especial',
  context_dependent: 'depende do contexto',
}

function downloadReport(report: Report, filename: string) {
  const blob = new Blob([JSON.stringify(report, null, 2)], {
    type: 'application/json',
  })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${filename}.report.json`
  a.click()
  URL.revokeObjectURL(url)
}

export function ReportView({
  report,
  filename,
}: {
  report: Report
  filename: string
}) {
  const sections = report.regulations ?? []
  const [active, setActive] = useState(sections[0]?.regulation ?? 'gdpr')
  const section = sections.find((s) => s.regulation === active) ?? sections[0]

  return (
    <div className="report">
      <div className="report-head">
        <h4>Relatório de conformidade</h4>
        <button
          type="button"
          className="ghost"
          onClick={() => downloadReport(report, filename)}
        >
          Exportar JSON
        </button>
      </div>

      <p className="report-meta muted">
        {report.total_findings} achado{report.total_findings === 1 ? '' : 's'} ·
        detector {report.detector_version} ·{' '}
        {formatDateTime(report.generated_at)}
      </p>

      {sections.length > 1 && (
        <div className="reg-tabs" role="tablist" aria-label="Regulação">
          {sections.map((s) => (
            <button
              key={s.regulation}
              type="button"
              role="tab"
              aria-selected={s.regulation === active}
              className={`reg-tab ${s.regulation === active ? 'active' : ''}`}
              onClick={() => setActive(s.regulation)}
            >
              {REGULATION_LABEL[s.regulation] ?? s.regulation.toUpperCase()}
            </button>
          ))}
        </div>
      )}

      {!section ? (
        <p className="muted">
          Este relatório é anterior às seções por regulação — apague e reenvie o
          documento para regerá-lo.
        </p>
      ) : (
        <>
          {section.categories && section.categories.length > 0 && (
            <ul className="finding-cards">
              {section.categories.map((c) => (
                <li key={c.category} className="finding-card">
                  <div className="finding-head">
                    <span className="finding-name">
                      {categoryLabel(c.category)}
                    </span>
                    <span className="finding-count">
                      {c.count}
                      <span className="muted">
                        {' '}
                        ocorrência{c.count === 1 ? '' : 's'}
                      </span>
                    </span>
                  </div>
                  <div className="finding-tags">
                    <span className="tag">
                      {CLASS_LABEL[c.class] ?? c.class.replace(/_/g, ' ')}
                    </span>
                    {c.special && (
                      <span className="tag tag-critical">
                        <span className="glyph" aria-hidden="true">
                          ▲
                        </span>
                        {SPECIAL_LABEL[section.regulation] ?? 'sensível'}
                      </span>
                    )}
                  </div>
                  <p className="finding-note muted">{c.notes}</p>
                </li>
              ))}
            </ul>
          )}

          <h5>Obrigações disparadas</h5>
          <ul className="obligations">
            {section.obligations.map((o) => (
              <li key={o}>{o}</li>
            ))}
          </ul>
        </>
      )}
    </div>
  )
}
