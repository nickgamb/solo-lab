import {
  Background, BackgroundVariant, Controls, ReactFlow, ReactFlowProvider, useNodesInitialized, useNodesState, useReactFlow,
  type Connection, type Edge, type EdgeChange, type Node,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { BUILTIN_ATTRIBUTES, type ContinuitySpec, type Directory, type IdentityContinuity } from '../api'
import { kindOf } from './mapping'
import { useSync } from './syncContext'
import { IDP_W, PROFILE_W, idpColor, idpPositions } from './syncLayout'
import { IdpNode, MappingEdge, ProfileNode, type AttrRow, type IdpData, type MapEdgeData, type PathRow, type ProfileData } from './SyncNodes'

type XY = { x: number; y: number }
type Props = {
  ic: IdentityContinuity; spec: ContinuitySpec; extras: Record<string, string[]>; schemas: Record<string, string[]>
  pending: Set<string>; positions: Map<string, XY>; onMap: (idp: string, path: string, attribute: string) => void
}

const nodeTypes = { idp: IdpNode, profile: ProfileNode }
const edgeTypes = { mapping: MappingEdge }
const edgeId = (idp: string, attribute: string) => `${idp}|${attribute}`

const ENTERPRISE = 'urn:ietf:params:scim:schemas:extension:enterprise:2.0:User'
// what each kind of IdP commonly holds, until its directory's schema is read
const COMMON: Record<Directory['type'], string[]> = {
  scim: ['emails[primary eq true].value', 'name.givenName', 'name.familyName', 'displayName', 'title',
    'phoneNumbers[type eq "work"].value', `${ENTERPRISE}:department`, `${ENTERPRISE}:employeeNumber`, `${ENTERPRISE}:costCenter`],
  auth0: ['email', 'given_name', 'family_name', 'name', 'nickname', 'user_metadata.department'],
  keycloak: ['email', 'firstName', 'lastName'],
}

export function SyncCanvas(props: Props) {
  return <ReactFlowProvider><Canvas {...props} /></ReactFlowProvider>
}

function Canvas({ ic, spec, extras, schemas, pending, positions, onMap }: Props) {
  const c = useSync()
  const [sel, setSel] = useState<string>()

  const { built, edges, legend } = useMemo(() => {
    // the IdPs in chain order: the first is the primary (read), the rest failovers (written)
    const idps = spec.tiers.filter(t => t.type === 'oidc')
    const declared = new Set((spec.profile?.attributes ?? []).map(a => a.name))
    const data: IdpData[] = idps.map((t, k) => {
      const mapped = (t.attributes ?? []).map(m => m.path)
      const known = [...COMMON[t.directory?.type ?? kindOf(t.oidc?.issuer)], ...(schemas[t.name] ?? [])]
      const extra = extras[t.name] ?? []
      const paths: PathRow[] = [...new Set([...mapped, ...known, ...extra])].map(path => ({
        path, mapped: mapped.includes(path), custom: extra.includes(path) && !known.includes(path),
      }))
      return { tier: t, order: k + 1, role: k === 0 ? 'primary' : 'failover', color: idpColor(k), paths, credsPending: pending.has(t.name) }
    })
    const at = idpPositions(data.map(d => d.paths.length))
    const nodes: Node[] = data.map((d, k) => {
      const id = `idp:${d.tier.name}`
      return { id, type: 'idp', position: positions.get(id) ?? at[k], data: d, deletable: false, style: { width: IDP_W } }
    })
    // attributes an IdP maps but the profile doesn't declare still get a row,
    // so their wires show (and say what's wrong)
    const orphans = idps.flatMap(t => (t.attributes ?? []).map(m => m.attribute))
      .filter(a => !(BUILTIN_ATTRIBUTES as readonly string[]).includes(a) && !declared.has(a))
    const attrs: AttrRow[] = [
      ...BUILTIN_ATTRIBUTES.filter(a => a !== 'username').map(name => ({ name, builtin: true, multivalued: false, declared: true })),
      ...(spec.profile?.attributes ?? []).map(a => ({ name: a.name, builtin: false, multivalued: !!a.multivalued, declared: true })),
      ...[...new Set(orphans)].map(name => ({ name, builtin: false, multivalued: false, declared: false })),
    ]
    const profile: ProfileData = { title: "S&V profile", issuer: ic.status?.broker?.issuer, attrs }
    nodes.push({ id: 'profile', type: 'profile', position: positions.get('profile') ?? { x: 0, y: 0 }, data: profile, deletable: false, style: { width: PROFILE_W } })
    const edges: Edge[] = idps.flatMap((t, k) => (t.attributes ?? []).map(m => {
      const id = edgeId(t.name, m.attribute)
      const d: MapEdgeData = { idp: t.name, path: m.path, attribute: m.attribute, color: idpColor(k) }
      return {
        id, type: 'mapping', data: d, selected: id === sel,
        source: `idp:${t.name}`, sourceHandle: `a:${m.path}`, target: 'profile', targetHandle: `in:${m.attribute}`,
      }
    }))
    const legend = data.map(d => ({ name: d.tier.displayName || d.tier.name, color: d.color, role: d.role }))
    return { built: nodes, edges, legend }
  }, [ic, spec, extras, schemas, pending, positions, sel])

  // React Flow owns positions and measurements; the spec owns what each node shows
  const [nodes, setNodes, onNodesChange] = useNodesState(built)
  useEffect(() => {
    setNodes(prev => {
      const old = new Map(prev.map(n => [n.id, n]))
      return built.map(n => { const o = old.get(n.id); return o ? { ...o, data: n.data } : n })
    })
  }, [built, setNodes])

  // fit once every node has its real size (the rows make them tall)
  const rf = useReactFlow()
  const ready = useNodesInitialized()
  const fitted = useRef(false)
  useEffect(() => {
    if (!ready || fitted.current) return
    fitted.current = true
    requestAnimationFrame(() => { rf.fitView({ padding: 0.08 }) })
  }, [ready, rf])

  // an IdP's attribute -> the S&V attribute it pairs with
  const parse = (x: Connection | Edge) => (x.source.startsWith('idp:') && x.target === 'profile' && x.targetHandle?.startsWith('in:')
    ? { idp: x.source.slice(4), path: x.sourceHandle?.slice(2), attribute: x.targetHandle.slice(3) } : undefined)
  const valid = (x: Connection | Edge) => !!parse(x)?.path
  const connect = (x: Connection) => {
    const m = parse(x)
    if (m?.path) onMap(m.idp, m.path, m.attribute)
  }
  const onEdgesChange = (changes: EdgeChange[]) => changes.forEach(ch => {
    if (ch.type === 'select') setSel(s => (ch.selected ? ch.id : s === ch.id ? undefined : s))
    if (ch.type === 'remove') {
      const i = ch.id.indexOf('|')
      c.removeMapping(ch.id.slice(0, i), ch.id.slice(i + 1))
    }
  })

  return (
    <div className="cm-canvas">
      <div className="cm-legend">
        <span className="subtle small grow">Wire each IdP's attributes to the S&amp;V attributes they pair with. The sync reads the primary into S&amp;V's profile,
          then writes the profile to every failover, so its users can sign in there if it takes over.</span>
        {legend.map(l => <span key={l.name} className="cm-swatch small" style={{ '--tier': l.color } as CSSProperties} title={l.role}>{l.name}</span>)}
      </div>
      <ReactFlow nodes={nodes} edges={edges} nodeTypes={nodeTypes} edgeTypes={edgeTypes}
        onNodesChange={onNodesChange} onEdgesChange={onEdgesChange} onConnect={connect} isValidConnection={valid}
        onNodeDragStop={(_, n) => positions.set(n.id, n.position)}
        deleteKeyCode={['Delete', 'Backspace']} minZoom={0.2} maxZoom={1.6}
        connectionLineStyle={{ stroke: 'var(--accent)', strokeWidth: 2 }} proOptions={{ hideAttribution: true }}>
        <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  )
}
