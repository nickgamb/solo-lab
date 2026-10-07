import { BaseEdge, EdgeLabelRenderer, getBezierPath, Handle, Position, useUpdateNodeInternals, type EdgeProps, type NodeProps } from '@xyflow/react'
import { memo, useEffect, useState, type CSSProperties } from 'react'
import { ATTRIBUTE_TYPES, type AttributeType, type ProfileAttribute, type Tier } from '../api'
import { DirectoryForm } from './DirectoryForm'
import { useSync } from './syncContext'
import { TestConnection } from './TestConnection'

export type Role = 'primary' | 'failover'
export type PathRow = { path: string; mapped: boolean }
export type IdpData = { tier: Tier; order: number; role: Role; color: string; paths: PathRow[]; credsPending: boolean }
export type AttrRow = { name: string; builtin: boolean; declared: boolean; type: AttributeType; multivalued?: boolean; displayName?: string }
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

// AttrForm adds an attribute to the broker's profile, or edits one: its name (fixed
// once added), display name, value type, and whether it holds a list.
function AttrForm({ attr, taken, onDone, onCancel }: {
  attr?: ProfileAttribute; taken: (name: string) => boolean
  onDone: (a: ProfileAttribute) => string | undefined | void; onCancel: () => void
}) {
  const [name, setName] = useState(attr?.name ?? '')
  const [displayName, setDisplayName] = useState(attr?.displayName ?? '')
  const [type, setType] = useState<AttributeType>(attr?.type ?? 'string')
  const [list, setList] = useState(!!attr?.multivalued)
  const [err, setErr] = useState<string>()
  const nameErr = attr ? undefined : !name ? undefined : !/^[a-zA-Z][a-zA-Z0-9_.-]*$/.test(name) ? 'letter first; letters, digits, _ . -'
    : taken(name) ? 'already in the profile' : undefined
  const submit = () => {
    if (!name || nameErr) return
    const e = onDone({ name, displayName: displayName.trim() || undefined, type, multivalued: list })
    if (e) setErr(e)
  }
  return (
    <div className="cm-attr-form nodrag nowheel" onKeyDown={e => {
      if (e.key === 'Escape') { e.stopPropagation(); onCancel() }
      // Enter in a text field submits; buttons and the checkbox keep their own Enter
      if (e.key === 'Enter' && e.target instanceof HTMLInputElement && e.target.type === 'text') { e.preventDefault(); submit() }
    }}>
      {attr ? <div className="mono small">{attr.name}</div>
        : <input type="text" className={`field${nameErr ? ' invalid' : ''}`} autoFocus placeholder="name (e.g. department)" aria-label="Attribute name" value={name} onChange={e => setName(e.target.value)} />}
      {nameErr && <span className="small danger-text">{nameErr}</span>}
      <input type="text" className="field" placeholder="display name (optional)" aria-label="Display name" value={displayName} onChange={e => setDisplayName(e.target.value)} />
      <div className="row">
        <label className="small grow">type
          <select className="field" value={type} onChange={e => setType(e.target.value as AttributeType)}>
            {ATTRIBUTE_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </label>
        <label className="tog small" title="Holds a list of values (e.g. several phone numbers); otherwise one"><input type="checkbox" checked={list} onChange={e => setList(e.target.checked)} />list</label>
      </div>
      {err && <span className="small danger-text">{err}</span>}
      <div className="row">
        <span className="grow" />
        <button className="btn small ghost" onClick={onCancel}>Cancel</button>
        <button className="btn small primary" disabled={!name || !!nameErr} onClick={submit}>{attr ? 'Done' : 'Add'}</button>
      </div>
    </div>
  )
}

const host = (u?: string) => { try { return u ? new URL(u).host : '' } catch { return u ?? '' } }

// IdpNode: one IdP's attributes, each wired to the broker's attribute it pairs
// with. The role says which way the sync moves values.
export const IdpNode = memo(({ id, data }: NodeProps) => {
  const d = data as IdpData
  const [editing, setEditing] = useState(false)
  useRows(id, d.paths.map(x => x.path))
  const t = d.tier
  const primary = d.role === 'primary'
  return (
    <div className="cm-node cm-idp left" style={tint(d.color)}>
      <div className="cm-head">
        <div className="row">
          <span className="order">{d.order}</span>
          <b className="grow ellipsis" title={t.displayName ? `${t.displayName} (${t.name})` : t.name}>{t.displayName || t.name}</b>
        </div>
        <div className="row">
          <span className="mono subtle small ellipsis grow" title={t.oidc?.issuer}>{host(t.oidc?.issuer) || 'no issuer'}</span>
          <span className="chip" title={primary ? "Read into the broker's profile" : "Written from the broker's profile; users the primary has are created here"}>
            {primary ? 'primary · read' : 'failover · written'}</span>
        </div>
      </div>
      <ul className="cm-rows">
        {d.paths.map(x => (
          <li key={x.path} className={x.mapped ? 'cm-row mapped' : 'cm-row'}>
            <span className="mono ellipsis grow" title={x.path}>{shortPath(x.path)}</span>
            <Handle type="source" position={Position.Right} id={`a:${x.path}`} className="cm-handle" />
          </li>
        ))}
        {!d.paths.length && <li className="cm-row subtle small">{t.directory ? 'Run Test connection to detect its attributes.' : 'Set its directory, then Test connection detects its attributes.'}</li>}
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

// ProfileNode: the broker's profile, the standard every IdP maps to.
// Its attributes are the inputs every IdP wires into.
export const ProfileNode = memo(({ id, data }: NodeProps) => {
  const d = data as ProfileData
  const c = useSync()
  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<string>()
  useRows(id, d.attrs.map(a => a.name + (a.name === editing ? '*' : '')))
  const taken = (n: string) => d.attrs.some(a => a.name === n && (a.builtin || a.declared))
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
            {editing === a.name ? (
              <AttrForm attr={{ name: a.name, displayName: a.displayName, type: a.type, multivalued: a.multivalued }} taken={taken}
                onDone={x => { c.updateAttribute(x); setEditing(undefined) }} onCancel={() => setEditing(undefined)} />
            ) : (
              <>
                <span className="mono ellipsis grow" title={a.displayName ?? (a.builtin ? `${a.name}: built into the broker's profile` : a.name)}>{a.name}</span>
                {!a.declared ? <span className="cm-disc danger-text" title="mapped but not in the profile: add it with + attribute">undeclared</span>
                  : a.builtin ? <span className="cm-type" title="built in">{a.type}</span>
                  : <button className="cm-type nodrag" onClick={() => setEditing(a.name)} title="Edit its display name, type, or list">{a.type}{a.multivalued ? ' · list' : ''}</button>}
                {!a.builtin && a.declared && (
                  <button className="cm-x nodrag" onClick={() => c.removeAttribute(a.name)} title={`Remove ${a.name} and its mappings`} aria-label={`Remove attribute ${a.name}`}>×</button>
                )}
              </>
            )}
          </li>
        ))}
        <li className="cm-row add">
          {adding
            ? <AttrForm taken={taken} onDone={x => { const e = c.addAttribute(x); if (!e) setAdding(false); return e }} onCancel={() => setAdding(false)} />
            : <button className="cm-addbtn nodrag" onClick={() => setAdding(true)} title="Add an attribute to the broker's profile">+ attribute</button>}
        </li>
      </ul>
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
