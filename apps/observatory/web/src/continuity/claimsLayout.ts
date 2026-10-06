// Where the claims canvas puts things. The IdPs stack in chain order on the
// left (primary on top); the unified profile, the destination, is on the
// right and takes inputs only.

export const IDP_W = 240
export const UNI_W = 300
const GAP_X = 200
const GAP_Y = 36
const ROW = 27

// tier colours: the primary takes the accent, the rest the app's palette
const PALETTE = ['var(--accent)', 'var(--k-a2a)', 'var(--k-oidc)', 'var(--k-llm)', 'var(--k-substrate)', 'var(--k-mesh)', 'var(--info)']
export const tierColor = (k: number) => PALETTE[k % PALETTE.length]

export type Side = 'left' | 'right'
export const sideOf = (_k: number): Side => 'left'

// heights are estimates for stacking only; React Flow measures the real ones
const idpHeight = (rows: number) => 64 + (Math.max(rows, 1) + 1) * ROW + 62

export function idpPositions(rows: number[]): { x: number; y: number }[] {
  const y = { left: 0, right: 0 }
  return rows.map((n, k) => {
    const side = sideOf(k)
    const p = { x: side === 'left' ? -IDP_W - GAP_X : UNI_W + GAP_X, y: y[side] }
    y[side] += idpHeight(n) + GAP_Y
    return p
  })
}
