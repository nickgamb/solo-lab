import { useEffect, useMemo, useRef, useState } from 'react'
import { isConflict, putContinuity, putSecret, type ContinuitySpec, type IdentityContinuity, type Tier, type TierStatus } from '../api'
import { AssuranceRules } from './AssuranceRules'
import { enforced } from './rulesCode'
import { DirectorySync } from './DirectorySync'
import { clone, noFill, stable, syncSummary } from './mapping'

const DNS = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const SECRET_KEY = 'client-secret'

// The spec as compared for edits: key order, and enabled and drain at their
// defaults, never read as a change.
function norm(s: ContinuitySpec): string {
  return stable({ ...s, tiers: (s.tiers ?? []).map(t => {
    const x = { ...t }
    if (x.enabled === true) delete x.enabled
    if (!x.drain) delete x.drain
    return x
  }) })
}

// RuleBuilder edits the failover chain declaratively: IdP order, what
// counts as a failure, draining, health thresholds and failback. Saving
// writes the IdentityContinuity spec as the signed-in admin; the continuity
// controller does the rest.
export function RuleBuilder({ ic, broker, profiles }: { ic: IdentityContinuity; broker: string; profiles: { name: string; phase?: string; mode?: string }[] }) {
  const ns = ic.metadata.namespace
  const [spec, setSpec] = useState<ContinuitySpec>(() => clone(ic.spec))
  const [base, setBase] = useState(() => norm(ic.spec))
  // the resourceVersion the working copy started from: Save is refused if
  // the object changed since
  const [rv, setRv] = useState(ic.metadata.resourceVersion)
  // the IdPs the cluster has: a secret for any other waits for Save
  const [saved, setSaved] = useState(() => new Set((ic.spec.tiers ?? []).map(t => t.name)))
  // new IdPs' client secrets, written once their IdP is saved; a ref so
  // they never sit in rendered state
  const secrets = useRef(new Map<string, string>())
  const [secretsVersion, setSecretsVersion] = useState(0)
  const [adding, setAdding] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const [mapping, setMapping] = useState(false)
  const [rules, setRules] = useState(false)
  const staged = useMemo(() => new Set(secrets.current.keys()), [secretsVersion]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = norm(spec) !== base || staged.size > 0

  const stage = (f: (m: Map<string, string>) => void) => { f(secrets.current); setSecretsVersion(v => v + 1) }

  // start over from an object: the live one, or the one a save stored
  const adopt = (o: IdentityContinuity) => {
    setSpec(clone(o.spec)); setBase(norm(o.spec)); setRv(o.metadata.resourceVersion); setSaved(new Set((o.spec.tiers ?? []).map(t => t.name)))
  }
  const reset = () => { adopt(ic); stage(m => m.clear()); setMsg(undefined) }

  // follow the live object unless there are local edits
  useEffect(() => {
    if (dirty) return
    if (norm(ic.spec) !== base) adopt(ic)
    else setRv(ic.metadata.resourceVersion)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ic.spec, ic.metadata.resourceVersion])

  const tiers = spec.tiers ?? []
  // the IdPs, in failover order; the broker's break-glass accounts stay last
  // in the spec, untouched
  const idps = tiers.map((t, i) => ({ t, i })).filter(x => x.t.type === 'oidc')
  const set = (i: number, patch: Partial<Tier>) => setSpec(s => ({ ...s, tiers: s.tiers.map((t, j) => (j === i ? { ...t, ...patch } : t)) }))
  // enabled and drain are left out at their defaults
  const toggle = (i: number, key: 'enabled' | 'drain', on: boolean) => setSpec(s => ({ ...s, tiers: s.tiers.map((t, j) => {
    if (j !== i) return t
    const x: Tier = { ...t }
    if (on === (key === 'enabled')) delete x[key]
    else x[key] = on
    return x
  }) }))
  const setWhen = (i: number, patch: Partial<NonNullable<Tier['failoverWhen']>>) => set(i, { failoverWhen: { ...tiers[i].failoverWhen, ...patch } })
  const move = (k: number, d: number) => setSpec(s => {
    const a = idps[k]?.i, b = idps[k + d]?.i
    if (a === undefined || b === undefined) return s
    const t = [...s.tiers]
    ;[t[a], t[b]] = [t[b], t[a]]
    return { ...s, tiers: t }
  })
  const remove = (i: number) => {
    const name = tiers[i]?.name
    if (name && secrets.current.has(name)) stage(m => m.delete(name))
    setSpec(s => ({ ...s, tiers: s.tiers.filter((_, j) => j !== i) }))
  }
  const health = spec.health ?? {}
  const setHealth = (patch: NonNullable<ContinuitySpec['health']>) => setSpec(s => ({ ...s, health: { ...s.health, ...patch } }))
  const timeoutMs = (health.timeoutSeconds ?? 2) * 1000
  const slow = (t: Tier) => (t.failoverWhen?.latencyAboveMs ?? 0) >= timeoutMs
  const invalid = idps.some(x => slow(x.t))

  const save = async () => {
    setBusy(true); setMsg(undefined)
    try {
      const out = await putContinuity(ns, ic.metadata.name, rv, spec)
      adopt(out)
      // the IdPs are saved: now their staged client secrets (one that fails
      // stays staged, and Save tries it again)
      for (const [tier, value] of [...secrets.current]) {
        const ref = out.spec.tiers?.find(t => t.name === tier)?.oidc?.clientSecretRef
        if (ref) await putSecret(ns, ref.name, { key: ref.key ?? SECRET_KEY, value, for: 'tier' })
        stage(m => m.delete(tier))
      }
      setMsg({ ok: true, text: 'Saved. The controller applies it on its next probe.' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message, conflict: isConflict(e) })
    } finally { setBusy(false) }
  }

  return (
    <aside className="rules scroll">
      <div className="row">
        <h3 className="grow">Continuity rules</h3>
        <button className="btn small ghost" disabled={!dirty || busy} onClick={reset}>Reset</button>
        <button className="btn small primary" disabled={!dirty || busy || invalid} onClick={save}
          title={invalid ? `Fix the latency rule first: below ${timeoutMs}ms (the probe timeout), or it never fires` : undefined}>Save</button>
      </div>
      <p className="subtle small">Sign-in goes to the first IdP that is enabled, not drained and healthy. Everything downstream keeps trusting {broker}, whichever IdP authenticated the person.</p>
      {msg && (
        <div className={`note ${msg.ok ? 'ok' : 'bad'}`}>
          {msg.text}
          {msg.conflict && <> <button className="btn small" onClick={reset} title="Discard these edits and start from the live object">Reload</button></>}
        </div>
      )}

      <ol className="tiers">
        {idps.map(({ t, i }, k) => (
          <li key={t.name} className={t.enabled === false || t.drain ? 'tier off' : 'tier'}>
            <div className="row">
              <span className="order">{k + 1}</span>
              <div className="grow">
                <input className="field bare" value={t.displayName ?? ''} placeholder={t.name} aria-label={`Display name of ${t.name}`}
                  onChange={e => set(i, { displayName: e.target.value || undefined })} />
                <div className="subtle mono small ellipsis">{t.oidc?.issuer}</div>
                <Trust status={ic.status?.tiers?.find(x => x.name === t.name)} />
              </div>
              <button className="btn ghost small" disabled={k === 0} onClick={() => move(k, -1)} title="Move up" aria-label={`Move ${t.name} up`}>↑</button>
              <button className="btn ghost small" disabled={k === idps.length - 1} onClick={() => move(k, 1)} title="Move down" aria-label={`Move ${t.name} down`}>↓</button>
            </div>
            <div className="row wrap">
              <label className="tog"><input type="checkbox" checked={t.enabled !== false} onChange={e => toggle(i, 'enabled', e.target.checked)} />enabled</label>
              <label className="tog"><input type="checkbox" checked={!!t.drain} onChange={e => toggle(i, 'drain', e.target.checked)} />drain</label>
              <button className="btn ghost small danger-text" onClick={() => remove(i)}>Remove</button>
            </div>
            <div className="when">
              <span className="label">Fail over when</span>
              <label className="tog"><input type="checkbox" checked={t.failoverWhen?.unreachable ?? true} onChange={e => setWhen(i, { unreachable: e.target.checked })} />unreachable</label>
              <label className="tog"><input type="checkbox" checked={t.failoverWhen?.serverError ?? true} onChange={e => setWhen(i, { serverError: e.target.checked })} />5xx</label>
              <label className="tog"><input type="checkbox" checked={t.failoverWhen?.invalidDiscovery ?? true} onChange={e => setWhen(i, { invalidDiscovery: e.target.checked })} />bad discovery / JWKS</label>
              <label className="tog" title={`must be below the ${timeoutMs}ms probe timeout: a slower answer times out first`}>latency &gt;
                <input className={`field num${slow(t) ? ' invalid' : ''}`} type="number" min={1}
                  max={timeoutMs - 1} step={100} value={t.failoverWhen?.latencyAboveMs ?? ''} placeholder="off"
                  onChange={e => {
                    const n = Number(e.target.value)
                    if (e.target.value === '') setWhen(i, { latencyAboveMs: undefined })
                    else if (Number.isInteger(n) && n >= 1) setWhen(i, { latencyAboveMs: n })
                  }} />ms</label>
              {slow(t) && <span className="small danger-text">below {timeoutMs}ms (the probe timeout), or it never fires</span>}
              {staged.has(t.name) ? <span className="subtle small">client secret written on save</span>
                : <SecretField ns={ns} tier={t} stage={saved.has(t.name) ? undefined : v => stage(m => m.set(t.name, v))} />}
            </div>
          </li>
        ))}
      </ol>

      {adding ? <AddTier brokerIssuer={ic.status?.broker?.issuer} existing={tiers.map(t => t.name)} onCancel={() => setAdding(false)}
        onAdd={(t, secret) => {
          if (secret) stage(m => m.set(t.name, secret))
          setSpec(s => { const at = s.tiers.findIndex(x => x.type !== 'oidc'); const ts = [...s.tiers]; ts.splice(at < 0 ? ts.length : at, 0, t); return { ...s, tiers: ts } })
          setAdding(false)
        }} />
        : <button className="btn small" onClick={() => setAdding(true)}>+ Add OIDC IdP</button>}

      {/* the window saves the live spec, so unsaved rule edits would be lost behind it */}
      <div className="cm-open">
        <button className="btn small" disabled={dirty} onClick={() => setMapping(true)}
          title={dirty ? 'Save or reset the rule changes first' : "Map the IdPs' profile attributes and schedule the sync from the primary to the failovers"}>Directory sync</button>
        <span className="subtle small">{syncSummary(ic.spec)}</span>
      </div>
      {mapping && <DirectorySync ic={ic} onSaved={adopt} onClose={() => setMapping(false)} />}
      <div className="cm-open">
        <button className="btn small" disabled={dirty} onClick={() => setRules(true)}
          title={dirty ? 'Save or reset the rule changes first' : 'What a sign-in must prove to reach each resource, whichever IdP it came through'}>Assurance rules</button>
        <span className="subtle small">{assuranceSummary(ic, profiles)}</span>
      </div>
      {rules && <AssuranceRules ic={ic} onClose={() => setRules(false)} />}

      <div className="label" style={{ marginTop: 18 }}>Health checks</div>
      <div className="grid2">
        <NumField label="interval (s)" value={health.intervalSeconds ?? 5} onChange={n => setHealth({ intervalSeconds: n })} />
        <NumField label="timeout (s)" value={health.timeoutSeconds ?? 2} onChange={n => setHealth({ timeoutSeconds: n })} />
        <NumField label="unhealthy after" value={health.unhealthyThreshold ?? 2} onChange={n => setHealth({ unhealthyThreshold: n })} />
        <NumField label="healthy after" value={health.healthyThreshold ?? 3} onChange={n => setHealth({ healthyThreshold: n })} />
      </div>
      <label className="tog" style={{ marginTop: 14 }}
        title="On: sign-in moves back to a higher IdP once it's healthy again. Off: it stays on the current IdP until that one fails or you drain it.">
        <input type="checkbox" checked={(spec.failback ?? 'Automatic') === 'Automatic'}
          onChange={e => setSpec(s => ({ ...s, failback: e.target.checked ? 'Automatic' : 'Manual' }))} />Fail back automatically
      </label>
    </aside>
  )
}

// Trust: the controller's checks of the broker's registration at this IdP,
// as a badge; each check on hover.
function Trust({ status }: { status?: TierStatus }) {
  const checks = status?.trust?.checks
  if (!checks?.length) return null
  const fails = checks.filter(c => c.result === 'Fail')
  const detail = checks.map(c => `${c.result === 'Pass' ? '✓' : c.result === 'Fail' ? '✗' : '?'} ${c.name}${c.message ? `: ${c.message}` : ''}`).join('\n')
  return fails.length
    ? <span className="chip bad small" title={detail}>trust: {fails.map(f => f.name).join(', ')}</span>
    : <span className="chip ok small" title={detail}>trust ok</span>
}

function assuranceSummary(ic: IdentityContinuity, profiles: { name: string; phase?: string; mode?: string }[]): string {
  const closed = profiles.filter(p => p.phase === 'FailedClosed' && enforced(p)).length
  const min = ic.spec.assurancePolicy?.minimum
  return [`${profiles.length} rule${profiles.length === 1 ? '' : 's'}`, min && `default ${min}`, closed && `${closed} failing closed`].filter(Boolean).join(' · ')
}

// NumField: a whole number, at least 1. While it's being typed in, an empty
// or invalid entry keeps the last valid value, which comes back on blur.
export function NumField({ label, value, onChange }: { label: string; value: number; onChange: (n: number) => void }) {
  const [text, setText] = useState<string>()
  return (
    <label>{label}
      <input className={`field${text !== undefined && text !== String(value) ? ' invalid' : ''}`} type="number" min={1} step={1}
        value={text ?? String(value)} onBlur={() => setText(undefined)}
        onChange={e => {
          setText(e.target.value)
          const n = Number(e.target.value)
          if (e.target.value !== '' && Number.isInteger(n) && n >= 1) onChange(n)
        }} />
    </label>
  )
}

// SecretField writes an IdP's client secret, or, for an IdP not saved yet,
// stages it for Save. It's write-only: the value is sent once and never
// read back.
function SecretField({ ns, tier, stage }: { ns: string; tier: Tier; stage?: (value: string) => void }) {
  const [v, setV] = useState('')
  const [state, setState] = useState<string>()
  const ref = tier.oidc?.clientSecretRef
  if (!ref) return null
  const put = async () => {
    if (stage) { stage(v); setV(''); return }
    setState('saving')
    try {
      await putSecret(ns, ref.name, { key: ref.key ?? SECRET_KEY, value: v, for: 'tier' })
      setV(''); setState('saved')
    } catch (e) { setState((e as Error).message) }
  }
  return (
    <div className="secret">
      <span className="subtle small mono">client {tier.oidc?.clientID || '(no client id)'} · secret {ref.name}</span>
      <div className="row">
        <input className="field" type="password" {...noFill} placeholder="set client secret" aria-label={`Client secret of ${tier.name}`}
          value={v} onChange={e => setV(e.target.value)} />
        <button className="btn small" disabled={!v} onClick={put}>Set</button>
      </div>
      {state && <span className="subtle small">{state}</span>}
    </div>
  )
}

// AddTier adds an IdP to the working copy. Its client secret is staged and
// written once Save has stored the IdP.
function AddTier({ brokerIssuer, existing, onAdd, onCancel }: {
  brokerIssuer?: string; existing: string[]; onAdd: (t: Tier, secret: string) => void; onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [displayName, setDisplay] = useState('')
  const [issuer, setIssuer] = useState('')
  const [clientID, setClient] = useState('')
  const [secret, setSecret] = useState('')
  const valid = DNS.test(name) && name.length <= 40 && !existing.includes(name) && /^https:\/\/\S+$/.test(issuer) && clientID !== ''
  const add = () => onAdd({ name, displayName: displayName || undefined, type: 'oidc',
    oidc: { issuer, clientID, clientSecretRef: { name: `upstream-${name}`, key: SECRET_KEY } },
    failoverWhen: { unreachable: true, serverError: true, invalidDiscovery: true } }, secret)
  return (
    <div className="add">
      <div className="label">New OIDC IdP</div>
      <input className="field" placeholder="name (e.g. ping)" aria-label="Name" value={name} onChange={e => setName(e.target.value.toLowerCase())} />
      <input className="field" placeholder="display name" aria-label="Display name" value={displayName} onChange={e => setDisplay(e.target.value)} />
      <input className="field" placeholder="issuer (https://…)" aria-label="Issuer" value={issuer} onChange={e => setIssuer(e.target.value.trim())} />
      <input className="field" placeholder="client id" aria-label="Client ID" autoComplete="off" data-1p-ignore data-lpignore="true" value={clientID} onChange={e => setClient(e.target.value.trim())} />
      <input className="field" type="password" {...noFill} placeholder="client secret (stored as a Secret on save)" aria-label="Client secret" value={secret} onChange={e => setSecret(e.target.value)} />
      <p className="subtle small">Register this redirect URI with the IdP: <span className="mono">{brokerIssuer ?? '<broker issuer>'}/broker/{name || '<name>'}/endpoint</span></p>
      <div className="row"><span className="grow" /><button className="btn small ghost" onClick={onCancel}>Cancel</button><button className="btn small primary" disabled={!valid} onClick={add}>Add IdP</button></div>
    </div>
  )
}
