import { BaseEdge, EdgeLabelRenderer, getBezierPath, type EdgeProps } from '@xyflow/react'
import { memo } from 'react'
import type { EdgeKind } from '../api'
import { spline, type Pt } from './layout'

export type FlowData = {
  kind: EdgeKind
  label?: string
  rps?: number
  l4?: number
  errors?: number
  pulses?: { id: string; bad: boolean }[]
  state?: 'active' | 'down' | 'standby' // continuity wire state
  fog?: boolean
  showLabel?: boolean
  points?: Pt[] // the routed curve inside a party; otherwise a bezier
  via?: string[] // waypoints the call passes, shown as a badge
  badgeAt?: 'src' | 'dst' // which end carries the badge (one per group of wires)
  focused?: boolean // part of the hovered node's neighbourhood
  cut?: boolean // the network on this path is cut (a simulated outage)
  badgeLit?: boolean // a hop on this wire is the product picked in the rail (lights its group's one badge)
  [key: string]: unknown
}

// FlowEdge encodes three things: thickness is traffic (three steps), colour
// is the kind of call (red when denied or failing), and style says whether
// anything has flowed lately (solid with moving dots) or not (thin, grey,
// dashed). Each request also sends a pulse along it.
export const FlowEdge = memo((p: EdgeProps) => {
  const d = (p.data ?? {}) as FlowData
  const [bz, bx, by] = getBezierPath({ ...p, curvature: 0.4 })
  const routed = !!d.points && d.points.length > 1
  const path = routed ? spline(d.points!) : bz
  const pts = routed ? d.points! : [{ x: p.sourceX, y: p.sourceY }, { x: p.targetX, y: p.targetY }]
  const end = d.badgeAt === 'dst' ? pts[pts.length - 1] : pts[0]
  const mid = d.badgeAt ? { x: end.x + (d.badgeAt === 'dst' ? -22 : 22), y: end.y } : routed ? pts[Math.floor(pts.length / 2)] : { x: bx, y: by }
  const load = (d.rps ?? 0) + (d.l4 ?? 0) * 4
  const live = load > 0.004 || d.state === 'active'
  const bad = (d.errors ?? 0) > 0 || d.state === 'down'
  const color = d.state === 'active' ? 'var(--ok)' : bad ? 'var(--bad)' : `var(--k-${d.kind})`
  const width = d.state ? 3 : !live ? 2 : load < 0.2 ? 2.8 : load < 2 ? 3.8 : 5
  const speed = live ? Math.max(0.5, 1.6 - Math.log10(1 + load)) : 0
  const cls = ['flow', live ? 'live' : 'idle', bad ? 'bad' : '', d.state ?? '', d.fog ? 'fog' : '', d.focused ? 'focused' : ''].join(' ')
  return (
    <>
      {live && !d.fog && <path d={path} className="glow" style={{ stroke: color, strokeWidth: width + 6 }} />}
      <BaseEdge id={p.id} path={path} className={cls} markerEnd={live || d.focused ? p.markerEnd : undefined} style={{ stroke: color, strokeWidth: width }} interactionWidth={14} />
      {live && !d.fog && d.state !== 'down' && (
        <path d={path} className="dots" style={{ stroke: color, strokeWidth: width + 1.4, animationDuration: `${speed}s` }} />
      )}
      {!d.fog && d.pulses?.map(x => (
        <circle key={x.id} r={4.5} className={x.bad ? 'pulse bad' : 'pulse'} style={{ fill: x.bad ? undefined : color }}>
          <animateMotion dur="1.1s" repeatCount="1" path={path} fill="freeze" />
        </circle>
      ))}
      {d.cut && (
        <EdgeLabelRenderer>
          <div className="ecut" style={{ transform: `translate(-50%,-50%) translate(${(pts[0].x + pts[pts.length - 1].x) / 2}px,${(pts[0].y + pts[pts.length - 1].y) / 2}px)` }}>
            <span>✕</span>{d.label ?? 'cut'}
          </div>
        </EdgeLabelRenderer>
      )}
      {((d.via?.length && d.badgeAt) || (d.showLabel && d.label)) && !d.fog && !d.cut ? (
        <EdgeLabelRenderer>
          <div className={d.via?.length ? (d.badgeLit ? 'ebadge lit' : 'ebadge') : 'elabel'} style={{ transform: `translate(${d.badgeAt === 'dst' ? '-100%' : d.badgeAt === 'src' ? '0' : '-50%'},-50%) translate(${mid.x}px,${mid.y}px)` }}
            title={d.via?.length ? `through the waypoint ${d.via.join(', ')}` : undefined}>
            {d.via?.length ? <><span className="shield">⛨</span>{d.via.join(' · ')}</> : d.label}
          </div>
        </EdgeLabelRenderer>
      ) : null}
    </>
  )
})

export const edgeTypes = { flow: FlowEdge }
