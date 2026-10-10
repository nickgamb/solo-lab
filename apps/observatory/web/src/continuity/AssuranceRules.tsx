import { lazy, Suspense, useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { checkTrust, deleteProfile, evaluate, getAssurance, getAssuranceLogic, isConflict, putContinuity, putPolicyPoint, putProfile,
  type AssuranceView, type EvalSession, type Evaluation, type IdentityContinuity } from '../api'
import { Guard } from '../topology/DetailsPanel'
import { clone, critClass, draftOf, DURATION, fromCode, idpLabel, phaseChip, pointKey, refOf, same, toCode, type CodeError, type Draft } from './rulesCode'
import { IdpForm, RuleForm, WhatIf } from './RuleForms'

const CodeEditor = lazy(() => import('./CodeEditor'))

type Tab = 'rules' | 'idps' | 'whatif' | 'code'
const TABS: [Tab, string][] = [['rules', 'Rules'], ['idps', 'IdPs'], ['whatif', 'What if'], ['code', 'Code']]
type Selection = { kind: 'rule'; name: string } | { kind: 'uncovered'; name: string }
const NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

// AssuranceRules: what a sign-in must prove to reach what relies on the
// broker. The rules, most critical first, then what no rule covers, then the
// default rule every rule starts from (the chain's assurancePolicy). Each
// opens in the same form; what a rule decides is the assurance gate's own
// answer (its evaluate API), for the rules as edited. Saved as the
// signed-in admin: the chain, each rule, and the gateway policies that ask
// the gate for one.
export function AssuranceRules({ ic, onClose }: { ic: IdentityContinuity; onClose: () => void }) {
  const ns = ic.metadata.namespace, chain = ic.metadata.name
  const [view, setView] = useState<AssuranceView>()
  const [draft, setDraft] = useState<Draft>()
  const [base, setBase] = useState<Draft>()
  const [sel, setSel] = useState<Selection>({ kind: 'rule', name: '' })
  const [idpSel, setIdpSel] = useState<string>()
  const [tab, setTab] = useState<Tab>('rules')
  const [code, setCode] = useState('')
  const [codeBase, setCodeBase] = useState('')
  const [codeErrs, setCodeErrs] = useState<CodeError[]>()
  // the Code tab shows the rules, or the gate's decision logic they feed
  const [codeView, setCodeView] = useState<'rules' | 'logic'>('rules')
  const [logic, setLogic] = useState<{ text?: string; error?: string }>()
  const [ev, setEv] = useState<Evaluation>()
  const [evErr, setEvErr] = useState<string>()
  const [msg, setMsg] = useState<{ ok: boolean; text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const [adding, setAdding] = useState<string>()
  const dialog = useRef<HTMLDivElement>(null)

  const show = useCallback((v: AssuranceView) => {
    const d = draftOf(v)
    setView(v); setDraft(d); setBase(d)
    return { v, d }
  }, [])
  const load = useCallback(() => getAssurance(ns, chain).then(show), [ns, chain, show])
  // after a save: the view once it shows what was written (the server reads
  // a watch cache, a moment behind its own writes)
  const settle = async (wrote: { rules: Record<string, string | null>; points: Record<string, string> }) => {
    for (let i = 0; ; i++) {
      const v = await getAssurance(ns, chain)
      const ok = Object.entries(wrote.rules).every(([n, rv]) => (rv === null ? !v.rules.some(r => r.name === n) : v.rules.find(r => r.name === n)?.resourceVersion === rv))
        && Object.entries(wrote.points).every(([k, rv]) => v.policyPoints.find(p => pointKey(p) === k)?.resourceVersion === rv)
      if (ok || i >= 20) return show(v)
      await new Promise(r => setTimeout(r, 250))
    }
  }
  useEffect(() => {
    let live = true
    getAssurance(ns, chain).then(v => {
      if (!live) return
      show(v)
      // open on the most critical rule
      const crit = (c: string) => { const i = v.schema?.criticality.indexOf(c) ?? -1; return i < 0 ? 99 : i }
      const first = [...v.rules].sort((a, b) => crit(a.spec.criticality) - crit(b.spec.criticality) || a.name.localeCompare(b.name))[0]
      if (first) setSel({ kind: 'rule', name: first.name })
    }, e => { if (live) setMsg({ ok: false, text: (e as Error).message }) })
    return () => { live = false }
  }, [ns, chain, show])

  // the gate's answer for the rules as edited
  const evalDraft = (d: Draft) => ({ policy: d.policy, rules: d.rules, tiers: Object.fromEntries(Object.entries(d.idps).map(([k, a]) => [k, a ?? null])) })
  useEffect(() => {
    if (!draft) return
    let live = true
    const t = setTimeout(() => {
      evaluate(ns, chain, evalDraft(draft)).then(r => { if (live) { setEv(r); setEvErr(undefined) } }, e => { if (live) setEvErr((e as Error).message) })
    }, 250)
    return () => { live = false; clearTimeout(t) }
  }, [draft, ns, chain])

  const dirty = !!draft && !!base && (!same(draft, base) || (tab === 'code' && code !== codeBase))
  const edit = (f: (d: Draft) => Draft) => setDraft(d => (d ? f(clone(d)) : d))

  const applyCode = (): Draft | undefined => {
    if (!draft || !view || code === codeBase) return draft
    const r = fromCode(code, view, chain)
    if ('errors' in r) { setCodeErrs(r.errors); return undefined }
    setDraft(r.draft); setCodeErrs(undefined)
    return r.draft
  }
  const go = (next: Tab): boolean => {
    if (next === tab || !draft || !view) return true
    const d = tab === 'code' ? applyCode() : draft
    if (!d) return false
    if (next === 'code') { const c = toCode(d, view); setCode(c); setCodeBase(c); setCodeErrs(undefined) }
    setTab(next)
    return true
  }
  const loadLogic = () => {
    getAssuranceLogic(ns, chain).then(text => setLogic({ text }), e => setLogic({ error: (e as Error).message }))
  }
  const showLogic = () => { setCodeView('logic'); if (!logic?.text) loadLogic() }
  const onTabKey = (e: ReactKeyboardEvent) => {
    const i = TABS.findIndex(([t]) => t === tab)
    const j = e.key === 'ArrowRight' ? (i + 1) % TABS.length : e.key === 'ArrowLeft' ? (i + TABS.length - 1) % TABS.length : -1
    if (j < 0) return
    e.preventDefault()
    if (go(TABS[j][0])) document.getElementById(`ar-tab-${TABS[j][0]}`)?.focus()
  }

  const save = async () => {
    if (!view || !base) return
    const d = tab === 'code' ? applyCode() : draft
    if (!d) return
    const bad = invalid(d, view)
    if (bad) { setMsg({ ok: false, text: bad }); return }
    setBusy(true); setMsg(undefined)
    try {
      // the chain: its default rule and its IdPs' assurance, as one spec write
      if (!same(d.policy, base.policy) || !same(d.idps, base.idps)) {
        const spec = clone(ic.spec)
        spec.assurancePolicy = d.policy
        spec.tiers = spec.tiers.map(t => (same(d.idps[t.name], base.idps[t.name]) ? t : { ...t, assurance: d.idps[t.name] }))
        await putContinuity(ns, chain, ic.metadata.resourceVersion, spec)
      }
      const wrote: { rules: Record<string, string | null>; points: Record<string, string> } = { rules: {}, points: {} }
      // rules added or changed, before anything asks the gate for them
      for (const [name, spec] of Object.entries(d.rules)) {
        if (spec && !same(spec, base.rules[name])) wrote.rules[name] = (await putProfile(ns, name, view.rules.find(r => r.name === name)?.resourceVersion, spec)).metadata.resourceVersion
      }
      // the gateway policies: those that stop asking first
      const moved = view.policyPoints.filter(p => !p.extAuth && d.points[pointKey(p)] !== base.points[pointKey(p)])
      for (const p of [...moved.filter(p => d.points[pointKey(p)] === null), ...moved.filter(p => d.points[pointKey(p)] !== null)]) {
        wrote.points[pointKey(p)] = (await putPolicyPoint(ns, chain, p, d.points[pointKey(p)] ?? null)).resourceVersion
      }
      // rules removed, once nothing asks for them
      for (const [name, spec] of Object.entries(d.rules)) if (spec === null && base.rules[name]) { await deleteProfile(ns, name); wrote.rules[name] = null }
      const fresh = await settle(wrote)
      if (tab === 'code') { const c = toCode(fresh.d, fresh.v); setCode(c); setCodeBase(c) }
      setMsg({ ok: true, text: 'Saved. The assurance gate applies it to the next request.' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message, conflict: isConflict(e) })
    } finally { setBusy(false) }
  }
  const reset = async () => {
    setMsg(undefined); setCodeErrs(undefined)
    const { v, d } = await load()
    if (tab === 'code') { const c = toCode(d, v); setCode(c); setCodeBase(c) }
  }
  const close = () => {
    if (dirty && !window.confirm('Discard the unsaved assurance rules?')) return
    onClose()
  }
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !e.defaultPrevented) close() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })
  useEffect(() => {
    const root = document.getElementById('root')
    const prev = document.activeElement as HTMLElement | null
    root?.setAttribute('inert', '')
    dialog.current?.focus()
    return () => { root?.removeAttribute('inert'); prev?.focus() }
  }, [])

  const addRule = (name: string, from?: AssuranceView['uncovered'][number]) => {
    edit(d => {
      d.rules[name] = { continuity: chain, criticality: '', ...(from?.workloads?.length ? { workloads: from.workloads } : {}), ...(from?.clients?.length ? { clients: from.clients } : {}) }
      const p = from?.source === 'policyPoint' ? view?.policyPoints.find(x => `${x.namespace}/${x.name}` === from.ref) : undefined
      if (p) d.points[pointKey(p)] = name
      return d
    })
    setSel({ kind: 'rule', name }); setAdding(undefined)
  }
  const removeRule = (name: string) => {
    edit(d => {
      if (view?.rules.some(r => r.name === name)) d.rules[name] = null
      else delete d.rules[name]
      for (const k of Object.keys(d.points)) if (d.points[k] === name) d.points[k] = null
      return d
    })
    setSel({ kind: 'rule', name: '' })
  }
  const freeName = (n: string) => { let s = n, i = 2; while (draft && s in draft.rules && draft.rules[s] !== null) s = `${n}-${i++}`; return s }

  const ruleNames = draft && view?.schema
    ? Object.keys(draft.rules).filter(n => draft.rules[n]).sort((a, b) => {
      const ci = (n: string) => { const i = view.schema!.criticality.indexOf(draft.rules[n]!.criticality); return i < 0 ? 99 : i }
      return ci(a) - ci(b) || a.localeCompare(b)
    })
    : []
  const evOf = (n: string) => ev?.rules.find(r => r.name === n)
  const pointsOf = (n: string) => Object.values(draft?.points ?? {}).filter(r => r === n).length
  const uncovered = (view?.uncovered ?? []).filter(u => !(draft?.rules[u.name]))
  const addError = adding !== undefined && adding !== '' && (!NAME.test(adding) || !!draft?.rules[adding]) ? (draft?.rules[adding] ? 'taken' : 'lowercase letters, digits and -') : undefined

  return createPortal(
    <div className="cm-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <div className="cm-window" role="dialog" aria-modal="true" aria-labelledby="ar-title" tabIndex={-1} ref={dialog}>
        <header className="cm-header">
          <h2 id="ar-title">Assurance rules</h2>
          <span className="mono subtle">{ns}/{chain}</span>
          <div className="tabs" role="tablist" aria-label="Assurance rules views" onKeyDown={onTabKey}>
            {TABS.map(([t, label]) => (
              <button key={t} id={`ar-tab-${t}`} role="tab" aria-selected={tab === t} tabIndex={tab === t ? 0 : -1}
                className={tab === t ? 'tab active' : 'tab'} onClick={() => go(t)}>
                {label}{t === 'code' && <span className="cm-adv">advanced</span>}
              </button>
            ))}
          </div>
          <span className="grow" />
          {view?.gate && <span className="subtle small mono" title="the assurance gate these rules are evaluated and enforced by">gate {view.gate.namespace}/{view.gate.name}</span>}
          {dirty && <span className="chip warn">unsaved</span>}
          <button className="btn small ghost" disabled={!dirty || busy} onClick={reset}>Reset</button>
          <button className="btn small primary" disabled={!dirty || busy} onClick={save}>Save</button>
          <button className="btn small" onClick={close} title="Close (Esc)">Close</button>
        </header>
        {msg && (
          <div className={`note ${msg.ok ? 'ok' : 'bad'} cm-msg`}>
            {msg.text}
            {msg.conflict && <> <button className="btn small" onClick={reset}>Reload</button></>}
          </div>
        )}
        {view?.schemaError && <div className="note bad cm-msg">{view.schemaError}</div>}
        {evErr && <div className="note bad cm-msg">{evErr}</div>}
        {view && draft && view.schema && (
          <div className="cm-body">
            {tab === 'rules' && (
              <div className="ar-split">
                <nav className="ar-list scroll" aria-label="Rules">
                  <div className="ar-group">Rules</div>
                  {ruleNames.map(n => {
                    const r = draft.rules[n]!, e = evOf(n)
                    return (
                      <button key={n} className={`ar-item${sel.kind === 'rule' && sel.name === n ? ' on' : ''}`} onClick={() => setSel({ kind: 'rule', name: n })}>
                        <span className="row"><b className="grow ellipsis">{n}</b>{r.criticality && <span className={critClass(view.schema, r.criticality)}>{r.criticality}</span>}</span>
                        <span className="row wrap">
                          {e && <span className={phaseChip(e.phase)}>{e.phase}</span>}
                          {r.mode && r.mode !== view.schema!.defaults.mode && <span className="chip warn">{r.mode}</span>}
                          <span className="subtle small">{e?.effective.minimum}{pointsOf(n) ? '' : ' · not enforced'}</span>
                        </span>
                      </button>
                    )
                  })}
                  {adding === undefined
                    ? <button className="cm-addbtn ar-add" onClick={() => setAdding('')}>+ Add rule</button>
                    : (
                      <form className="ar-add-form" onSubmit={e => { e.preventDefault(); if (adding && !addError) addRule(adding) }}>
                        <input className={`field mono${addError ? ' invalid' : ''}`} autoFocus value={adding} placeholder="rule name" onChange={e => setAdding(e.target.value)}
                          onKeyDown={e => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); setAdding(undefined) } }} />
                        <button className="btn small" disabled={!adding || !!addError}>Add</button>
                        {addError && <span className="small danger-text">{addError}</span>}
                      </form>
                    )}
                  {!!uncovered.length && <div className="ar-group">Not covered</div>}
                  {uncovered.map(u => (
                    <button key={u.name} className={`ar-item bare${sel.kind === 'uncovered' && sel.name === u.name ? ' on' : ''}`} onClick={() => setSel({ kind: 'uncovered', name: u.name })}>
                      <span className="row"><b className="grow ellipsis">{u.label}</b><span className="cm-type">{u.source === 'sso' ? 'sign-in' : 'policy'}</span></span>
                      <span className="subtle small ellipsis mono">{u.ref}</span>
                    </button>
                  ))}
                  <div className="ar-group">Default</div>
                  <button className={`ar-item${sel.kind === 'rule' && sel.name === '' ? ' on' : ''}`} onClick={() => setSel({ kind: 'rule', name: '' })}>
                    <span className="row"><b className="grow">Default rule</b>{evOf('') && <span className={phaseChip(evOf('')!.phase)}>{evOf('')!.phase}</span>}</span>
                    <span className="subtle small">{evOf('')?.effective.minimum}{pointsOf('') ? '' : ' · not enforced'}</span>
                  </button>
                </nav>
                <section className="ar-detail scroll">
                  {sel.kind === 'rule' && (sel.name === '' || draft.rules[sel.name]) && (
                    <RuleForm key={sel.name} view={view} draft={draft} name={sel.name} ev={ev} setDraft={edit}
                      onRemove={sel.name ? () => removeRule(sel.name) : undefined} />
                  )}
                  {sel.kind === 'uncovered' && (() => {
                    const u = uncovered.find(x => x.name === sel.name)
                    if (!u) return null
                    return (
                      <div className="ar-form">
                        <div className="ar-title"><h3 className="grow">{u.label}</h3><span className="cm-type">{u.source === 'sso' ? 'sign-in' : 'policy'}</span></div>
                        <dl className="ar-facts">
                          <dt>{u.source === 'sso' ? 'SSO' : 'Policy'}</dt><dd className="mono">{u.ref}</dd>
                          {!!u.hosts?.length && <><dt>Hosts</dt><dd className="mono">{u.hosts.join(', ')}</dd></>}
                          {!!u.workloads?.length && <><dt>Workloads</dt><dd className="mono">{u.workloads.map(w => `${w.namespace}/${w.serviceAccount}`).join(', ')}</dd></>}
                          {!!u.clients?.length && <><dt>Broker clients</dt><dd className="mono">{u.clients.join(', ')}</dd></>}
                        </dl>
                        <div><button className="btn small primary" onClick={() => addRule(freeName(u.name), u)}>Add a rule for it</button></div>
                      </div>
                    )
                  })()}
                </section>
              </div>
            )}
            {tab === 'idps' && (
              <div className="ar-split">
                <nav className="ar-list scroll" aria-label="IdPs">
                  <div className="ar-group">In failover order</div>
                  {view.idps.map((i, n) => {
                    const on = (idpSel ?? view.idps[0]?.name) === i.name
                    const fails = i.checks?.filter(c => c.result === 'Fail').length ?? 0
                    return (
                      <button key={i.name} className={`ar-item${on ? ' on' : ''}`} onClick={() => setIdpSel(i.name)}>
                        <span className="row"><span className="order">{n + 1}</span><b className="grow ellipsis">{idpLabel(i)}</b>{i.name === view.active && <span className="chip accent">active</span>}</span>
                        <span className="row wrap">
                          <span className="cm-type">{i.type}</span>
                          {ev?.idps.find(x => x.name === i.name) && <span className="subtle small">at most {ev.idps.find(x => x.name === i.name)!.ceiling}</span>}
                          {!!i.checks?.length && <span className={fails ? 'chip bad' : 'chip ok'}>{fails ? `${fails} trust check${fails > 1 ? 's' : ''} failing` : 'trust ok'}</span>}
                        </span>
                      </button>
                    )
                  })}
                </nav>
                <section className="ar-detail scroll">
                  {(() => {
                    const i = view.idps.find(x => x.name === (idpSel ?? view.idps[0]?.name))
                    return i && <IdpForm key={i.name} view={view} idp={i} draft={draft} ev={ev} setDraft={edit}
                      recheck={async () => { await checkTrust(ns, chain); setTimeout(() => { getAssurance(ns, chain).then(v => setView(v)) }, 4000) }} />
                  })()}
                </section>
              </div>
            )}
            {tab === 'whatif' && <WhatIf view={view} run={(s: EvalSession) => evaluate(ns, chain, evalDraft(draft), s)} />}
            {tab === 'code' && (
              <div className="cm-code">
                <div className="row">
                  <div className="seg" role="radiogroup" aria-label="Code view">
                    <button role="radio" aria-checked={codeView === 'rules'} className={codeView === 'rules' ? 'on' : ''} onClick={() => setCodeView('rules')}>Rules (YAML)</button>
                    <button role="radio" aria-checked={codeView === 'logic'} className={codeView === 'logic' ? 'on' : ''} onClick={showLogic}>Decision logic (Rego, read-only)</button>
                  </div>
                  {codeView === 'logic' && <button className="btn small ghost" onClick={loadLogic}>Refresh</button>}
                </div>
                {codeView === 'rules' ? (
                  <>
                    <div className="monaco">
                      <Guard>
                        <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                          <CodeEditor value={code} onChange={v => { setCode(v); if (codeErrs) setCodeErrs(undefined) }} language="yaml" markers={codeErrs} />
                        </Suspense>
                      </Guard>
                    </div>
                    {codeErrs && (
                      <ul className="note bad ar-errs" role="alert">
                        {codeErrs.map((e, i) => <li key={i}><span className="mono">{e.line}:{e.col}</span> {e.message}</li>)}
                      </ul>
                    )}
                  </>
                ) : (
                  <>
                    <p className="subtle small">What the assurance gate decides every request with: the rules, the chain and the session are its input. Read-only here; edit the rules.</p>
                    {logic?.error && <div className="note bad">{logic.error}</div>}
                    <div className="monaco">
                      <Guard>
                        <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                          <CodeEditor value={logic?.text ?? ''} onChange={() => {}} language="rego" readOnly />
                        </Suspense>
                      </Guard>
                    </div>
                  </>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

// invalid: why the draft can't be saved, if it can't.
function invalid(d: Draft, v: AssuranceView): string | undefined {
  if (d.policy.maxAge && !DURATION.test(d.policy.maxAge)) return `default rule: "${d.policy.maxAge}" isn't a duration, like 12h`
  for (const [k, r] of Object.entries(d.rules)) {
    if (!r) continue
    if (!r.criticality) return `${k}: choose a criticality`
    if (r.assurance?.maxAge !== undefined && !DURATION.test(r.assurance.maxAge)) return `${k}: "${r.assurance.maxAge}" isn't a duration, like 12h`
  }
  for (const [k, a] of Object.entries(d.idps)) {
    for (const l of a?.levels ?? []) if (!(l.acr || l.amr)) return `${idpLabel(v.idps.find(i => i.name === k) ?? { name: k })}: each mapped value needs a value`
  }
  for (const [k, r] of Object.entries(d.points)) {
    if (r && !d.rules[r]) return `${refOf(v, k)} asks for "${r}", which is being removed`
  }
  return undefined
}
