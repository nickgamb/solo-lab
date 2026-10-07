import { useEffect, useMemo, useRef, useState } from 'react'
import { isConflict, putModels, putModelSecret, type ModelProvider, type ModelView, type ProviderIn, type RulesIn } from '../api'
import { NumField } from '../continuity/RuleBuilder'
import { clone, noFill, stable } from '../continuity/mapping'
import { endpoint, KIND_LABEL, kindLabel, seconds } from './chain'
import { EnterpriseModelControls } from './EnterpriseModelControls'

const DNS = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const HOST = /^[A-Za-z0-9]([-A-Za-z0-9.:]*[A-Za-z0-9])?$/
const EDITABLE = ['openai', 'anthropic', 'ollama']

// a connection in the working copy; other: a provider kind this editor
// doesn't write (it blocks Save rather than being dropped)
type Conn = ProviderIn & { other?: string }

const toConn = (p: ModelProvider): Conn => {
  const c: Conn = { name: p.name, kind: (EDITABLE.includes(p.kind) ? p.kind : 'openai') as Conn['kind'], model: p.model }
  if (p.host) { c.host = p.host; c.port = p.port }
  if (p.secret) c.secret = p.secret
  if (!EDITABLE.includes(p.kind)) c.other = p.kind
  return c
}
// what Save sends of a connection
const written = (c: Conn): ProviderIn => { const x = { ...c }; delete x.other; return x }
const toRules = (v: ModelView): RulesIn => ({
  on5xx: v.rules.on5xx, on429: v.rules.on429, consecutiveFailures: v.rules.consecutiveFailures || 1,
  duration: `${seconds(v.rules.duration) || 30}s`, retryAttempts: v.rules.retryAttempts,
})
const norm = (cs: Conn[], r: RulesIn) => stable({ cs, r })

// ModelRules edits the model chain declaratively: the providers in priority
// order, what takes a provider out of rotation and for how long, and whether
// a failed call is retried on the next one. Saving rewrites the backend and
// the two policies as the signed-in admin; the gateway does the rest.
export function ModelRules({ view, onSaved }: { view: ModelView; onSaved: (v: ModelView) => void }) {
  const be = view.backend!
  const [conns, setConns] = useState<Conn[]>(() => view.providers.map(toConn))
  const [rules, setRules] = useState<RulesIn>(() => toRules(view))
  const [base, setBase] = useState(() => norm(view.providers.map(toConn), toRules(view)))
  // the resourceVersion the working copy started from: Save is refused if
  // the chain changed since
  const [rv, setRv] = useState(be.resourceVersion)
  // the Secrets the saved chain refers to: a key for any other waits for Save
  const [saved, setSaved] = useState(() => new Set(view.providers.flatMap(p => (p.secret ? [p.secret] : []))))
  // new connections' API keys by Secret name, written once Save has stored
  // the reference; a ref so they never sit in rendered state
  const keys = useRef(new Map<string, string>())
  const [keysVersion, setKeysVersion] = useState(0)
  const [adding, setAdding] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const staged = useMemo(() => new Set(keys.current.keys()), [keysVersion]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = norm(conns, rules) !== base || staged.size > 0

  const stage = (f: (m: Map<string, string>) => void) => { f(keys.current); setKeysVersion(v => v + 1) }

  // start over from a view: the live one, or the one a save returned
  const adopt = (v: ModelView) => {
    const cs = v.providers.map(toConn), r = toRules(v)
    setConns(clone(cs)); setRules(r); setBase(norm(cs, r)); setRv(v.backend?.resourceVersion ?? '')
    setSaved(new Set(v.providers.flatMap(p => (p.secret ? [p.secret] : []))))
  }
  const reset = () => { adopt(view); stage(m => m.clear()); setMsg(undefined) }

  // follow the live chain unless there are local edits (an outage cut or
  // restored moves the resourceVersion too)
  const live = norm(view.providers.map(toConn), toRules(view))
  useEffect(() => {
    if (dirty) return
    if (live !== base) adopt(view)
    else setRv(be.resourceVersion)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live, be.resourceVersion])

  const set = (i: number, patch: Partial<Conn>) => setConns(cs => cs.map((c, j) => (j === i ? { ...c, ...patch } : c)))
  const move = (i: number, d: number) => setConns(cs => {
    if (i + d < 0 || i + d >= cs.length) return cs
    const t = [...cs]
    ;[t[i], t[i + d]] = [t[i + d], t[i]]
    return t
  })
  const remove = (i: number) => {
    const sec = conns[i]?.secret
    if (sec && keys.current.has(sec) && !conns.some((c, j) => j !== i && c.secret === sec)) stage(m => m.delete(sec))
    setConns(cs => cs.filter((_, j) => j !== i))
  }
  const secs = seconds(rules.duration)
  const other = conns.find(c => c.other), noModel = conns.find(c => !c.model.trim())
  const problem = other ? `${other.name} is a ${other.other} provider, which this editor doesn't write: remove it or use make llm`
    : noModel ? `${noModel.name} needs a model`
      : secs < 1 || secs > 3600 ? 'Take a failed provider out for 1 to 3600 seconds'
          : rules.consecutiveFailures > 10 ? 'At most 10 failures before a provider is taken out'
            : conns.length > 4 ? 'At most 4 model connections' : undefined

  const save = async () => {
    setBusy(true); setMsg(undefined)
    try {
      const out = await putModels(be.namespace, be.name, rv, conns.map(written), rules)
      adopt(out)
      onSaved(out)
      // the references are saved: now the staged keys (one that fails stays
      // staged, and Save tries it again)
      for (const [secret, value] of [...keys.current]) {
        if (out.providers.some(p => p.secret === secret)) await putModelSecret(be.namespace, secret, value)
        stage(m => m.delete(secret))
      }
      setMsg({ ok: true, text: 'Saved. The gateway applies it within seconds.' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message, conflict: isConflict(e) })
    } finally { setBusy(false) }
  }

  return (
    <aside className="rules scroll">
      <div className="row">
        <h3 className="grow">Failover rules</h3>
        <button className="btn small ghost" disabled={!dirty || busy} onClick={reset}>Reset</button>
        <button className="btn small primary" disabled={!dirty || busy || !!problem} onClick={save} title={problem}>Save</button>
      </div>
      <p className="subtle small">Calls go to the first connection that is in rotation. One that fails is taken out for a while, and a retried call goes to the next.</p>
      {msg && (
        <div className={`note ${msg.ok ? 'ok' : 'bad'}`}>
          {msg.text}
          {msg.conflict && <> <button className="btn small" onClick={reset} title="Discard these edits and start from the live chain">Reload</button></>}
        </div>
      )}
      {problem && dirty && <span className="small danger-text">{problem}</span>}

      <ol className="tiers">
        {conns.map((c, i) => (
          <li key={c.name} className="tier">
            <div className="row">
              <span className="order">{i + 1}</span>
              <div className="grow">
                <div className="row"><b className="ellipsis">{c.name}</b><span className="chip">{c.other ?? kindLabel(c)}</span></div>
                <div className="subtle mono small ellipsis">{endpoint(c)}</div>
              </div>
              <button className="btn ghost small" disabled={i === 0} onClick={() => move(i, -1)} title="Move up" aria-label={`Move ${c.name} up`}>↑</button>
              <button className="btn ghost small" disabled={i === conns.length - 1} onClick={() => move(i, 1)} title="Move down" aria-label={`Move ${c.name} down`}>↓</button>
            </div>
            <label className="small subtle">model
              <input className={`field${c.model.trim() ? '' : ' invalid'}`} value={c.model} aria-label={`Model of ${c.name}`} disabled={!!c.other}
                onChange={e => set(i, { model: e.target.value })} />
            </label>
            <div className="row wrap">
              <span className="grow" />
              <button className="btn ghost small danger-text" disabled={conns.length === 1} onClick={() => remove(i)}
                title={conns.length === 1 ? 'The chain needs a connection' : undefined}>Remove</button>
            </div>
            {c.secret && (staged.has(c.secret) ? <span className="subtle small">API key written to {c.secret} on save</span>
              : <KeyField ns={be.namespace} conn={c} stage={saved.has(c.secret) ? undefined : v => stage(m => m.set(c.secret!, v))} />)}
          </li>
        ))}
      </ol>

      {adding ? <AddConnection existing={conns.map(c => c.name)} onCancel={() => setAdding(false)}
        onAdd={(c, key) => {
          if (key && c.secret) stage(m => m.set(c.secret!, key))
          setConns(cs => [...cs, c])
          setAdding(false)
        }} />
        : <button className="btn small" disabled={conns.length >= 4} onClick={() => setAdding(true)}>+ Add model connection</button>}

      <div className="label" style={{ marginTop: 18 }}>Fail over when</div>
      <span className="small">a provider answers 5xx, or doesn't answer at all. A rate-limited call (429) is retried, but doesn't take the provider out.</span>
      {view.rules.custom && <span className="subtle small">The live condition (<span className="mono">{view.rules.condition}</span>) says more than this; saving replaces it.</span>}
      <div className="grid2">
        <NumField label="take a failed provider out for (s)" value={secs || 30} onChange={n => setRules(r => ({ ...r, duration: `${n}s` }))} />
        <NumField label="after N failures" value={rules.consecutiveFailures} onChange={n => setRules(r => ({ ...r, consecutiveFailures: n }))} />
      </div>
      <label className="tog" style={{ marginTop: 6 }} title="On: a call that fails (or is rate limited) is tried once more, on the next connection in rotation">
        <input type="checkbox" checked={rules.retryAttempts > 0}
          onChange={e => setRules(r => ({ ...r, retryAttempts: e.target.checked ? Math.max(view.rules.retryAttempts, 1) : 0 }))} />Retry the call
      </label>

      <p className="subtle small" style={{ marginTop: 12 }}>
        <span className="mono">make llm</span> rewrites this chain from .env{view.declared?.provider ? ` (last: ${view.declared.provider}${view.declared.model ? `/${view.declared.model}` : ''}${view.declared.fallback ? `, then ${view.declared.fallback}` : ''})` : ''}.
        Rules: {be.policies.join(', ') || 'no failover policy found'}.
      </p>
      <EnterpriseModelControls view={view} />
    </aside>
  )
}

// KeyField writes a connection's API key, or, for a Secret the saved chain
// doesn't refer to yet, stages it for Save. Write-only: the value is sent
// once and never read back.
function KeyField({ ns, conn, stage }: { ns: string; conn: Conn; stage?: (value: string) => void }) {
  const [v, setV] = useState('')
  const [state, setState] = useState<string>()
  const put = async () => {
    if (stage) { stage(v); setV(''); return }
    setState('saving')
    try {
      await putModelSecret(ns, conn.secret!, v)
      setV(''); setState('saved')
    } catch (e) { setState((e as Error).message) }
  }
  return (
    <div className="secret">
      <span className="subtle small mono">API key in Secret {conn.secret}</span>
      <div className="row">
        <input className="field" type="password" {...noFill} placeholder="set API key" aria-label={`API key of ${conn.name}`}
          value={v} onChange={e => setV(e.target.value)} />
        <button className="btn small" disabled={!v} onClick={put}>Set</button>
      </div>
      {state && <span className="subtle small">{state}</span>}
    </div>
  )
}

type AddKind = 'ollama' | 'openai' | 'anthropic'

// AddConnection adds a model connection to the working copy. A cloud
// provider's API key is staged and written once Save has stored the
// connection; left empty, the Secret is used as it is.
function AddConnection({ existing, onAdd, onCancel }: { existing: string[]; onAdd: (c: Conn, key: string) => void; onCancel: () => void }) {
  const [kind, setKind] = useState<AddKind>('ollama')
  const free = (k: string) => { let n = k, i = 2; while (existing.includes(n)) n = `${k}-${i++}`; return n }
  const [name, setName] = useState(() => free('ollama'))
  const [host, setHost] = useState('')
  const [port, setPort] = useState('11434')
  const [model, setModel] = useState('')
  const [secret, setSecret] = useState(() => `model-${free('ollama')}`)
  const [key, setKey] = useState('')
  const pick = (k: AddKind) => { setKind(k); const n = free(k); setName(n); setSecret(`model-${n}`) }
  const p = Number(port)
  const valid = DNS.test(name) && name.length <= 40 && !existing.includes(name) && model.trim() !== ''
    && (kind === 'ollama' ? HOST.test(host) && Number.isInteger(p) && p >= 1 && p <= 65535 : DNS.test(secret))
  const add = () => onAdd(kind === 'ollama' ? { name, kind, model: model.trim(), host, port: p }
    : { name, kind, model: model.trim(), secret }, kind === 'ollama' ? '' : key)
  return (
    <div className="add">
      <div className="label">New model connection</div>
      <div className="seg small" role="group" aria-label="Provider">
        {(['ollama', 'openai', 'anthropic'] as const).map(k => (
          <button key={k} className={kind === k ? 'on' : ''} aria-pressed={kind === k} onClick={() => pick(k)}>
            {k === 'ollama' ? 'Ollama / OpenAI-compatible' : KIND_LABEL[k]}</button>
        ))}
      </div>
      <input className="field" placeholder="name" aria-label="Name" value={name} onChange={e => { setName(e.target.value.toLowerCase()); setSecret(`model-${e.target.value.toLowerCase()}`) }} />
      {kind === 'ollama' && (
        <div className="row">
          <input className="field grow" placeholder="host (e.g. host.docker.internal)" aria-label="Host" value={host} onChange={e => setHost(e.target.value.trim())} />
          <input className="field num" type="number" min={1} max={65535} placeholder="port" aria-label="Port" value={port} onChange={e => setPort(e.target.value)} />
        </div>
      )}
      <input className="field" placeholder={kind === 'anthropic' ? 'model (e.g. claude-sonnet-5)' : kind === 'openai' ? 'model (e.g. gpt-5-mini)' : 'model (e.g. qwen3.8:27b)'}
        aria-label="Model" value={model} onChange={e => setModel(e.target.value)} />
      {kind !== 'ollama' && <>
        <input className="field" placeholder="Secret for the key" aria-label="Secret name" value={secret} onChange={e => setSecret(e.target.value.toLowerCase())} />
        <input className="field" type="password" {...noFill} placeholder="API key (stored in that Secret on save; empty: use it as it is)" aria-label="API key" value={key} onChange={e => setKey(e.target.value)} />
      </>}
      <div className="row"><span className="grow" /><button className="btn small ghost" onClick={onCancel}>Cancel</button><button className="btn small primary" disabled={!valid} onClick={add}>Add connection</button></div>
    </div>
  )
}
