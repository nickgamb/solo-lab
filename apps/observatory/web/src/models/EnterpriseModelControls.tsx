import type { BudgetRule, ModelView, TokenLimit } from '../api'

// EnterpriseModelControls: the spend controls only Solo Enterprise for
// agentgateway has, as rules: token limits per agent (the Solo rate
// limiter) and budgets (EnterpriseAgentgatewayBudget). Nothing on OSS.
export function EnterpriseModelControls({ view }: { view: ModelView }) {
  if (view.edition !== 'enterprise') return null
  const { budgets, rateLimits } = view.enterprise
  return (
    <section className="ent-models" aria-label="Solo Enterprise spend controls">
      <div className="row"><div className="label grow">Spend controls</div><span className="chip accent">Solo Enterprise</span></div>
      <div className="subtle small">Token limits per agent</div>
      {rateLimits.length ? rateLimits.map((l, i) => (
        <div key={i} className="row line small"><span className="ellipsis grow">{who(l)}</span><span className="mono">{limit(l)}</span></div>
      )) : <span className="subtle small">none</span>}
      <div className="subtle small" style={{ marginTop: 6 }}>Budgets</div>
      {budgets.length ? budgets.map(b => (
        <div key={b.resource + b.name} className="row line small" title={`${b.resource} · ${b.name}`}>
          <span className="ellipsis grow">{subject(b)}</span>
          <span className="mono">{amount(b)} a {b.window.toLowerCase()}</span>
          <span className={`chip ${b.action === 'Block' ? 'bad' : ''}`}>{b.action.toLowerCase()}</span>
        </div>
      )) : <span className="subtle small">none</span>}
    </section>
  )
}

const n = (v: number) => v.toLocaleString()
const who = (l: TokenLimit) => l.value ? (l.key === 'caller' ? l.value.split('/').pop() : `${l.key} ${l.value}`) : l.key === 'caller' ? 'every other agent' : `every ${l.key}`
const limit = (l: TokenLimit) => `${n(l.perUnit)} ${l.tokens ? 'tokens' : 'requests'} a ${l.unit.toLowerCase()}`
const subject = (b: BudgetRule) => {
  const s = b.subject ?? {}
  if (!Object.keys(s).length) return 'all calls'
  return Object.entries(s).map(([k, v]) => (k === 'virtualKey' ? `key ${v}` : `${k} ${v}`)).join(' · ')
}
const amount = (b: BudgetRule) => b.unit === 'USD' ? `$${n(b.amount)}` : `${n(b.amount)} ${b.amount === 1 ? 'token' : 'tokens'}`
