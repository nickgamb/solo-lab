import agw from '../assets/agentgateway.svg?raw'
import kagent from '../assets/kagent.svg?raw'
import kgw from '../assets/kgateway.svg?raw'
import type { LabNode } from '../api'

const glyph: Record<string, string> = {
  idp: 'M12 2 4 5v6c0 5 3.4 9.7 8 11 4.6-1.3 8-6 8-11V5l-8-3zm0 6a3 3 0 1 1 0 6 3 3 0 0 1 0-6z',
  mcp: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM16.5 13v3.5H13v2h3.5V22h2v-3.5H22v-2h-3.5V13z',
  db: 'M12 3c-4.4 0-8 1.3-8 3v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6c0-1.7-3.6-3-8-3zm0 2c3.9 0 6 1.1 6 1s-2.1 1-6 1-6-.9-6-1 2.1-1 6-1z',
  llm: 'M12 2l2.2 5.8L20 10l-5.8 2.2L12 18l-2.2-5.8L4 10l5.8-2.2z',
  ui: 'M3 4h18v13H3zM8 20h8v1.5H8z',
  controller: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zm9 3h-2.1a7 7 0 0 0-1-2.4l1.5-1.5-1.4-1.4-1.5 1.5a7 7 0 0 0-2.5-1V4h-2v2.2a7 7 0 0 0-2.4 1L8 5.7 6.6 7.1l1.5 1.5a7 7 0 0 0-1 2.4H5v2h2.1a7 7 0 0 0 1 2.4l-1.5 1.5L8 18.3l1.5-1.5a7 7 0 0 0 2.5 1V20h2v-2.2a7 7 0 0 0 2.4-1l1.5 1.5 1.4-1.4-1.5-1.5a7 7 0 0 0 1-2.4H21z',
  substrate: 'M3 5h8v6H3zM13 5h8v6h-8zM3 13h8v6H3zM13 13h8v6h-8z',
  workload: 'M12 2 3 7v10l9 5 9-5V7l-9-5zm0 2.3L18.5 8 12 11.7 5.5 8 12 4.3z',
  external: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm6.9 9h-3a15 15 0 0 0-1.3-6 8 8 0 0 1 4.3 6zM12 4c.9 1.3 1.8 3.8 2 7h-4c.2-3.2 1.1-5.7 2-7zm-2.6 1a15 15 0 0 0-1.3 6h-3a8 8 0 0 1 4.3-6zM5.1 13h3a15 15 0 0 0 1.3 6 8 8 0 0 1-4.3-6zM12 20c-.9-1.3-1.8-3.8-2-7h4c-.2 3.2-1.1 5.7-2 7zm2.6-1a15 15 0 0 0 1.3-6h3a8 8 0 0 1-4.3 6z',
  cluster: 'M4 4h16v4H4zM4 10h16v4H4zM4 16h16v4H4z',
  tool: 'M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.4 2.4-2.6-.6-.6-2.6z',
  waypoint: 'M12 2 2 12l10 10 10-10L12 2zm0 5 5 5-5 5-5-5 5-5z',
}

export function NodeIcon({ n, size = 18 }: { n: LabNode; size?: number }) {
  const cls = String(n.summary?.class ?? '')
  const raw = n.kind === 'agent' ? kagent
    : cls.includes('agentgateway') || n.kind === 'fabric' ? agw
      : cls === 'kgateway' || cls.includes('enterprise-kgateway') ? kgw
        : undefined
  if (raw) return <span className="nicon" style={{ width: size, height: size }} dangerouslySetInnerHTML={{ __html: raw }} />
  const d = glyph[n.kind] ?? glyph.workload
  return (
    <span className="nicon" style={{ width: size, height: size }}>
      <svg viewBox="0 0 24 24" width={size} height={size} fill="currentColor"><path d={d} /></svg>
    </span>
  )
}

export const kindLabel: Record<string, string> = {
  gateway: 'Gateway', waypoint: 'Waypoint', agent: 'Agent', mcp: 'MCP server', idp: 'Identity provider',
  db: 'Data', llm: 'Model provider', ui: 'UI', controller: 'Control plane', substrate: 'Worker pool',
  workload: 'Workload', external: 'External', cluster: 'Cluster', tool: 'Test tool',
  fabric: 'agentgateway attachment',
}

// The Solo products the map can highlight, in display order.
export const PRODUCTS: { id: string; label: string; blurb: string; docs: string }[] = [
  { id: 'kgateway', label: 'kgateway', blurb: 'the edge: TLS, per-party listeners, SSO', docs: 'https://docs.solo.io/kgateway/' },
  { id: 'agentgateway', label: 'agentgateway', blurb: 'AI and MCP gateway, waypoint, egress', docs: 'https://docs.solo.io/agentgateway/' },
  { id: 'kagent', label: 'kagent', blurb: 'agents, their controller and UI', docs: 'https://docs.solo.io/kagent/' },
  { id: 'kmcp', label: 'kmcp', blurb: 'MCP servers as Kubernetes resources', docs: 'https://docs.solo.io/kagent/' },
  { id: 'agentregistry', label: 'agentregistry', blurb: 'the catalog of agents, tools and skills', docs: 'https://docs.solo.io/agentregistry/' },
  { id: 'substrate', label: 'Agent Substrate', blurb: 'snapshot-backed sandboxes for agents', docs: 'https://docs.solo.io/kagent/' },
  { id: 'istio', label: 'Istio ambient', blurb: 'mTLS, SPIFFE identity, waypoints', docs: 'https://docs.solo.io/istio/' },
]

const productGlyph: Record<string, string> = {
  kmcp: glyph.mcp,
  agentregistry: 'M4 3h12l4 4v14H4zM7 9h10v2H7zm0 4h10v2H7zm0 4h6v2H7z',
  substrate: glyph.substrate,
  istio: 'M11 2v15H4L11 2zm2 4 6 11h-6V6zM3 19h18l-2 3H5z',
}

export function ProductIcon({ id, size = 14 }: { id: string; size?: number }) {
  const raw = id === 'agentgateway' ? agw : id === 'kagent' ? kagent : id === 'kgateway' ? kgw : undefined
  if (raw) return <span className="nicon" style={{ width: size, height: size }} dangerouslySetInnerHTML={{ __html: raw }} />
  return (
    <span className="nicon" style={{ width: size, height: size }}>
      <svg viewBox="0 0 24 24" width={size} height={size} fill="currentColor"><path d={productGlyph[id] ?? glyph.workload} /></svg>
    </span>
  )
}
