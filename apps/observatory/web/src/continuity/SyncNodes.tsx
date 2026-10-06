import { BaseEdge, EdgeLabelRenderer, getBezierPath, Handle, Position, useUpdateNodeInternals, type EdgeProps, type NodeProps } from '@xyflow/react'
import { memo, useEffect, useState, type CSSProperties } from 'react'
import type { Tier } from '../api'
import { DirectoryForm } from './DirectoryForm'
import { useSync } from './syncContext'
import { TestConnection } from './TestConnection'

export type Role = 'primary' | 'failover'
export type PathRow = { path: string; mapped: boolean; custom: boolean }
export type IdpData = { tier: Tier; order: number; role: Role; color: string; paths: PathRow[]; credsPending: boolean }
export type AttrRow = { name: string; builtin: boolean; multivalued: boolean; declared: boolean }
export type ProfileData = { title: string; issuer?: string; attrs: AttrRow[] }
export type MapEdgeData = { idp: string; path: string; attribute: string; color: string }

const tint = (color: string) => ({ '--tier': color }) as CSSProperties

// a SCIM extension attribute by its last part, the full URN on hover
const shortPath = (p: string) => (p.startsWith('urn:') ? `ext:${p.slice(p.lastIndexOf(':') + 1)}` : p)

// Rows come and go as attributes are added; React Flow has to re-measure the
// handles or new rows can't be wired.
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
  if (v === undefined) return <button className="cm-addbtn nodrag" autoFocus={back} onClick={() => setV('')} title={`Add ${placeholder}`}>{label}</button>
  const err = v ? validate(v.trim()) : undefined
  const done = () => { if (v.trim() && !err) { onAdd(v.trim()); close() } }
  return (
    <div className="cm-addrow nodrag">
      <input className={`field${err ? ' invalid' : ''}`} autoFocus value={v} placeholder={placeholder} aria-label={placeholder}
        onChange={e => setV(e.target.value)}
        onKeyDown={e => {
          // no default: the Enter would also press the button focus returns to
          if (e.key === 'Enter') { e.preventDefault(); done() }
          if (e.key === 'Escape') { e.stopPropagation(); close() }
        }}
        onBlur={() => { if (!v.trim()) setV(undefined); else done() }} />
      {err && <span className="small danger-text">{err}</span>}
    </div>
  )
}

const host = (u?: string) => { try { return u ? new URL(u).host : '' } catch { return u ?? '' } }

// IdpNode: one IdP's attributes, each wired to the S&V attribute it pairs
// with. The role says which way the sync moves values.
export const IdpNode = memo(({ id, data }: NodeProps) => {
  const d = data as IdpData
  const c = useSync()
  const [editing, setEditing] = useState(false)
  useRows(id, d.paths.map(x => x.path))
  const t = d.tier
  const primary = d.role === 'primary'
  const names = new Set(d.paths.map(x => x.path))
  return (
    <div className="cm-node cm-idp left" style={tint(d.color)}>
      <div className="cm-head">
        <div className="row">
          <span className="order">{d.order}</span>
          <b className="grow ellipsis" title={t.name}>{t.displayName || t.name}</b>
          <span className="chip" title={primary ? 'Read into S&V\'s profile' : 'Written from S&V\'s profile; users the primary has are created here'}>
            {primary ? 'primary · read' : 'failover · written'}</span>
        </div>
        <div className="mono subtle small ellipsis" title={t.oidc?.issuer}>{host(t.oidc?.issuer) || 'no issuer'}</div>
      </div>
      <ul className="cm-rows">
        {d.paths.map(x => (
          <li key={x.path} className={x.mapped ? 'cm-row mapped' : 'cm-row'}>
            <span className="mono ellipsis grow" title={x.path}>{shortPath(x.path)}</span>
            {x.custom && !x.mapped && (
              <button className="cm-x nodrag" onClick={() => c.dropPath(t.name, x.path)} title={`Remove ${x.path}`} aria-label={`Remove attribute ${x.path}`}>×</button>
            )}
            <Handle type="source" position={Position.Right} id={`a:${x.path}`} className="cm-handle" />
          </li>
        ))}
        {!d.paths.length && <li className="cm-row subtle small">Set the directory and test it to list its attributes.</li>}
        <li className="cm-row add">
          <AddRow label="+ attribute" placeholder="an attribute path (e.g. user_metadata.department)" onAdd={v => c.addPath(t.name, v)}
            validate={v => (/\s/.test(v) ? 'no spaces' : names.has(v) ? 'already listed' : undefined)} />
        </li>
      </ul>
      <div className="cm-foot">
        <div className="row">
          <span className="label grow">Directory</span>
          {!editing && <button className="cm-link nodrag" onClick={() => setEditing(true)} title="Where the sync reads and writes this IdP's users">Edit</button>}
        </div>
        {editing ? <DirectoryForm tier={t.name} issuer={t.oidc?.issuer} dir={t.directory} onClose={() => setEditing(false)} />
          : t.directory ? (
            <div className="small ellipsis" title={t.directory.url}><span className="chip">{t.directory.type}</span> <span className="mono">{host(t.directory.url)}</span></div>
          ) : <div className="subtle small">not set: this IdP isn't synced</div>}
        {d.credsPending && !editing && <div className="small subtle">credentials written on save</div>}
        {t.directory && !editing && <TestConnection idp={t.name} />}
      </div>
    </div>
  )
})

// ProfileNode: S&V's profile on the broker, the standard every IdP maps to.
// Its attributes are the inputs every IdP wires into.
export const ProfileNode = memo(({ id, data }: NodeProps) => {
  const d = data as ProfileData
  const c = useSync()
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
            <Handle type="target" position={Position.Left} id={`in:${a.name}`} className="cm-handle" />
            <span className="mono ellipsis grow" title={a.builtin ? `${a.name}: built into the broker's profile` : a.name}>{a.name}</span>
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
          <AddRow label="+ attribute" placeholder="an attribute name" onAdd={v => setErr(c.addAttribute(v))}
            validate={v => (!/^[a-zA-Z][a-zA-Z0-9_.-]*$/.test(v) ? 'letter first; letters, digits, _ . -'
              : d.attrs.some(a => a.name === v && (a.builtin || a.declared)) ? 'already in the profile' : undefined)} />
        </li>
      </ul>
      {err && <div className="small danger-text cm-pad">{err}</div>}
    </div>
  )
})

export const MappingEdge = memo((p: EdgeProps) => {
  const d = p.data as MapEdgeData
  const c = useSync()
  const [path, lx, ly] = getBezierPath(p)
  return (
    <>
      <BaseEdge id={p.id} path={path} className={p.selected ? 'cm-edge selected' : 'cm-edge'} style={{ stroke: d.color }} interactionWidth={16} />
      <EdgeLabelRenderer>
        <div className={p.selected ? 'cm-elabel nodrag nopan on' : 'cm-elabel nodrag nopan'} style={{ ...tint(d.color), transform: `translate(-50%,-50%) translate(${lx}px,${ly}px)` }}>
          <button onClick={() => c.removeMapping(d.idp, d.attribute)} title={`Remove ${d.idp}.${d.path} ↔ ${d.attribute}`} aria-label="Remove mapping">×</button>
        </div>
      </EdgeLabelRenderer>
    </>
  )
})
