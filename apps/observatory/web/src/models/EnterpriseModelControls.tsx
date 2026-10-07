import type { ModelView } from '../api'

// EnterpriseModelControls: model controls only the enterprise gateway has.
// For now, what is there: token budgets and rate limits, by name. Nothing on
// OSS.
export function EnterpriseModelControls({ view }: { view: ModelView }) {
  if (view.edition !== 'enterprise') return null
  const { budgets, rateLimits } = view.enterprise
  return (
    <section className="ent-models" aria-label="Enterprise model controls">
      <div className="label">Enterprise controls</div>
      <dl className="ent-list small">
        <dt className="subtle">Budgets</dt>
        <dd className="mono">{budgets.length ? budgets.join(', ') : <span className="subtle">none</span>}</dd>
        <dt className="subtle">Rate limits</dt>
        <dd className="mono">{rateLimits.length ? rateLimits.join(', ') : <span className="subtle">none</span>}</dd>
      </dl>
    </section>
  )
}
