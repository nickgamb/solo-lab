import type { EdgeKind, Graph, LabNode } from '../api'
import type { MapEdge, Tray } from './mapview'

// The map is an architecture diagram read left to right, the way people
// draw one on a whiteboard:
//
//   - columns are stages of a call, shared by every zone: whatever calls
//     sits left of what it calls, so a column means the same thing in every
//     lane (people come in, apps, agents, gateways, tools and partners)
//   - lanes are zones (trust boundaries), one per party, stacked; a lane
//     spans only the stages its party takes part in
//   - every wire runs between neighbouring columns; a longer call passes
//     through the columns in between on its own track (a Sugiyama layout,
//     like a layered graph layout's), so wires never cross tiles
//   - nodes sit on a row grid, lined up with what they talk to, so most
//     wires are straight
//   - a worker pool is a tray around the agents it runs
//
// Everything is derived from the graph: another lab lays out the same way.

export type Pt = { x: number; y: number }
export type Box = { x: number; y: number; w: number; h: number }
export type Wire = { id: string; source: string; target: string; kind: EdgeKind; of: string[]; ends: string[]; via: string[]; points?: Pt[] }
export type Column = { x: number; w: number; label: string }
export type Placed = {
  nodes: Record<string, Box> // the tile box, absolute
  groups: Record<string, Box>
  trays: Record<string, Box>
  columns: Column[]
  door?: string // the front door's node id, drawn as a pillar
  top: number // where the column headings sit
  wires: Wire[]
}

export const TILE = 84, HERO = 112, LABEL_W = 180, LABEL_H = 44, CAPTION_H = 22  // a tile's second sub-line
export const OUTSIDE = 'outside'

export const rootLevel = (n: LabNode) => n.kind === 'external' || n.kind === 'llm'
export const hero = (n: LabNode) => n.kind === 'gateway' && !!n.products?.includes('agentgateway')
export const tileSize = (n: LabNode) => (hero(n) ? { w: HERO, h: HERO } : { w: TILE, h: TILE })

const GUTTER = 190 // between columns, where wires curve
const COL_W = LABEL_W + GUTTER
const ROW_H = 158
const BAR_H = 70 // the zone's status bar
const OUT_H = 52 // the outside lane's heading
const PAD_X = 40, PAD_B = 14, LANE_GAP = 40
const MIN_W = 1040 // room for the status bar
const TRAY_PAD = 14, TRAY_HEAD = 30

const STAGE: Record<string, string> = {
  ui: 'Apps', gateway: 'Gateways', waypoint: 'Gateways', agent: 'Agents', controller: 'Control', mcp: 'Tools', tool: 'Tools',
  workload: 'Services', db: 'Data', idp: 'Identity', llm: 'Models', external: 'External', substrate: 'Workers',
}

const PRIORITY = ['Agents', 'Apps', 'Gateways', 'Tools', 'Models', 'Identity', 'Control', 'Workers', 'External', 'Services', 'Data']

type Item = { id: string; lane: string; col: number; dummy?: boolean }

export async function layout(g: Graph, nodes: LabNode[], edges: MapEdge[], trays: Record<string, Tray> = {}): Promise<Placed> {
  const byId = new Map(nodes.map(n => [n.id, n]))
  const laneOf = (id: string) => { const n = byId.get(id)!; return rootLevel(n) ? OUTSIDE : n.group }
  const all = edges.filter(e => byId.has(e.source) && byId.has(e.target) && e.source !== e.target)
  // the front door: the internet-facing gateway, drawn as a pillar down the
  // left of every lane, so each zone's way in is a straight line from it
  const entry = nodes.find(n => n.kind === 'gateway' && n.products?.includes('kgateway') && !rootLevel(n))
  const door = entry && all.some(e => e.source === entry.id) ? entry.id : undefined
  const doorEs = all.filter(e => e.source === door)
  const es = all.filter(e => e.source !== door && e.target !== door)
  const grid = nodes.filter(n => n.id !== door)
  const trayOf = new Map<string, string>()
  for (const [t, tr] of Object.entries(trays)) for (const m of tr.members) if (byId.has(m)) trayOf.set(m, t)

  // 1. break cycles: a call back to something already on the path is laid
  // out as if it went the other way
  const out = new Map<string, MapEdge[]>()
  for (const e of es) push(out, e.source, e)
  const flipped = new Set<string>(), state = new Map<string, number>()
  const visit = (id: string) => {
    state.set(id, 1)
    for (const e of out.get(id) ?? []) {
      const s = state.get(e.target)
      if (s === 1) flipped.add(e.id)
      else if (!s) visit(e.target)
    }
    state.set(id, 2)
  }
  // walk from the zone that starts the most calls into others, so a call
  // back (a callback, a registration) is what gets turned, not the story
  const indeg = (id: string) => es.filter(e => e.target === id).length
  const reachOut = new Map<string, number>()
  for (const e of es) if (laneOf(e.source) !== laneOf(e.target)) reachOut.set(laneOf(e.source), (reachOut.get(laneOf(e.source)) ?? 0) + 1)
  const lead = (id: string) => reachOut.get(laneOf(id)) ?? 0
  for (const n of [...grid].sort((a, b) => lead(b.id) - lead(a.id) || indeg(a.id) - indeg(b.id) || a.id.localeCompare(b.id))) if (!state.get(n.id)) visit(n.id)
  const dag = es.map(e => (flipped.has(e.id) ? { e, from: e.target, to: e.source } : { e, from: e.source, to: e.target }))
  const preds = new Map<string, string[]>(), succs = new Map<string, string[]>()
  for (const d of dag) { push(preds, d.to, d.from); push(succs, d.from, d.to) }

  // 2. columns: longest path from where calls start; a pure source sits
  // just left of the first thing it calls
  const col = new Map<string, number>()
  const colOf = (id: string): number => {
    if (col.has(id)) return col.get(id)!
    col.set(id, 0)
    const c = Math.max(0, ...(preds.get(id) ?? []).map(p => colOf(p) + 1))
    col.set(id, c)
    return c
  }
  for (const n of grid) colOf(n.id)
  for (const n of grid) {
    if (preds.get(n.id)?.length || !succs.get(n.id)?.length) continue
    col.set(n.id, Math.max(0, Math.min(...succs.get(n.id)!.map(t => col.get(t)!)) - 1))
  }

  // 3. long calls get a track through each column they pass, in the lane
  // they're headed for (so a call drops into its lane first, then runs
  // along it)
  // what leaves the lab (model providers, external services) sits in one
  // column past everything else, level with what calls it: in on the left
  // through the front door, out on the right
  const exits = new Set(grid.filter(n => laneOf(n.id) === OUTSIDE && !succs.get(n.id)?.length).map(n => n.id))
  const lastIn = Math.max(0, ...grid.filter(n => !exits.has(n.id)).map(n => col.get(n.id)!))
  for (const id of exits) col.set(id, lastIn + 1)
  const items = new Map<string, Item>()
  for (const n of grid) if (!exits.has(n.id)) items.set(n.id, { id: n.id, lane: laneOf(n.id), col: col.get(n.id)! })
  type Chain = { e: MapEdge; ids: string[]; flipped: boolean }
  const chains: Chain[] = []
  const nb = new Map<string, Set<string>>() // neighbours on the layered graph
  const link = (a: string, b: string) => { (nb.get(a) ?? nb.set(a, new Set()).get(a)!).add(b); (nb.get(b) ?? nb.set(b, new Set()).get(b)!).add(a) }
  for (const d of dag) {
    const c0 = col.get(d.from)!, c1 = col.get(d.to)!
    const ids = [d.from]
    const lane = exits.has(d.to) ? laneOf(d.from) : laneOf(d.e.target)
    for (let c = c0 + 1; c < c1; c++) {
      const id = `~${d.e.id}~${c}`
      items.set(id, { id, lane, col: c, dummy: true })
      ids.push(id)
    }
    ids.push(d.to)
    for (let i = 1; i < ids.length; i++) if (!exits.has(ids[i])) link(ids[i - 1], ids[i])
    chains.push({ e: d.e, ids, flipped: flipped.has(d.e.id) })
  }
  for (const e of doorEs) {
    const ids = [e.source]
    for (let c = 0; c < col.get(e.target)!; c++) {
      const id = `~${e.id}~${c}`
      items.set(id, { id, lane: laneOf(e.target), col: c, dummy: true })
      ids.push(id)
    }
    ids.push(e.target)
    for (let i = 2; i < ids.length; i++) link(ids[i - 1], ids[i])
    chains.push({ e, ids, flipped: false })
  }

  // 4. lane order: the lane with the way in on top, then the order that
  // keeps calls between lanes shortest (a declared order wins), outside last
  const declared = new Map(g.groups.map(x => [x.id, x.order]))
  const laneIds = [...new Set([...items.values()].map(i => i.lane))]
  const first = entry && laneIds.includes(laneOf(entry.id)) ? laneOf(entry.id) : undefined
  const cross = new Map<string, number>()
  const outs = new Map<string, number>(), ins = new Map<string, number>()
  for (const d of dag) {
    const a = laneOf(d.e.source), b = laneOf(d.e.target)
    if (a === b) continue
    const k = [a, b].sort().join('|'); cross.set(k, (cross.get(k) ?? 0) + 1)
    outs.set(a, (outs.get(a) ?? 0) + 1); ins.set(b, (ins.get(b) ?? 0) + 1)
  }
  // the zone that starts the most calls into others comes next: the story starts there
  const size = (l: string) => grid.filter(n => laneOf(n.id) === l).length
  const origin = laneIds.filter(l => l !== first && l !== OUTSIDE)
    .sort((a, b) => 2 * (outs.get(b) ?? 0) + size(b) - (ins.get(b) ?? 0) - (2 * (outs.get(a) ?? 0) + size(a) - (ins.get(a) ?? 0)))[0]
  const middle = laneIds.filter(l => l !== first && l !== OUTSIDE && l !== origin)
  const cost = (order: string[]) => {
    let c = 0
    for (const [k, w] of cross) { const [a, b] = k.split('|'); const i = order.indexOf(a), j = order.indexOf(b); if (i >= 0 && j >= 0) c += w * Math.abs(i - j) }
    return c
  }
  const meanCol = (l: string) => { const cs = grid.filter(n => laneOf(n.id) === l).map(n => col.get(n.id)!); return cs.reduce((a, b) => a + b, 0) / Math.max(1, cs.length) }
  const pre = [first, origin].filter(Boolean) as string[], post = laneIds.includes(OUTSIDE) ? [OUTSIDE] : []
  const tie = (a: string, b: string) => (declared.get(a) ?? 1000) - (declared.get(b) ?? 1000) || meanCol(a) - meanCol(b) || a.localeCompare(b)
  let lanes = [...pre, ...[...middle].sort(tie), ...post]
  if (middle.length <= 7) {
    let best = cost(lanes)
    for (const p of permutations([...middle].sort(tie))) {
      const cand = [...pre, ...p, ...post]
      const c = cost(cand)
      if (c < best) { best = c; lanes = cand }
    }
  }
  const laneIdx = new Map(lanes.map((l, i) => [l, i]))

  // 5. order within each (lane, column): barycentre sweeps, keeping a tray's
  // agents together, and the order with the fewest crossings wins
  const cols = Math.max(0, ...[...items.values()].map(i => i.col)) + 1 // lanes' stages (exits come after)
  const cells = new Map<string, string[]>() // `${lane}|${col}`
  const cellKey = (it: Item) => `${it.lane}|${it.col}`
  const KIND: Record<string, number> = { gateway: 0, ui: 1, controller: 2, agent: 3, workload: 4, mcp: 5, waypoint: 6, idp: 7, db: 8, llm: 9 }
  for (const it of [...items.values()].sort((a, b) => (KIND[byId.get(a.id)?.kind ?? ''] ?? 5) - (KIND[byId.get(b.id)?.kind ?? ''] ?? 5) || a.id.localeCompare(b.id))) {
    push(cells, cellKey(it), it.id)
  }
  const rank = new Map<string, number>()
  const rerank = () => { for (const [k, ids] of cells) { const li = laneIdx.get(k.split('|')[0])!; ids.forEach((id, i) => rank.set(id, li * 1000 + i)) } }
  rerank()
  const byCol = (c: number) => [...cells.entries()].filter(([k]) => Number(k.split('|')[1]) === c)
  const sortCell = (ids: string[], side: 'left' | 'right' | 'both') => {
    const bary = new Map<string, number>()
    for (const id of ids) {
      const it = items.get(id)!
      const ns = [...(nb.get(id) ?? [])].filter(o => side === 'both' || (side === 'left' ? items.get(o)!.col < it.col : items.get(o)!.col > it.col))
      bary.set(id, ns.length ? ns.reduce((a, o) => a + rank.get(o)!, 0) / ns.length : rank.get(id)!)
    }
    const blockOf = (id: string) => trayOf.get(id) ?? id
    const blocks = new Map<string, string[]>()
    for (const id of ids) push(blocks, blockOf(id), id)
    const bb = (b: string) => { const m = blocks.get(b)!; return m.reduce((a, id) => a + bary.get(id)!, 0) / m.length }
    return [...blocks.keys()].sort((a, b) => bb(a) - bb(b)).flatMap(b => blocks.get(b)!.sort((x, y) => bary.get(x)! - bary.get(y)!))
  }
  const crossings = () => {
    let n = 0
    for (let c = 0; c + 1 < cols; c++) {
      const segs: [number, number][] = []
      for (const [, ids] of byCol(c)) for (const id of ids) for (const o of nb.get(id) ?? []) if (items.get(o)!.col === c + 1) segs.push([rank.get(id)!, rank.get(o)!])
      for (let i = 0; i < segs.length; i++) for (let j = i + 1; j < segs.length; j++) if ((segs[i][0] - segs[j][0]) * (segs[i][1] - segs[j][1]) < 0) n++
    }
    return n
  }
  let best = crossings(), bestCells = new Map([...cells].map(([k, v]) => [k, [...v]]))
  for (let it = 0; it < 16; it++) {
    const down = it % 2 === 0
    for (let k = 0; k < cols; k++) {
      const c = down ? k : cols - 1 - k
      for (const [key, ids] of byCol(c)) cells.set(key, sortCell(ids, it > 12 ? 'both' : down ? 'left' : 'right'))
      rerank()
    }
    const n = crossings()
    if (n < best) { best = n; bestCells = new Map([...cells].map(([k, v]) => [k, [...v]])) }
  }
  for (const [k, v] of bestCells) cells.set(k, v)
  rerank()

  // 6. rows: each lane is a grid; a cell's nodes keep their order but slide
  // to line up with what they talk to in the same lane
  const rowOf = new Map<string, number>()
  const laneRows = new Map<string, number>()
  for (const l of lanes) {
    const R = Math.max(1, ...[...cells].filter(([k]) => k.split('|')[0] === l).map(([, ids]) => ids.length))
    laneRows.set(l, R)
    for (const [k, ids] of cells) if (k.split('|')[0] === l) { const off = Math.floor((R - ids.length) / 2); ids.forEach((id, i) => rowOf.set(id, off + i)) }
  }
  for (let pass = 0; pass < 6; pass++) {
    const down = pass % 2 === 0
    for (let k = 0; k < cols; k++) {
      const c = down ? k : cols - 1 - k
      for (const [key, ids] of byCol(c)) {
        const lane = key.split('|')[0], R = laneRows.get(lane)!
        const want = ids.map(id => {
          const ns = [...(nb.get(id) ?? [])].filter(o => items.get(o)!.lane === lane)
          return ns.length ? ns.reduce((a, o) => a + rowOf.get(o)!, 0) / ns.length : rowOf.get(id)!
        })
        assignRows(ids, want, R, ids.map(id => trayOf.get(id))).forEach((r, i) => rowOf.set(ids[i], r))
      }
    }
  }
  // drop rows nobody uses
  for (const l of lanes) {
    const used = [...new Set([...items.values()].filter(i => i.lane === l).map(i => rowOf.get(i.id)!))].sort((a, b) => a - b)
    const remap = new Map(used.map((r, i) => [r, i]))
    for (const it of items.values()) if (it.lane === l) rowOf.set(it.id, remap.get(rowOf.get(it.id)!)!)
    laneRows.set(l, used.length)
  }

  // 7. coordinates
  const placed: Placed = { nodes: {}, groups: {}, trays: {}, columns: [], top: 0, wires: [] }
  const cx = (c: number) => (c + (door ? 1 : 0)) * COL_W + LABEL_W / 2
  // rows are ROW_H apart, with room above a row where a tray opens and
  // below one where a tray closes
  const laneTop = new Map<string, number>()
  const head = (l: string) => (l === OUTSIDE ? OUT_H : BAR_H)
  const rowTop = new Map<string, number[]>() // lane -> y of each row, from the lane's top
  for (const l of lanes) {
    const R = laneRows.get(l)!
    const opens = new Array(R).fill(false), closes = new Array(R).fill(false)
    for (const [k, ids] of cells) {
      if (k.split('|')[0] !== l) continue
      ids.forEach((id, i) => {
        const t = trayOf.get(id)
        if (!t) return
        if (trayOf.get(ids[i - 1]) !== t) opens[rowOf.get(id)!] = true
        if (trayOf.get(ids[i + 1]) !== t) closes[rowOf.get(id)!] = true
      })
    }
    const ys: number[] = []
    let yy = head(l)
    for (let r = 0; r < R; r++) {
      if (opens[r]) yy += TRAY_HEAD + TRAY_PAD
      ys.push(yy)
      yy += ROW_H
      if (closes[r]) yy += TRAY_PAD
    }
    ys.push(yy)
    rowTop.set(l, ys)
  }
  let y = 0
  for (const l of lanes) {
    laneTop.set(l, y)
    // every lane spans every stage, so the zones line up
    const h = rowTop.get(l)![laneRows.get(l)!] + PAD_B
    const x0 = cx(0) - LABEL_W / 2 - PAD_X
    const w = Math.max(cx(cols - 1) + LABEL_W / 2 + PAD_X - x0, MIN_W)
    placed.groups[l] = { x: x0, y, w, h }
    y += h + LANE_GAP
  }
  // a tile's centre: its icon, where wires meet it (the label hangs below)
  const centreY = (id: string) => {
    const l = items.get(id)!.lane
    return laneTop.get(l)! + rowTop.get(l)![rowOf.get(id)!] + (ROW_H - LABEL_H) / 2 + 4
  }
  for (const n of grid) {
    if (exits.has(n.id)) continue
    const s = tileSize(n)
    placed.nodes[n.id] = { x: cx(col.get(n.id)!) - s.w / 2, y: centreY(n.id) - s.h / 2, w: s.w, h: s.h }
  }
  for (const [t, tr] of Object.entries(trays)) {
    const ms = tr.members.filter(m => placed.nodes[m])
    if (!ms.length) continue
    const bs = ms.map(m => ({ x0: cx(col.get(m)!) - LABEL_W / 2, x1: cx(col.get(m)!) + LABEL_W / 2, y0: placed.nodes[m].y, y1: placed.nodes[m].y + placed.nodes[m].h + LABEL_H }))
    const x0 = Math.min(...bs.map(b => b.x0)) - TRAY_PAD, x1 = Math.max(...bs.map(b => b.x1)) + TRAY_PAD
    const y0 = Math.min(...bs.map(b => b.y0)) - TRAY_PAD - TRAY_HEAD, y1 = Math.max(...bs.map(b => b.y1)) + TRAY_PAD
    placed.trays[t] = { x: x0, y: y0, w: x1 - x0, h: y1 - y0 }
  }

  // exits: level with the calls arriving, spread so they don't overlap
  const ls = lanes.filter(l => l !== OUTSIDE)
  if (exits.size && ls.length) {
    const want = [...exits].map(id => {
      const ys = chains.filter(ch => ch.ids[ch.ids.length - 1] === id).map(ch => centreY(ch.ids[ch.ids.length - 2]))
      return { id, y: ys.length ? ys.reduce((a, b) => a + b, 0) / ys.length : 0 }
    }).sort((a, b) => a.y - b.y || a.id.localeCompare(b.id))
    let last = -Infinity
    for (const w of want) { w.y = Math.max(w.y, last + ROW_H); last = w.y }
    const top = placed.groups[ls[0]].y, bottom = placed.groups[ls[ls.length - 1]].y + placed.groups[ls[ls.length - 1]].h
    for (const w of want) {
      const s = tileSize(byId.get(w.id)!)
      placed.nodes[w.id] = { x: cx(cols) - s.w / 2, y: w.y - s.h / 2, w: s.w, h: s.h }
    }
    delete placed.groups[OUTSIDE]
    const x0 = cx(cols) - LABEL_W / 2 - PAD_X
    placed.groups[OUTSIDE] = { x: x0, y: top, w: LABEL_W + 2 * PAD_X, h: Math.max(bottom, last + ROW_H) - top }
    placed.columns.push({ x: cx(cols) - LABEL_W / 2, w: LABEL_W, label: 'Outside the lab' })
  }

  if (door) {
    const y0 = placed.groups[ls[0]].y, last = placed.groups[ls[ls.length - 1]]
    placed.nodes[door] = { x: cx(-1) - TILE / 2, y: y0, w: TILE, h: last.y + last.h - y0 }
    placed.door = door
    placed.columns.push({ x: cx(-1) - LABEL_W / 2, w: LABEL_W, label: 'Front door' })
  }

  // column headings: what each stage mostly holds
  for (let c = 0; c < cols; c++) {
    const count = new Map<string, number>()
    for (const n of grid) if (col.get(n.id) === c && !exits.has(n.id)) { const s = STAGE[n.kind] ?? 'Services'; count.set(s, (count.get(s) ?? 0) + 1) }
    // the headline stages first; plumbing (services, data) only when it's all there is
    const named = [...count].sort((a, b) => PRIORITY.indexOf(a[0]) - PRIORITY.indexOf(b[0]) || b[1] - a[1]).map(([x]) => x)
    const lead = named.filter(x => x !== 'Services' && x !== 'Data')
    const label = (lead.length ? lead : named).slice(0, 2).join(' · ')
    placed.columns.push({ x: cx(c) - LABEL_W / 2, w: LABEL_W, label })
  }
  placed.top = -64

  // 8. wires: right side of the caller to left side of the callee, straight
  // through the columns in between
  const right = (id: string) => { const b = placed.nodes[id]; return { x: b.x + b.w, y: b.y + b.h / 2 } }
  const left = (id: string) => { const b = placed.nodes[id]; return { x: b.x, y: b.y + b.h / 2 } }
  for (const ch of chains) {
    const pts: Pt[] = []
    for (const d of ch.ids.slice(1, -1)) {
      const it = items.get(d)!, yy = centreY(d)
      pts.push({ x: cx(it.col) - LABEL_W / 2, y: yy }, { x: cx(it.col) + LABEL_W / 2, y: yy })
    }
    // a flipped call is drawn the way it goes: from its caller, back along the track
    const e = ch.e
    const start = e.source === door ? { x: placed.nodes[door].x + TILE, y: (pts[0] ?? left(e.target)).y } : right(e.source)
    const path = ch.flipped ? [left(e.source), ...pts.reverse(), right(e.target)] : [start, ...pts, left(e.target)]
    placed.wires.push({ id: e.id, source: e.source, target: e.target, kind: e.kind, of: e.of, ends: [e.source, e.target], via: e.via, points: path })
  }
  return placed
}

// assignRows keeps the cell's order and gives each node (a tray's agents as
// one block) the rows closest to where it wants to be.
function assignRows(ids: string[], want: number[], R: number, tray: (string | undefined)[]): number[] {
  type U = { idx: number[] }
  const units: U[] = []
  ids.forEach((_, i) => {
    const last = units[units.length - 1]
    if (last && tray[i] && tray[last.idx[0]] === tray[i]) last.idx.push(i)
    else units.push({ idx: [i] })
  })
  const rows = Math.max(R, ids.length)
  const INF = 1e9
  // dp[u][r]: the best cost with unit u starting at row r
  const dp = units.map(() => new Array<number>(rows).fill(INF)), from = units.map(() => new Array<number>(rows).fill(-1))
  const unitCost = (u: U, r: number) => u.idx.reduce((a, i, j) => a + Math.abs(r + j - want[i]), 0)
  for (let u = 0; u < units.length; u++) {
    const size = units[u].idx.length
    for (let r = 0; r + size <= rows; r++) {
      const c = unitCost(units[u], r)
      if (u === 0) { dp[u][r] = c; continue }
      const ps = units[u - 1].idx.length
      for (let q = 0; q + ps <= r; q++) if (dp[u - 1][q] + c < dp[u][r]) { dp[u][r] = dp[u - 1][q] + c; from[u][r] = q }
    }
  }
  const last = units.length - 1
  let r = 0
  for (let q = 0; q < rows; q++) if (dp[last][q] < dp[last][r]) r = q
  const out = new Array<number>(ids.length)
  for (let u = last; u >= 0; u--) { units[u].idx.forEach((i, j) => (out[i] = r + j)); r = from[u][r] }
  return out
}

function* permutations<T>(xs: T[]): Generator<T[]> {
  if (xs.length <= 1) { yield xs; return }
  for (let i = 0; i < xs.length; i++) for (const p of permutations([...xs.slice(0, i), ...xs.slice(i + 1)])) yield [xs[i], ...p]
}

function push<K, V>(m: Map<K, V[]>, k: K, v: V) {
  const l = m.get(k)
  if (l) l.push(v); else m.set(k, [v])
}

// spline: a smooth curve through the points, level at each one, so a wire
// leaves and arrives horizontally and runs straight along its track.
export function spline(pts: Pt[]): string {
  if (pts.length < 2) return ''
  let d = `M ${pts[0].x} ${pts[0].y}`
  for (let i = 1; i < pts.length; i++) {
    const p = pts[i - 1], q = pts[i]
    if (Math.abs(p.y - q.y) < 0.5) { d += ` L ${q.x} ${q.y}`; continue }
    const mx = (p.x + q.x) / 2
    d += ` C ${mx} ${p.y} ${mx} ${q.y} ${q.x} ${q.y}`
  }
  return d
}
