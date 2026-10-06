import { BaseEdge, EdgeLabelRenderer, getBezierPath, Handle, Position, useUpdateNodeInternals, type EdgeProps, type NodeProps } from '@xyflow/react'
import { memo, useEffect, useState, type CSSProperties } from 'react'
import type { Tier } from '../api'
import { useClaims } from './claimsContext'
import type { Side } from './claimsLayout'
import { DirectoryForm } from './DirectoryForm'
import { TestConnection } from './TestConnection'

export type ClaimRow = { name: string; discovered: boolean; mapped: boolean; custom: boolean }
export type IdpData = { tier: Tier; order: number; rank: string; side: Side; color: string; claims: ClaimRow[]; credsPending: boolean }
export type AttrRow = { name: string; builtin: boolean; key: boolean; multivalued: boolean; declared: boolean }
export type UnifiedData = { title: string; issuer?: string; attrs: AttrRow[]; tokenClients: string[] }
export type MapEdgeData = { tier: string; claim: string; attribute: string; color: string; directoryPath?: string; open: boolean }

const KEY_TIP = 'Identity key: set at first sign-in, never overwritten by a mapping or the sync.'
const tint = (color: string) => ({ '--tier': color }) as CSSProperties

// Rows come and go as claims and attributes are added; React Flow has to
// re-measure the handles or new rows can't be wired.
function useRows(id: string, rows: string[]) {
  const update = useUpdateNodeInternals()
  const key = rows.join('\n')
  useEffect(() => { update(id) }, [id, key, update])
}

// AddRow: a "+ thing" row that turns into an inline input.
function AddRow({ label, placeholder, validate, onAdd }: { label: string; placeholder: string; validate: (v: string) => string | undefined; onAdd: (v: string) => void }) {
  const [v, setV] = useState<string>()
  const [back, setBack] = useState(false) // keyboard focus returns to the button after an add or cancel
  const close = () => { setV(undefined); setBack(true) }
  if (v === undefined) return <button className="cm-addbtn nodrag" autoFocus={back} onClick={() => setV('')} title={`Add a ${placeholder}`}>{label}</button>
  const err = v ? validate(v.trim()) : undefined
  const done = () => { if (v.trim() && !err) { onAdd(v.trim()); close() } }
  return (
    <div className="cm-addrow nodrag">
      <input className={`field${err ? ' invalid' : ''}`} autoFocus value={v} placeholder={placeholder} aria-label={placeholder}
        onChange={e => setV(e.target.value)}
        onKeyDown={e => {
          if (e.key === 'Enter') done()
          if (e.key === 'Escape') { e.stopPropagation(); close() }
        }}
        onBlur={() => { if (!v.trim()) setV(undefined) }} />
      {err && <span className="small danger-text">{err}</span>}
    </div>
  )
}

const host = (u?: string) => { try { return u ? new URL(u).host : '' } catch { return u ?? '' } }

export const IdpNode = memo(({ id, data }: NodeProps) => {
  const d = data as IdpData
  const c = useClaims()
  const [editing, setEditing] = useState(false)
  useRows(id, d.claims.map(x => x.name))
  const t = d.tier
  const pos = d.side === 'left' ? Position.Right : Position.Left
  const names = new Set(d.claims.map(x => x.name))
  return (
    <div className={`cm-node cm-idp ${d.side}`} style={tint(d.color)}>
      <div className="cm-head">
        <div className="row">
          <span className="order">{d.order}</span>
          <b className="grow ellipsis" title={t.name}>{t.displayName || t.name}</b>
          <span className="chip">{d.rank}</span>
        </div>
        <div className="mono subtle small ellipsis" title={t.oidc?.issuer}>{t.oidc?.issuer ?? 'no issuer'}</div>
      </div>
      <ul className="cm-rows">
        {d.claims.map(x => (
          <li key={x.name} className={x.mapped ? 'cm-row mapped' : 'cm-row'}>
            <span className="mono ellipsis grow" title={x.name}>{x.name}</span>
            {x.discovered && <span className="cm-disc" title="advertised by the IdP (claims_supported in its discovery document)">discovered</span>}
            {x.custom && !x.mapped && (
              <button className="cm-x nodrag" onClick={() => c.dropClaim(t.name, x.name)} title={`Remove ${x.name}`} aria-label={`Remove claim ${x.name}`}>×</button>
            )}
            <Handle type="source" position={pos} id={`c:${x.name}`} className="cm-handle" title={`Drag ${x.name} to a profile attribute`} />
          </li>
        ))}
        {!d.claims.length && <li className="cm-row subtle small">No claims discovered yet.</li>}
        <li className="cm-row add">
          <AddRow label="+ claim" placeholder="claim name" onAdd={v => c.addClaim(t.name, v)}
            validate={v => (/\s/.test(v) ? 'no spaces' : names.has(v) ? 'already listed' : undefined)} />
        </li>
      </ul>
      <div className="cm-foot">
        <div className="row">
          <span className="label grow">Directory</span>
          {!editing && <button className="cm-link nodrag" onClick={() => setEditing(true)} title="Where the sync reads this tier's user records">Edit</button>}
        </div>
        {editing ? <DirectoryForm tier={t.name} dir={t.directory} onClose={() => setEditing(false)} />
          : t.directory ? (
            <div className="small ellipsis" title={t.directory.url}><span className="chip">{t.directory.type}</span> <span className="mono">{host(t.directory.url)}</span></div>
          ) : <div className="subtle small">not configured</div>}
        {d.credsPending && !editing && <div className="small subtle">credentials written on save</div>}
        {t.directory && !editing && <TestConnection tier={t.name} />}
      </div>
    </div>
  )
})

export const UnifiedNode = memo(({ id, data }: NodeProps) => {
  const d = data as UnifiedData
  const c = useClaims()
  const [err, setErr] = useState<string>()
  useRows(id, d.attrs.map(a => a.name))
  return (
    <div className="cm-node cm-unified">
      <div className="cm-head">
        <b>{d.title}</b>
        <div className="mono subtle small ellipsis" title={d.issuer}>{d.issuer ?? 'broker issuer unknown'}</div>
      </div>
      <ul className="cm-rows">
        {d.attrs.map(a => (
          <li key={a.name} className={a.declared ? 'cm-row' : 'cm-row undeclared'}>
            <Handle type="target" position={Position.Left} id={`l:${a.name}`} className="cm-handle" />
            <span className="mono ellipsis grow" title={a.key ? `${a.name}. ${KEY_TIP}` : a.builtin ? `${a.name}: built into the broker's profile` : a.name}>{a.name}</span>
            {!a.declared && <span className="cm-disc danger-text" title="mapped but not in the profile: add it with + attribute">undeclared</span>}
            {!a.builtin && a.declared && (
              <>
                <button className={a.multivalued ? 'cm-multi on nodrag' : 'cm-multi nodrag'} aria-pressed={a.multivalued}
                  onClick={() => c.toggleMultivalued(a.name)} title={a.multivalued ? 'Multivalued: holds a list' : 'Single value (click for a list)'}>[ ]</button>
                <button className="cm-x nodrag" onClick={() => c.removeAttribute(a.name)} title={`Remove ${a.name} and its mappings`} aria-label={`Remove attribute ${a.name}`}>×</button>
              </>
            )}
          </li>
        ))}
        <li className="cm-row add">
          <AddRow label="+ attribute" placeholder="attribute name" onAdd={v => setErr(c.addAttribute(v))}
            validate={v => (!/^[a-zA-Z][a-zA-Z0-9_.-]*$/.test(v) ? 'letter first; letters, digits, _ . -'
              : d.attrs.some(a => a.name === v && (a.builtin || a.declared)) ? 'already in the profile' : undefined)} />
        </li>
      </ul>
      {err && <div className="small danger-text cm-pad">{err}</div>}
      <TokenClients clients={d.tokenClients} />
    </div>
  )
})

// TokenClients: which clients' tokens carry the profile attributes.
function TokenClients({ clients }: { clients: string[] }) {
  const c = useClaims()
  const [v, setV] = useState<string>()
  const done = () => { c.setTokenClients((v ?? '').split(/[\s,]+/).filter(Boolean)); setV(undefined) }
  return (
    <div className="cm-foot">
      <div className="row">
        <span className="label grow" title="Clients whose tokens carry the profile attributes. Others never see them.">In tokens for</span>
        {v === undefined && <button className="cm-link nodrag" onClick={() => setV(clients.join(', '))} title="Clients whose tokens carry the profile attributes">Edit</button>}
      </div>
      {v === undefined
        ? clients.length ? <div className="small mono ellipsis" title={clients.join(', ')}>{clients.join(', ')}</div> : <div className="subtle small">no client</div>
        : <input className="field mono nodrag" autoFocus value={v} placeholder="client IDs, comma separated" aria-label="Token clients"
            onChange={e => setV(e.target.value)} onBlur={done}
            onKeyDown={e => {
              if (e.key === 'Enter') done()
              if (e.key === 'Escape') { e.stopPropagation(); setV(undefined) }
            }} />}
    </div>
  )
}

export const MappingEdge = memo((p: EdgeProps) => {
  const d = p.data as MapEdgeData
  const c = useClaims()
  const [path, lx, ly] = getBezierPath(p)
  return (
    <>
      <BaseEdge id={p.id} path={path} className={p.selected ? 'cm-edge selected' : 'cm-edge'} style={{ stroke: d.color }} interactionWidth={16} />
      <EdgeLabelRenderer>
        <div className={p.selected || d.open ? 'cm-elabel nodrag nopan on' : 'cm-elabel nodrag nopan'} style={{ ...tint(d.color), transform: `translate(-50%,-50%) translate(${lx}px,${ly}px)` }}>
          <button onClick={() => c.openEdge(p.id)} title={`${d.claim} → ${d.attribute}: set where it is in the directory`}>
            {d.directoryPath ? <span className="mono">{d.directoryPath}</span> : <span aria-hidden>·</span>}
          </button>
          <button onClick={() => c.removeMapping(d.tier, d.attribute)} title={`Remove ${d.claim} → ${d.attribute}`} aria-label="Remove mapping">×</button>
          {d.open && <PathPopover d={d} />}
        </div>
      </EdgeLabelRenderer>
    </>
  )
})

function PathPopover({ d }: { d: MapEdgeData }) {
  const c = useClaims()
  const [v, setV] = useState(d.directoryPath ?? '')
  const done = () => { c.setPath(d.tier, d.attribute, v); c.openEdge(undefined) }
  return (
    <div className="cm-pop nowheel" role="dialog" aria-label="Directory path">
      <div className="small"><span className="mono">{d.claim}</span> → <span className="mono">{d.attribute}</span></div>
      <label className="small subtle">Where this value is in the directory record, if not the claim's standard place
        <input className="field mono" autoFocus value={v} placeholder="e.g. urn:…:enterprise:2.0:User.department" onChange={e => setV(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') done()
            if (e.key === 'Escape') { e.stopPropagation(); c.openEdge(undefined) }
          }} />
      </label>
      <div className="row">
        <button className="btn small ghost danger-text" onClick={() => { c.removeMapping(d.tier, d.attribute); c.openEdge(undefined) }} title="Remove this mapping">Remove</button>
        <span className="grow" />
        <button className="btn small ghost" onClick={() => c.openEdge(undefined)} title="Close without changes">Cancel</button>
        <button className="btn small primary" onClick={done} title="Set the directory path">Done</button>
      </div>
    </div>
  )
}
