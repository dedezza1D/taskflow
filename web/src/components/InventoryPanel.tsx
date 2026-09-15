import { categoryLabel } from '../lib/compliance'
import type { Inventory } from '../lib/compliance'

/**
 * Corpus-level exposure: how many documents contain each category. One measure
 * over nominal categories, so every bar wears the same hue — bar length already
 * encodes the value, and spending colour on identity here would say nothing.
 */
function ExposureChart({ exposure }: { exposure: Inventory['exposure'] }) {
  const max = Math.max(...exposure.map((e) => e.documents), 1)

  return (
    <figure className="exposure">
      <figcaption>
        Documentos por categoria de dado pessoal
        <span className="muted">
          {' '}
          · {exposure.length} categorias detectadas
        </span>
      </figcaption>
      <ul className="exposure-rows">
        {exposure.map((e) => (
          <li key={e.category}>
            <span className="exposure-label">{categoryLabel(e.category)}</span>
            <span className="exposure-track">
              <span
                className="exposure-bar"
                style={{ inlineSize: `${(e.documents / max) * 100}%` }}
              />
            </span>
            <span className="exposure-value">
              {e.documents}
              <span className="muted">
                {' '}
                {e.occurrences !== e.documents && `(${e.occurrences} ocorr.)`}
              </span>
            </span>
          </li>
        ))}
      </ul>
    </figure>
  )
}

interface Props {
  inventory: Inventory
}

function splitLabel(withPersonalData: number, restricted: number): string {
  const common = withPersonalData - restricted
  return `${common} ${common === 1 ? 'comum' : 'comuns'} + ${restricted} ${restricted === 1 ? 'restrito' : 'restritos'}`
}

export function InventoryPanel({ inventory }: Props) {
  const {
    analysed,
    withPersonalData,
    restricted,
    unanalysed,
    pending,
    exposure,
  } = inventory

  return (
    <section className="inventory" aria-labelledby="inventory-heading">
      <h2 id="inventory-heading">Inventário de dados pessoais</h2>

      <div className="inventory-grid">
        <div className="tiles">
          <div className="tile tile-hero">
            <span className="tile-label">Documentos com dados pessoais</span>
            <span className="tile-value">{withPersonalData}</span>
            <span className="tile-sub">
              de {analysed} analisado{analysed === 1 ? '' : 's'}
              {pending > 0 && ` · ${pending} em processamento`}
            </span>
            {/* The filter chips split this number in two (Restritos / Dados
                pessoais), so say how it splits, or 4 here and 3 under
                "Dados pessoais" reads as a counting error. */}
            {restricted > 0 && (
              <span className="tile-sub">
                {splitLabel(withPersonalData, restricted)}
              </span>
            )}
          </div>

          <div className="tile">
            <span className="tile-label">
              <span className="glyph glyph-critical" aria-hidden="true">
                ▲
              </span>
              Restritos
            </span>
            <span className="tile-value">{restricted}</span>
            <span className="tile-sub">
              dados sensíveis ou financeiros — Art. 32 / 46
            </span>
          </div>

          <div className="tile">
            <span className="tile-label">
              <span className="glyph glyph-serious" aria-hidden="true">
                ●
              </span>
              Não analisados
            </span>
            <span className="tile-value">{unanalysed}</span>
            <span className="tile-sub">
              falharam ou sem relatório — exposição desconhecida
            </span>
          </div>
        </div>

        {exposure.length > 0 ? (
          <ExposureChart exposure={exposure} />
        ) : (
          <p className="exposure-empty muted">
            Nenhuma categoria detectada ainda. O gráfico de exposição aparece
            assim que o primeiro relatório for gerado.
          </p>
        )}
      </div>
    </section>
  )
}
