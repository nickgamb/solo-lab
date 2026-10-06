import { useEffect, useState } from 'react'
import { api, type ContinuitySpec, type IdentityContinuity, type Tier } from '../api'
import { DirectorySync } from './DirectorySync'
import { syncSummary } from './mapping'

const clone = <T,>(v: T): T => JSON.parse(JSON.stringify(v))
const DNS = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

// RuleBuilder edits the failover chain declaratively: IdP order, what
// counts as a failure, draining, health thresholds and failback. Saving
// writes the IdentityContinuity spec as the signed-in admin; the continuity
// controller does the rest.
export function RuleBuilder({ ic, broker }: { ic: IdentityContinuity; broker: string }) {
  const [spec, setSpec] = useState<ContinuitySpec>(() => clone(ic.spec))
  const [base, setBase] = useState(() => JSON.stringify(ic.spec))
  const [adding, setAdding] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const [busy, setBusy] = useState(false)
  const [mapping, setMapping] = useState(false)
  const dirty = JSON.stringify(spec) !== base

  // follow the live object unless there are local edits
  useEffect(() => {
    const live = JSON.stringify(ic.spec)
    if (!dirty && live !== base) { setSpec(clone(ic.spec)); setBase(live) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ic.spec])

  const tiers = spec.tiers ?? []
  // the IdPs, in failover order; the broker's break-glass accounts stay last
  // in the spec, untouched
  const idps = tiers.map((t, i) => ({ t, i })).filter(x => x.t.type === 'oidc')
  const set = (i: number, patch: Partial<Tier>) => setSpec(s => ({ ...s, tiers: s.tiers.map((t, j) => (j === i ? { ...t, ...patch } : t)) }))
  const setWhen = (i: number, patch: Partial<NonNullable<Tier['failoverWhen']>>) => set(i, { failoverWhen: { ...tiers[i].failoverWhen, ...patch } })
  const move = (k: number, d: number) => setSpec(s => {
    const a = idps[k]?.i, b = idps[k + d]?.i
    if (a === undefined || b === undefined) return s
    const t = [...s.tiers]
    ;[t[a], t[b]] = [t[b], t[a]]
    return { ...s, tiers: t }
  })
  const remove = (i: number) => setSpec(s => ({ ...s, tiers: s.tiers.filter((_, j) => j !== i) }))
  const health = spec.health ?? {}

  const save = async () => {
    setBusy(true); setMsg(undefined)
    try {
      await api(`/api/continuity/${ic.metadata.namespace}/${ic.metadata.name}`, { method: 'PUT', body: JSON.stringify(spec) })
      setBase(JSON.stringify(spec))
      setMsg({ ok: true, text: 'Saved. The controller applies it on its next probe.' })
    } catch (e) { setMsg({ ok: false, text: (e as Error).message }) } finally { setBusy(false) }
  }

  return (
    <aside className="rules scroll">
      <div className="row">
        <h3 className="grow">Continuity rules</h3>
        <button className="btn small ghost" disabled={!dirty || busy} onClick={() => { setSpec(clone(ic.spec)); setMsg(undefined) }}>Reset</button>
        <button className="btn small primary" disabled={!dirty || busy} onClick={save}>Save</button>
      </div>
      <p className="subtle small">Sign-in goes to the first IdP that is enabled, not drained and healthy. Everything downstream keeps trusting {broker}, whichever IdP authenticated the person.</p>
      {msg && <div className={`note ${msg.ok ? 'ok' : 'bad'}`}>{msg.text}</div>}

      <ol className="tiers">
        {idps.map(({ t, i }, k) => (
          <li key={t.name} className={t.enabled === false || t.drain ? 'tier off' : 'tier'}>
            <div className="row">
              <span className="order">{k + 1}</span>
              <div className="grow">
                <input className="field bare" value={t.displayName ?? ''} placeholder={t.name} onChange={e => set(i, { displayName: e.target.value })} />
                <div className="subtle mono small ellipsis">{t.oidc?.issuer}</div>
              </div>
              <button className="btn ghost small" disabled={k === 0} onClick={() => move(k, -1)} title="Move up">↑</button>
              <button className="btn ghost small" disabled={k === idps.length - 1} onClick={() => move(k, 1)} title="Move down">↓</button>
            </div>
            <div className="row wrap">
              <label className="tog"><input type="checkbox" checked={t.enabled !== false} onChange={e => set(i, { enabled: e.target.checked })} />enabled</label>
              <label className="tog"><input type="checkbox" checked={!!t.drain} onChange={e => set(i, { drain: e.target.checked })} />drain</label>
              <button className="btn ghost small danger-text" onClick={() => remove(i)}>Remove</button>
            </div>
            {(
              <div className="when">
                <span className="label">Fail over when</span>
                <label className="tog"><input type="checkbox" checked={t.failoverWhen?.unreachable ?? true} onChange={e => setWhen(i, { unreachable: e.target.checked })} />unreachable</label>
                <label className="tog"><input type="checkbox" checked={t.failoverWhen?.serverError ?? true} onChange={e => setWhen(i, { serverError: e.target.checked })} />5xx</label>
                <label className="tog"><input type="checkbox" checked={t.failoverWhen?.invalidDiscovery ?? true} onChange={e => setWhen(i, { invalidDiscovery: e.target.checked })} />bad discovery / JWKS</label>
                <label className="tog" title={`must be below the ${(health.timeoutSeconds ?? 2) * 1000}ms probe timeout: a slower answer times out first`}>latency &gt;
                  <input className={`field num${(t.failoverWhen?.latencyAboveMs ?? 0) >= (health.timeoutSeconds ?? 2) * 1000 ? ' invalid' : ''}`} type="number" min={1}
                    max={(health.timeoutSeconds ?? 2) * 1000 - 1} step={100} value={t.failoverWhen?.latencyAboveMs ?? ''} placeholder="off"
                    onChange={e => setWhen(i, { latencyAboveMs: e.target.value === '' ? undefined : Number(e.target.value) })} />ms</label>
                {(t.failoverWhen?.latencyAboveMs ?? 0) >= (health.timeoutSeconds ?? 2) * 1000 &&
                  <span className="small danger-text">below {(health.timeoutSeconds ?? 2) * 1000}ms (the probe timeout), or it never fires</span>}
                <SecretField ns={ic.metadata.namespace} tier={t} />
              </div>
            )}
          </li>
        ))}
      </ol>

      {adding ? <AddTier ns={ic.metadata.namespace} brokerIssuer={ic.status?.broker?.issuer} existing={tiers.map(t => t.name)} onCancel={() => setAdding(false)}
        onAdd={t => { setSpec(s => { const at = s.tiers.findIndex(x => x.type !== 'oidc'); const ts = [...s.tiers]; ts.splice(at < 0 ? ts.length : at, 0, t); return { ...s, tiers: ts } }); setAdding(false) }} />
        : <button className="btn small" onClick={() => setAdding(true)}>+ Add OIDC IdP</button>}

      {/* the window saves the live spec, so unsaved rule edits would be lost behind it */}
      <div className="cm-open">
        <button className="btn small" disabled={dirty} onClick={() => setMapping(true)}
          title={dirty ? 'Save or reset the rule changes first' : "Map the IdPs' profile attributes and schedule the sync from the primary to the failovers"}>Directory sync</button>
        <span className="subtle small">{syncSummary(ic.spec)}</span>
      </div>
      {mapping && <DirectorySync ic={ic} onClose={() => setMapping(false)} />}

      <div className="label" style={{ marginTop: 18 }}>Health checks</div>
      <div className="grid2">
        <label>interval (s)<input className="field" type="number" min={1} value={health.intervalSeconds ?? 5} onChange={e => setSpec(s => ({ ...s, health: { ...s.health, intervalSeconds: Number(e.target.value) } }))} /></label>
        <label>timeout (s)<input className="field" type="number" min={1} value={health.timeoutSeconds ?? 2} onChange={e => setSpec(s => ({ ...s, health: { ...s.health, timeoutSeconds: Number(e.target.value) } }))} /></label>
        <label>unhealthy after<input className="field" type="number" min={1} value={health.unhealthyThreshold ?? 2} onChange={e => setSpec(s => ({ ...s, health: { ...s.health, unhealthyThreshold: Number(e.target.value) } }))} /></label>
        <label>healthy after<input className="field" type="number" min={1} value={health.healthyThreshold ?? 3} onChange={e => setSpec(s => ({ ...s, health: { ...s.health, healthyThreshold: Number(e.target.value) } }))} /></label>
      </div>
      <div className="label" style={{ marginTop: 14 }}>Failback</div>
      <div className="seg">
        {(['Automatic', 'Manual'] as const).map(f => (
          <button key={f} className={(spec.failback ?? 'Automatic') === f ? 'on' : ''} onClick={() => setSpec(s => ({ ...s, failback: f }))}>{f}</button>
        ))}
      </div>
      <p className="subtle small">Automatic moves back up the chain once a higher IdP passes its healthy threshold. Manual stays put until you drain the active IdP or reorder.</p>
    </aside>
  )
}

// SecretField writes a tier's client secret. It's write-only: the value is
// sent once and never read back.
function SecretField({ ns, tier }: { ns: string; tier: Tier }) {
  const [v, setV] = useState('')
  const [state, setState] = useState<string>()
  const ref = tier.oidc?.clientSecretRef
  if (!ref) return null
  const put = async () => {
    setState('saving')
    try {
      await api(`/api/continuity/${ns}/secret`, { method: 'PUT', body: JSON.stringify({ name: ref.name, key: ref.key ?? 'client-secret', value: v }) })
      setV(''); setState('saved')
    } catch (e) { setState((e as Error).message) }
  }
  return (
    <div className="secret">
      <span className="subtle small mono">client {tier.oidc?.clientID || '(no client id)'} · secret {ref.name}</span>
      <div className="row">
        <input className="field" type="password" autoComplete="off" placeholder="set client secret" value={v} onChange={e => setV(e.target.value)} />
        <button className="btn small" disabled={!v} onClick={put}>Set</button>
      </div>
      {state && <span className="subtle small">{state}</span>}
    </div>
  )
}

function AddTier({ ns, brokerIssuer, existing, onAdd, onCancel }: { ns: string; brokerIssuer?: string; existing: string[]; onAdd: (t: Tier) => void; onCancel: () => void }) {
  const [name, setName] = useState('')
  const [displayName, setDisplay] = useState('')
  const [issuer, setIssuer] = useState('')
  const [clientID, setClient] = useState('')
  const [secret, setSecret] = useState('')
  const [err, setErr] = useState<string>()
  const valid = DNS.test(name) && !existing.includes(name) && /^https:\/\//.test(issuer) && clientID !== ''
  const add = async () => {
    setErr(undefined)
    const secretName = `upstream-${name}`
    if (secret) {
      try { await api(`/api/continuity/${ns}/secret`, { method: 'PUT', body: JSON.stringify({ name: secretName, key: 'client-secret', value: secret }) }) }
      catch (e) { setErr((e as Error).message); return }
    }
    onAdd({ name, displayName: displayName || undefined, type: 'oidc', enabled: true,
      oidc: { issuer, clientID, clientSecretRef: { name: secretName, key: 'client-secret' } },
      failoverWhen: { unreachable: true, serverError: true, invalidDiscovery: true } })
  }
  return (
    <div className="add">
      <div className="label">New OIDC IdP</div>
      <input className="field" placeholder="name (e.g. ping)" value={name} onChange={e => setName(e.target.value.toLowerCase())} />
      <input className="field" placeholder="display name" value={displayName} onChange={e => setDisplay(e.target.value)} />
      <input className="field" placeholder="issuer (https://…)" value={issuer} onChange={e => setIssuer(e.target.value.trim())} />
      <input className="field" placeholder="client id" value={clientID} onChange={e => setClient(e.target.value.trim())} />
      <input className="field" type="password" autoComplete="off" placeholder="client secret (stored as a Secret)" value={secret} onChange={e => setSecret(e.target.value)} />
      <p className="subtle small">Register this redirect URI with the IdP: <span className="mono">{brokerIssuer ?? '<broker issuer>'}/broker/{name || '<name>'}/endpoint</span></p>
      {err && <div className="note bad">{err}</div>}
      <div className="row"><span className="grow" /><button className="btn small ghost" onClick={onCancel}>Cancel</button><button className="btn small primary" disabled={!valid} onClick={add}>Add IdP</button></div>
    </div>
  )
}
