import {
  Background, BackgroundVariant, Controls, ReactFlow, ReactFlowProvider, useNodesInitialized, useNodesState, useReactFlow,
  type Connection, type Edge, type EdgeChange, type Node,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { BUILTIN_ATTRIBUTES, IDENTITY_KEYS, type ContinuitySpec, type IdentityContinuity } from '../api'
import { useClaims } from './claimsContext'
import { IDP_W, UNI_W, idpPositions, sideOf, tierColor } from './claimsLayout'
import { IdpNode, MappingEdge, UnifiedNode, type AttrRow, type ClaimRow, type IdpData, type MapEdgeData, type UnifiedData } from './ClaimsNodes'

type XY = { x: number; y: number }
type Props = {
  ic: IdentityContinuity; spec: ContinuitySpec; extras: Record<string, string[]>; pending: Set<string>
  positions: Map<string, XY>; openEdge?: string; onMap: (tier: string, claim: string, attribute: string) => void
}

const claimNodeTypes = { idp: IdpNode, unified: UnifiedNode }
const claimEdgeTypes = { mapping: MappingEdge }
const rank = (i: number) => (i === 0 ? 'primary' : i === 1 ? 'secondary' : `#${i + 1}`)
const edgeId = (tier: string, attribute: string) => `${tier}|${attribute}`
const isKey = (n: string) => (IDENTITY_KEYS as readonly string[]).includes(n)
// token mechanics, not facts about the user: never offered for mapping
const PROTOCOL = new Set(['iss', 'aud', 'exp', 'iat', 'nbf', 'jti', 'auth_time', 'nonce', 'at_hash', 'c_hash', 's_hash', 'azp', 'sid', 'acr', 'amr', 'cnf'])

export function ClaimsCanvas(props: Props) {
  return <ReactFlowProvider><Canvas {...props} /></ReactFlowProvider>
}

function Canvas({ ic, spec, extras, pending, positions, openEdge, onMap }: Props) {
  const c = useClaims()
  const [sel, setSel] = useState<string>()

  const { built, edges, legend } = useMemo(() => {
    const status = new Map((ic.status?.tiers ?? []).map(s => [s.name, s]))
    // every IdP in chain order gets a node (the broker's break-glass tier has
    // nothing to map)
    const chain = spec.tiers.map((t, i) => ({ t, i })).filter(x => x.t.type === 'oidc')
    const oidc = chain
    const kOf = new Map(chain.map((x, k) => [x.t.name, k]))
    const declared = new Set((spec.profile?.attributes ?? []).map(a => a.name))
    const idps: IdpData[] = chain.map(({ t, i }, k) => {
      const discovered = (status.get(t.name)?.claimsSupported ?? []).filter(c => !PROTOCOL.has(c))
      const mappedClaims = (t.claims ?? []).map(m => m.claim)
      const extra = extras[t.name] ?? []
      const rows: ClaimRow[] = [...new Set([...discovered, ...mappedClaims, ...extra])].map(name => ({
        name, discovered: discovered.includes(name), mapped: mappedClaims.includes(name), custom: extra.includes(name) && !discovered.includes(name),
      }))
      const data: IdpData = { tier: t, order: i + 1, rank: rank(k), side: sideOf(k), color: tierColor(k), claims: rows, credsPending: pending.has(t.name) }
      return data
    })
    const at = idpPositions(idps.map(d => d.claims.length))
    const nodes: Node[] = idps.map((data, k) => {
      const id = `idp:${data.tier.name}`
      return { id, type: 'idp', position: positions.get(id) ?? at[k], data, deletable: false, style: { width: IDP_W } }
    })
    // attributes a tier maps but the profile doesn't declare still get a row,
    // so their wires show (and say what's wrong)
    const orphans = oidc.flatMap(x => (x.t.claims ?? []).map(m => m.attribute))
      .filter(a => !(BUILTIN_ATTRIBUTES as readonly string[]).includes(a) && !declared.has(a))
    const attrs: AttrRow[] = [
      ...BUILTIN_ATTRIBUTES.map(name => ({ name, builtin: true, key: isKey(name), multivalued: false, declared: true })),
      ...(spec.profile?.attributes ?? []).map(a => ({ name: a.name, builtin: false, key: isKey(a.name), multivalued: !!a.multivalued, declared: true })),
      ...[...new Set(orphans)].map(name => ({ name, builtin: false, key: false, multivalued: false, declared: false })),
    ]
    const uni: UnifiedData = { title: `${ic.metadata.name} profile`, issuer: ic.status?.broker?.issuer, attrs, tokenClients: spec.profile?.tokenClients ?? [] }
    nodes.push({ id: 'unified', type: 'unified', position: positions.get('unified') ?? { x: 0, y: 0 }, data: uni, deletable: false, style: { width: UNI_W } })
    const edges: Edge[] = oidc.flatMap(({ t }) => (t.claims ?? []).map(m => {
      const k = kOf.get(t.name) ?? 0
      const id = edgeId(t.name, m.attribute)
      const data: MapEdgeData = { tier: t.name, claim: m.claim, attribute: m.attribute, color: tierColor(k), directoryPath: m.directoryPath, open: id === openEdge }
      return {
        id, type: 'mapping', source: `idp:${t.name}`, sourceHandle: `c:${m.claim}`, target: 'unified',
        targetHandle: `l:${m.attribute}`, selected: id === sel, data, zIndex: id === openEdge ? 10 : 0,
      }
    }))
    const legend = idps.map(d => ({ name: d.tier.displayName || d.tier.name, color: d.color }))
    return { built: nodes, edges, legend }
  }, [ic, spec, extras, pending, positions, openEdge, sel])

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

  const valid = (x: Connection | Edge) => !!x.source.startsWith('idp:') && x.target === 'unified' &&
    !!x.targetHandle?.startsWith('l:')
  const connect = (x: Connection) => {
    if (!valid(x) || !x.sourceHandle || !x.targetHandle) return
    onMap(x.source.slice(4), x.sourceHandle.slice(2), x.targetHandle.slice(2))
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
        <span className="subtle small grow">Drag from an IdP claim to a profile attribute. Earlier tiers win when the scheduled sync finds different values.
          The first tier's claims update the profile at every sign-in; later tiers fill an attribute at first sign-in only.</span>
        {legend.map(l => <span key={l.name} className="cm-swatch small" style={{ '--tier': l.color } as CSSProperties}>{l.name}</span>)}
      </div>
      <ReactFlow nodes={nodes} edges={edges} nodeTypes={claimNodeTypes} edgeTypes={claimEdgeTypes}
        onNodesChange={onNodesChange} onEdgesChange={onEdgesChange} onConnect={connect} isValidConnection={valid}
        onNodeDragStop={(_, n) => positions.set(n.id, n.position)}
        onEdgeClick={(_, e) => c.openEdge(e.id)} onPaneClick={() => c.openEdge(undefined)}
        deleteKeyCode={['Delete', 'Backspace']} minZoom={0.2} maxZoom={1.6}
        connectionLineStyle={{ stroke: 'var(--accent)', strokeWidth: 2 }} proOptions={{ hideAttribution: true }}>
        <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="var(--border)" />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  )
}
