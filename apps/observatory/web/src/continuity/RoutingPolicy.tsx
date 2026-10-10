import { lazy, Suspense, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { getResource, isConflict, putContinuity, type IdentityContinuity } from '../api'
import { Guard } from '../topology/DetailsPanel'
import { fromCode, sameRules, toCode, type CodeError, type RoutingRule } from './routingCode'

const CodeEditor = lazy(() => import('./CodeEditor'))

type Tab = 'rules' | 'policy'
const TABS: [Tab, string][] = [['rules', 'Rules'], ['policy', 'Gateway policy']]

// RoutingPolicy: which of the firm's IdPs each sign-in goes to, decided at
// the gateway. Rules: each rule's CEL as written, its IdPs in order of
// preference, saved to the chain (spec.routing.rules) as the signed-in
// admin. Gateway policy: what the gateway runs now, the policy the
// continuity controller writes from those rules and the chain's health,
// read-only (the controller rewrites it on every failover).
export function RoutingPolicy({ ic, onSaved, onClose }: { ic: IdentityContinuity; onSaved: (ic: IdentityContinuity) => void; onClose: () => void }) {
  const ns = ic.metadata.namespace, chain = ic.metadata.name
  const routing = ic.spec.routing
  const known = ic.spec.tiers.filter(t => t.type === 'oidc').map(t => t.name)
  const [rules, setRules] = useState<RoutingRule[]>(routing?.rules ?? [])
  const [code, setCode] = useState(() => toCode(routing?.rules ?? []))
  const [errs, setErrs] = useState<CodeError[]>()
  const [tab, setTab] = useState<Tab>('rules')
  const [policy, setPolicy] = useState<{ yaml?: string; error?: string }>({})
  const [msg, setMsg] = useState<{ ok: boolean; text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const [rv, setRv] = useState(ic.metadata.resourceVersion)
  const dialog = useRef<HTMLDivElement>(null)

  const parsed = fromCode(code, known)
  const dirty = !parsed.rules || !sameRules(parsed.rules, rules)

  // the live policy, read again on Refresh and after a save
  const ref = routing?.policy
  const policyKey = ref ? [ref.apiVersion, ref.kind, ref.namespace, ref.name].join('|') : ''
  const [reads, setReads] = useState(0)
  useEffect(() => {
    if (!policyKey) return
    const [apiVersion, kind, namespace, name] = policyKey.split('|')
    let live = true
    getResource(new URLSearchParams({ apiVersion, kind, namespace, name }))
      .then(r => { if (live) setPolicy({ yaml: r.yaml }) }, e => { if (live) setPolicy({ error: (e as Error).message }) })
    return () => { live = false }
  }, [policyKey, reads])
  const loadPolicy = () => setReads(n => n + 1)

  const reset = () => { setCode(toCode(rules)); setErrs(undefined); setMsg(undefined) }
  const save = async () => {
    if (!routing) return
    if (!parsed.rules) { setErrs(parsed.errors); return }
    setBusy(true); setMsg(undefined)
    try {
      const saved = await putContinuity(ns, chain, rv, { ...ic.spec, routing: { ...routing, rules: parsed.rules } })
      setRv(saved.metadata.resourceVersion)
      setRules(parsed.rules); setCode(toCode(parsed.rules)); setErrs(undefined)
      setMsg({ ok: true, text: 'Saved. The controller writes the gateway policy on its next pass (a few seconds).' })
      onSaved(saved)
      setTimeout(loadPolicy, 6000)
    } catch (e) {
      setMsg(isConflict(e) ? { ok: false, text: 'The chain changed since this opened. Reload to edit the current rules.', conflict: true }
        : { ok: false, text: (e as Error).message })
    } finally { setBusy(false) }
  }

  const close = () => {
    if (dirty && !window.confirm('Discard the unsaved routing rules?')) return
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
  const onTabKey = (e: ReactKeyboardEvent) => {
    const i = TABS.findIndex(([t]) => t === tab)
    const j = e.key === 'ArrowRight' ? (i + 1) % TABS.length : e.key === 'ArrowLeft' ? (i + TABS.length - 1) % TABS.length : -1
    if (j < 0) return
    e.preventDefault()
    setTab(TABS[j][0])
    document.getElementById(`rp-tab-${TABS[j][0]}`)?.focus()
  }

  const now = new Map((ic.status?.routing ?? []).map(r => [r.name, r]))
  const active = ic.status?.active

  return createPortal(
    <div className="cm-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <div className="cm-window" role="dialog" aria-modal="true" aria-labelledby="rp-title" tabIndex={-1} ref={dialog}>
        <header className="cm-header">
          <h2 id="rp-title">Routing policy</h2>
          <span className="mono subtle">{ns}/{chain}</span>
          <div className="tabs" role="tablist" aria-label="Routing policy views" onKeyDown={onTabKey}>
            {TABS.map(([t, label]) => (
              <button key={t} id={`rp-tab-${t}`} role="tab" aria-selected={tab === t} tabIndex={tab === t ? 0 : -1}
                className={tab === t ? 'tab active' : 'tab'} onClick={() => setTab(t)}>{label}</button>
            ))}
          </div>
          <span className="grow" />
          {routing && <span className="subtle small mono" title="the gateway policy these rules are written into">{routing.policy.kind} {routing.policy.namespace}/{routing.policy.name}</span>}
          {dirty && <span className="chip warn">unsaved</span>}
          <button className="btn small ghost" disabled={!dirty || busy} onClick={reset}>Reset</button>
          <button className="btn small primary" disabled={!dirty || busy || !routing} onClick={save}>Save</button>
          <button className="btn small" onClick={close} title="Close (Esc)">Close</button>
        </header>
        {msg && (
          <div className={`note ${msg.ok ? 'ok' : 'bad'} cm-msg`}>
            {msg.text}
            {msg.conflict && <> <button className="btn small" onClick={onClose}>Close and reload</button></>}
          </div>
        )}
        {!routing && <div className="note bad cm-msg">This chain has no routing policy (spec.routing): every sign-in goes to the active IdP.</div>}
        <div className="cm-body">
          {tab === 'rules' && (
            <div className="cm-code">
              <p className="subtle small">
                Every sign-in to the broker passes the gateway, which sends it to the IdP of the first rule it matches: the first IdP
                in that rule that can sign people in now. Anything else goes to the active IdP{active ? ` (${active})` : ''}. Each
                rule's CEL sees the sign-in request: <span className="mono">request.uri</span> (its <span className="mono">client_id</span>,
                the <span className="mono">resource</span> it is for, a <span className="mono">login_hint</span>), <span className="mono">request.headers</span>.
              </p>
              <div className="monaco">
                <Guard>
                  <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                    <CodeEditor value={code} onChange={v => { setCode(v); if (errs) setErrs(undefined) }} language="fabric-routing"
                      markers={errs ?? parsed.errors} />
                  </Suspense>
                </Guard>
              </div>
              {!!parsed.errors.length && (
                <ul className={`note ${parsed.rules ? 'warn' : 'bad'} ar-errs`} role="alert">
                  {parsed.errors.map((e, i) => <li key={i}><span className="mono">{e.line}:{e.col}</span> {e.message}</li>)}
                </ul>
              )}
              <div className="rp-now">
                <div className="label">Now</div>
                {(routing?.rules ?? []).map(r => {
                  const s = now.get(r.name)
                  return (
                    <div key={r.name} className="row line">
                      <span className="chip">{r.name}</span>→<span className={`chip ${s?.idp ? 'accent' : ''}`}>{s?.idp || active || '—'}</span>
                      <span className="subtle small ellipsis grow">{s?.reason ?? 'not applied yet'}</span>
                    </div>
                  )
                })}
                <div className="row line"><span className="chip">everything else</span>→<span className="chip accent">{active ?? '—'}</span>
                  <span className="subtle small">the active IdP</span></div>
              </div>
            </div>
          )}
          {tab === 'policy' && (
            <div className="cm-code">
              <p className="subtle small">
                What the gateway runs on every sign-in now: the continuity controller writes the <span className="mono">:path</span> transformation
                from the rules and the chain's health, and rewrites it on every failover. Read-only here; edit the rules.
                {' '}<button className="btn small ghost" onClick={loadPolicy}>Refresh</button>
              </p>
              {policy.error && <div className="note bad">{policy.error}</div>}
              <div className="monaco">
                <Guard>
                  <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                    <CodeEditor value={policy.yaml ?? ''} onChange={() => {}} language="yaml" readOnly />
                  </Suspense>
                </Guard>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>,
    document.body,
  )
}
