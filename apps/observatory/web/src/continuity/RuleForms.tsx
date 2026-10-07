import { useId, useState, type ReactNode } from 'react'
import type { AssuranceIdP, AssuranceLevel, AssurancePolicy, AssuranceView, EvalRule, Evaluation, PolicyPoint, ProfileSpec, Reach, RuleSchema, TierAssurance } from '../api'
import { critClass, DURATION, idpLabel, phaseChip, pointKey, type Draft } from './rulesCode'

// The Assurance rules window's forms. Everything they say about the chain
// comes from it: names from the IdentityContinuity and the cluster, choices
// from the CRDs (schema), outcomes and reasons from the assurance gate.

const OUTCOME: Record<Reach['outcome'], [string, string]> = { Admit: ['chip ok', 'admitted'], Conditional: ['chip warn', 'conditional'], Refuse: ['chip bad', 'refused'] }

// Outcome: the gate's outcome for one IdP; conditional names what the IdP
// must assert
export function Outcome({ r }: { r: Reach }) {
  const [cls, text] = OUTCOME[r.outcome] ?? ['chip', r.outcome]
  return (
    <span className="ar-outcome" title={r.reason}>
      <span className={cls}>{text}</span>
      {r.via && <span className="mono small ellipsis">{r.via}</span>}
    </span>
  )
}

function Section({ title, children, aside }: { title: string; children: ReactNode; aside?: ReactNode }) {
  return (
    <section className="ar-sec">
      <div className="ar-sec-head"><h4>{title}</h4>{aside}</div>
      {children}
    </section>
  )
}

// a field a rule can leave to the default rule
function Inherit<T>({ label, own, inherited, show, onOverride, onReset, children }: {
  label: string; own: T | undefined; inherited: T | undefined; show: (v: T | undefined) => string; onOverride: () => void; onReset: () => void; children: ReactNode
}) {
  const set = own !== undefined
  return (
    <div className={`ar-field${set ? ' set' : ''}`}>
      <span className="ar-field-label">{label}</span>
      <div className="ar-field-value">
        {set ? children : <span className="subtle">{show(inherited)}</span>}
      </div>
      {set
        ? <button className="cm-link" onClick={onReset} title="Leave it to the default rule">use default</button>
        : <button className="cm-link" onClick={onOverride}>override</button>}
    </div>
  )
}

function Plain({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="ar-field set">
      <span className="ar-field-label">{label}</span>
      <div className="ar-field-value">{children}</div>
      <span />
    </div>
  )
}

// TagInput: a list of values, from a list of known ones or typed in
export function TagInput({ values, options, placeholder, valid, onChange }: {
  values: string[]; options: string[]; placeholder: string; valid?: (v: string) => boolean; onChange: (v: string[]) => void
}) {
  const [text, setText] = useState('')
  const list = useId()
  const add = (raw: string) => {
    const v = raw.trim()
    if (!v || values.includes(v) || (valid && !valid(v))) return false
    onChange([...values, v])
    setText('')
    return true
  }
  const bad = !!text.trim() && !!valid && !valid(text.trim())
  return (
    <div className="ar-tags">
      {values.map(v => (
        <span key={v} className="ar-tag mono">{v}<button className="cm-x" aria-label={`Remove ${v}`} onClick={() => onChange(values.filter(x => x !== v))}>×</button></span>
      ))}
      <input className={`field ar-tag-input${bad ? ' invalid' : ''}`} list={list} value={text} placeholder={placeholder}
        onChange={e => { const v = e.target.value; if (options.includes(v.trim()) && add(v)) return; setText(v) }}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add(text) } }}
        onBlur={() => add(text)} />
      <datalist id={list}>{options.filter(o => !values.includes(o)).map(o => <option key={o} value={o} />)}</datalist>
    </div>
  )
}


// The requirements: the same fields, in the same order, for the default
// rule and for a rule (where each may be left to the default).
function Requirements({ schema, idps, policy, rule, setPolicy, setRule }: {
  schema: RuleSchema; idps: AssuranceIdP[]; policy: AssurancePolicy
  rule?: ProfileSpec; setPolicy?: (p: AssurancePolicy) => void; setRule?: (r: ProfileSpec) => void
}) {
  const upstreams = idps.filter(i => i.type !== 'local')
  const a = rule?.assurance ?? {}
  const setA = (patch: Partial<NonNullable<ProfileSpec['assurance']>>) => {
    if (!rule || !setRule) return
    const next = { ...a, ...patch }
    for (const k of Object.keys(next) as (keyof typeof next)[]) if (next[k] === undefined) delete next[k]
    setRule({ ...rule, assurance: Object.keys(next).length ? next : undefined })
  }
  const minimum = (v: string | undefined, on: (v: string) => void) => (
    <select className="field" value={v ?? ''} onChange={e => on(e.target.value)}>
      {schema.levels.map(l => <option key={l}>{l}</option>)}
    </select>
  )
  const sessions = (v: string | undefined, on: (v: string) => void) => (
    <select className="field" value={v ?? ''} onChange={e => on(e.target.value)}>
      {schema.sessions.map(l => <option key={l}>{l}</option>)}
    </select>
  )
  const bool = (v: boolean | undefined, on: (v: boolean) => void, label: string) => (
    <label className="tog"><input type="checkbox" checked={!!v} onChange={e => on(e.target.checked)} />{label}</label>
  )
  const age = (v: string | undefined, on: (v: string | undefined) => void) => (
    <input className={`field${v && !DURATION.test(v) ? ' invalid' : ''}`} value={v ?? ''} placeholder="12h" onChange={e => on(e.target.value || undefined)} />
  )
  const vouch = (v: string[] | undefined, on: (v: string[] | undefined) => void) => {
    const cur = v?.length ? v : upstreams.map(i => i.name)
    return (
      <div className="ar-opts">
        {upstreams.map(i => (
          <label key={i.name} className="tog">
            <input type="checkbox" checked={cur.includes(i.name)} onChange={e => {
              const next = upstreams.map(x => x.name).filter(n => (n === i.name ? e.target.checked : cur.includes(n)))
              on(next.length === upstreams.length ? undefined : next)
            }} />{idpLabel(i)}
          </label>
        ))}
      </div>
    )
  }
  const names = (v?: string[]) => (v?.length ? v.map(n => idpLabel(idps.find(i => i.name === n) ?? { name: n })).join(', ') : 'every IdP in the chain')

  if (!rule || !setRule) {
    const p = policy, set = setPolicy!
    return (
      <>
        <Plain label="Minimum assurance">{minimum(p.minimum ?? schema.defaults.minimum, v => set({ ...p, minimum: v }))}</Plain>
        <Plain label="Phishing-resistant">{bool(p.phishingResistant, v => set({ ...p, phishingResistant: v || undefined }), 'required')}</Plain>
        <Plain label="Signed in within">{age(p.maxAge, v => set({ ...p, maxAge: v }))}</Plain>
        <Plain label="IdPs that may vouch">{vouch(p.allowedIdPs, v => set({ ...p, allowedIdPs: v }))}</Plain>
        <Plain label="Break-glass accounts">{bool(p.allowBreakGlass, v => set({ ...p, allowBreakGlass: v || undefined }), 'allowed')}</Plain>
        <Plain label="Sessions">{sessions(p.sessions ?? schema.defaults.sessions, v => set({ ...p, sessions: v }))}</Plain>
      </>
    )
  }
  const r = rule
  return (
    <>
      <Inherit label="Minimum assurance" own={a.minimum} inherited={policy.minimum ?? schema.defaults.minimum} show={v => v ?? ''}
        onOverride={() => setA({ minimum: policy.minimum ?? schema.defaults.minimum })} onReset={() => setA({ minimum: undefined })}>
        {minimum(a.minimum, v => setA({ minimum: v }))}
      </Inherit>
      <Inherit label="Phishing-resistant" own={a.phishingResistant} inherited={policy.phishingResistant} show={v => (v ? 'required' : 'not required')}
        onOverride={() => setA({ phishingResistant: !!policy.phishingResistant })} onReset={() => setA({ phishingResistant: undefined })}>
        {bool(a.phishingResistant, v => setA({ phishingResistant: v }), 'required')}
      </Inherit>
      <Inherit label="Signed in within" own={a.maxAge} inherited={policy.maxAge} show={v => v ?? 'any time'}
        onOverride={() => setA({ maxAge: policy.maxAge ?? '' })} onReset={() => setA({ maxAge: undefined })}>
        {age(a.maxAge, v => setA({ maxAge: v ?? '' }))}
      </Inherit>
      <Inherit label="IdPs that may vouch" own={r.allowedIdPs?.length ? r.allowedIdPs : undefined} inherited={policy.allowedIdPs} show={names}
        onOverride={() => setRule({ ...r, allowedIdPs: policy.allowedIdPs?.length ? policy.allowedIdPs : upstreams.map(i => i.name) })}
        onReset={() => setRule({ ...r, allowedIdPs: undefined })}>
        {vouch(r.allowedIdPs, v => setRule({ ...r, allowedIdPs: v ?? upstreams.map(i => i.name) }))}
      </Inherit>
      <Inherit label="Break-glass accounts" own={r.allowBreakGlass} inherited={policy.allowBreakGlass} show={v => (v ? 'allowed' : 'not allowed')}
        onOverride={() => setRule({ ...r, allowBreakGlass: !!policy.allowBreakGlass })} onReset={() => setRule({ ...r, allowBreakGlass: undefined })}>
        {bool(r.allowBreakGlass, v => setRule({ ...r, allowBreakGlass: v }), 'allowed')}
      </Inherit>
      <Inherit label="Sessions" own={r.sessions} inherited={policy.sessions ?? schema.defaults.sessions} show={v => v ?? ''}
        onOverride={() => setRule({ ...r, sessions: policy.sessions ?? schema.defaults.sessions })} onReset={() => setRule({ ...r, sessions: undefined })}>
        {sessions(r.sessions, v => setRule({ ...r, sessions: v }))}
      </Inherit>
    </>
  )
}

// Enforced at: the gateway policies that take the broker's tokens, and the
// rule each asks the gate for.
function EnforcedAt({ view, draft, rule, setPoint }: {
  view: AssuranceView; draft: Draft; rule: string; setPoint: (p: PolicyPoint, rule: string | null) => void
}) {
  if (!view.policyPoints.length) return <p className="subtle small">No gateway policy takes {view.realm ?? view.name}'s tokens.</p>
  return (
    <ul className="ar-points">
      {view.policyPoints.map(p => {
        const k = pointKey(p), cur = draft.points[k]
        const mine = cur === rule
        const other = !p.extAuth && cur !== null && cur !== undefined && !mine
        return (
          <li key={k} className={mine ? 'on' : ''}>
            <label className="tog">
              <input type="checkbox" checked={mine} disabled={!!p.extAuth || other} onChange={e => setPoint(p, e.target.checked ? rule : null)} />
              <span className="mono">{p.namespace}/{p.name}</span>
            </label>
            <span className="subtle small ellipsis">{p.gateway}</span>
            {p.extAuth && <span className="chip" title="its extAuth goes elsewhere">extAuth {p.extAuth}</span>}
            {other && <span className="chip">{cur || 'default'}</span>}
            {mine && <span className="chip ok">{p.failureMode ?? 'FailClosed'}</span>}
          </li>
        )
      })}
    </ul>
  )
}

function Outcomes({ view, er }: { view: AssuranceView; er?: EvalRule }) {
  if (!er) return <p className="subtle small">…</p>
  return (
    <table className="ar-table">
      <tbody>
        {er.idps.map(r => {
          const i = view.idps.find(x => x.name === r.idp)
          return (
            <tr key={r.idp}>
              <td className="ar-idp-cell">{idpLabel(i ?? { name: r.idp })}{r.idp === view.active && <span className="chip accent">active</span>}</td>
              <td><Outcome r={r} /></td>
              <td className="subtle small">{r.reason}</td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

export function RuleForm({ view, draft, name, ev, setDraft, onRemove }: {
  view: AssuranceView; draft: Draft; name: string; ev?: Evaluation
  setDraft: (f: (d: Draft) => Draft) => void; onRemove?: () => void
}) {
  const schema = view.schema!
  const isDefault = name === ''
  const rule = isDefault ? undefined : draft.rules[name] ?? undefined
  const er = ev?.rules.find(r => r.name === name)
  const setRule = (r: ProfileSpec) => setDraft(d => ({ ...d, rules: { ...d.rules, [name]: r } }))
  const setPoint = (p: PolicyPoint, rule: string | null) => setDraft(d => ({ ...d, points: { ...d.points, [pointKey(p)]: rule } }))
  const saved = view.rules.find(r => r.name === name)
  return (
    <div className="ar-form">
      <div className="ar-title">
        <h3 className="grow ellipsis">{isDefault ? 'Default rule' : name}</h3>
        {rule?.criticality && <span className={critClass(schema, rule.criticality)}>{rule.criticality}</span>}
        {er && <span className={phaseChip(er.phase)} title={`eligible: ${er.eligible.join(', ') || 'none'}`}>{er.phase}</span>}
        {rule && (
          <select className="field ar-mode" value={rule.mode ?? schema.defaults.mode} onChange={e => setRule({ ...rule, mode: e.target.value })} aria-label="mode">
            {schema.modes.map(m => <option key={m}>{m}</option>)}
          </select>
        )}
        {onRemove && <button className="btn small ghost danger-text" onClick={onRemove}>Remove</button>}
      </div>
      {!saved && rule && <p className="subtle small">New: created on Save.</p>}

      {rule && (
        <Section title="About">
          <Plain label="Description"><input className="field" value={rule.description ?? ''} onChange={e => setRule({ ...rule, description: e.target.value || undefined })} /></Plain>
          <Plain label="Criticality">
            <select className={`field${rule.criticality ? '' : ' invalid'}`} value={rule.criticality} onChange={e => setRule({ ...rule, criticality: e.target.value })}>
              {!rule.criticality && <option value="">choose</option>}
              {schema.criticality.map(c => <option key={c}>{c}</option>)}
            </select>
          </Plain>
          <Plain label="Owner"><input className="field" value={rule.owner ?? ''} onChange={e => setRule({ ...rule, owner: e.target.value || undefined })} /></Plain>
          <Plain label="Obligations">
            <TagInput values={rule.obligations ?? []} options={[...new Set(view.rules.flatMap(r => r.spec.obligations ?? []))]} placeholder="add"
              onChange={v => setRule({ ...rule, obligations: v.length ? v : undefined })} />
          </Plain>
        </Section>
      )}
      {rule && (
        <Section title="Applies to">
          <Plain label="Workloads">
            <TagInput values={(rule.workloads ?? []).map(w => `${w.namespace}/${w.serviceAccount}`)} options={view.options.workloads} placeholder="namespace/service account"
              valid={v => /^[a-z0-9-]+\/[a-z0-9.-]+$/.test(v)}
              onChange={v => setRule({ ...rule, workloads: v.length ? v.map(x => { const [namespace, serviceAccount] = x.split('/'); return { namespace, serviceAccount } }) : undefined })} />
          </Plain>
          <Plain label="Broker clients">
            <TagInput values={rule.clients ?? []} options={view.options.clients} placeholder="client ID"
              onChange={v => setRule({ ...rule, clients: v.length ? v : undefined })} />
          </Plain>
        </Section>
      )}
      <Section title="Requirements">
        <Requirements schema={schema} idps={view.idps} policy={draft.policy} rule={rule}
          setPolicy={p => setDraft(d => ({ ...d, policy: p }))} setRule={rule ? setRule : undefined} />
      </Section>
      <Section title="Enforced at">
        <EnforcedAt view={view} draft={draft} rule={name} setPoint={setPoint} />
      </Section>
      <Section title="Outcome by IdP" aside={<span className="subtle small">from the assurance gate</span>}>
        <Outcomes view={view} er={er} />
      </Section>
    </div>
  )
}

// IdpForm: what one IdP's sign-ins prove, its trust checks, and what it
// gets under each rule.
export function IdpForm({ view, idp, draft, ev, setDraft, recheck }: {
  view: AssuranceView; idp: AssuranceIdP; draft: Draft; ev?: Evaluation
  setDraft: (f: (d: Draft) => Draft) => void; recheck: () => Promise<void>
}) {
  const schema = view.schema!
  const ta: TierAssurance = draft.idps[idp.name] ?? {}
  const set = (next: TierAssurance) => setDraft(d => ({ ...d, idps: { ...d.idps, [idp.name]: next.default || next.levels?.length ? next : undefined } }))
  const rows = ta.levels ?? []
  const put = (i: number, l: AssuranceLevel | null) => {
    const next = rows.map((x, j) => (j === i ? l : x)).filter((x): x is AssuranceLevel => !!x)
    set({ ...ta, levels: next.length ? next : undefined })
  }
  const [busy, setBusy] = useState(false)
  const ceiling = ev?.idps.find(i => i.name === idp.name)?.ceiling
  return (
    <div className="ar-form">
      <div className="ar-title">
        <h3 className="grow ellipsis">{idpLabel(idp)}</h3>
        <span className="cm-type">{idp.type}</span>
        {idp.name === view.active && <span className="chip accent">active</span>}
        {!idp.enabled && <span className="chip">disabled</span>}
        {ceiling && <span className="chip">at most {ceiling}</span>}
      </div>
      <Section title="What its sign-ins prove">
        <table className="ar-table ar-levels">
          <thead><tr><th>claim</th><th>value</th><th>level</th><th>phishing-resistant</th><th /></tr></thead>
          <tbody>
            {rows.map((l, i) => (
              <tr key={i}>
                <td>
                  <select className="field" value={l.acr !== undefined ? 'acr' : 'amr'} aria-label="claim" onChange={e => {
                    const v = l.acr ?? l.amr ?? ''
                    put(i, e.target.value === 'acr' ? { ...l, acr: v, amr: undefined } : { ...l, amr: v, acr: undefined })
                  }}><option>acr</option><option>amr</option></select>
                </td>
                <td><input className={`field mono${(l.acr ?? l.amr) ? '' : ' invalid'}`} value={l.acr ?? l.amr ?? ''} aria-label="value"
                  onChange={e => put(i, l.acr !== undefined ? { ...l, acr: e.target.value } : { ...l, amr: e.target.value })} /></td>
                <td><select className="field" value={l.level} aria-label="level" onChange={e => put(i, { ...l, level: e.target.value })}>
                  {schema.levels.map(x => <option key={x}>{x}</option>)}</select></td>
                <td><input type="checkbox" checked={!!l.phishingResistant} aria-label="phishing-resistant" onChange={e => put(i, { ...l, phishingResistant: e.target.checked || undefined })} /></td>
                <td><button className="cm-x" aria-label="Remove" onClick={() => put(i, null)}>×</button></td>
              </tr>
            ))}
            <tr>
              <td colSpan={2}><button className="cm-addbtn" onClick={() => set({ ...ta, levels: [...rows, { acr: '', level: schema.levels[schema.levels.length - 1] }] })}>+ value</button></td>
              <td colSpan={3} />
            </tr>
          </tbody>
        </table>
        <Plain label="Any other sign-in">
          <select className="field" value={ta.default ?? ''} onChange={e => set({ ...ta, default: e.target.value || undefined })}>
            <option value="">unset</option>
            {schema.levels.map(x => <option key={x}>{x}</option>)}
          </select>
        </Plain>
      </Section>
      <Section title="Trust checks" aside={
        <>
          {idp.checkedAt && <span className="subtle small">{new Date(idp.checkedAt).toLocaleTimeString()}</span>}
          <button className="btn small" disabled={busy} onClick={async () => { setBusy(true); try { await recheck() } finally { setBusy(false) } }}>Check now</button>
        </>
      }>
        {idp.checks?.length
          ? <ul className="ar-checks">{idp.checks.map(c => <li key={c.name}><span className={`cell ${c.result}`}>{c.result === 'Pass' ? '✓' : c.result === 'Fail' ? '✗' : '?'}</span> {c.name} <span className="subtle small">{c.message}</span></li>)}</ul>
          : <p className="subtle small">None yet.</p>}
      </Section>
      <Section title="Outcome by rule" aside={<span className="subtle small">from the assurance gate</span>}>
        <table className="ar-table">
          <tbody>
            {(ev?.rules ?? []).map(r => {
              const reach = r.idps.find(x => x.idp === idp.name)
              return (
                <tr key={r.name}>
                  <td className="mono">{r.name || 'default'}</td>
                  <td>{reach ? <Outcome r={reach} /> : <span className="chip">not in the chain</span>}</td>
                  <td className="subtle small">{reach?.reason}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </Section>
    </div>
  )
}

const DECISION: Record<string, string> = { allow: 'chip ok', deny: 'chip bad', 'would-deny': 'chip warn', off: 'chip' }

// WhatIf: one sign-in, as an IdP would assert it, against every rule.
export function WhatIf({ view, run }: { view: AssuranceView; run: (s: { idp: string; acr?: string; amr?: string[]; authTime?: number }) => Promise<Evaluation> }) {
  const [idp, setIdp] = useState(view.active ?? view.idps[0]?.name ?? '')
  const [acr, setAcr] = useState('')
  const [amr, setAmr] = useState('')
  const [ago, setAgo] = useState('')
  const [out, setOut] = useState<Evaluation>()
  const [err, setErr] = useState<string>()
  const [busy, setBusy] = useState(false)
  const tier = view.idps.find(i => i.name === idp)
  const known = tier?.assurance?.levels ?? []
  const agoBad = !!ago && !/^[0-9]+$/.test(ago)
  const go = async () => {
    if (agoBad) return
    setBusy(true); setErr(undefined)
    try {
      setOut(await run({ idp: tier?.type === 'local' ? '' : idp, acr: acr || undefined, amr: amr.split(/[\s,]+/).filter(Boolean),
        authTime: ago ? Math.floor(Date.now() / 1000) - Number(ago) * 60 : undefined }))
    } catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }
  return (
    <div className="ar-whatif scroll">
      <div className="ar-whatif-form">
        <label>IdP
          <select className="field" value={idp} onChange={e => setIdp(e.target.value)}>
            {view.idps.map(i => <option key={i.name} value={i.name}>{idpLabel(i)}</option>)}
          </select>
        </label>
        <label>acr
          <input className="field mono" list="ar-acr" value={acr} onChange={e => setAcr(e.target.value)} placeholder="none" />
          <datalist id="ar-acr">{known.filter(l => l.acr).map(l => <option key={l.acr} value={l.acr} />)}</datalist>
        </label>
        <label>amr
          <input className="field mono" value={amr} onChange={e => setAmr(e.target.value)} placeholder={known.filter(l => l.amr).map(l => l.amr).join(' ') || 'none'} />
        </label>
        <label>signed in (minutes ago)
          <input className={`field${agoBad ? ' invalid' : ''}`} value={ago} onChange={e => setAgo(e.target.value)} placeholder="not asserted" />
        </label>
        <button className="btn primary" disabled={busy || agoBad} onClick={go}>Evaluate</button>
      </div>
      {err && <div className="note bad">{err}</div>}
      {out && (
        <table className="ar-table">
          <thead><tr><th>rule</th><th>mode</th><th>decision</th><th>status</th><th>reason</th></tr></thead>
          <tbody>
            {out.rules.map(r => (
              <tr key={r.name}>
                <td className="mono">{r.name || 'default'}</td>
                <td className="subtle small">{r.mode}</td>
                <td><span className={DECISION[r.session?.decision ?? ''] ?? 'chip'}>{r.session?.decision}</span></td>
                <td className="mono">{r.session?.status}</td>
                <td className="small">{r.session?.reason}{r.session?.acrValues && <span className="subtle"> · acr_values {r.session.acrValues}</span>}{r.session?.maxAge && <span className="subtle"> · max_age {r.session.maxAge}</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
