// Where the directory sync canvas puts things: the IdPs stacked in chain
// order on the left (the primary on top), each wired into the same inputs of
// S&V's profile on the right. The mapping pairs attributes; which way a value
// moves is the IdP's role (primary: read into the profile; failover: written
// from it).

export const IDP_W = 260
export const PROFILE_W = 280
const GAP_X = 200
const GAP_Y = 36
const ROW = 27

// IdP colours: the primary takes the accent, the rest the app's palette
const PALETTE = ['var(--accent)', 'var(--k-a2a)', 'var(--k-oidc)', 'var(--k-llm)', 'var(--k-substrate)', 'var(--k-mesh)', 'var(--info)']
export const idpColor = (k: number) => PALETTE[k % PALETTE.length]

// heights are estimates for stacking only; React Flow measures the real ones
const idpHeight = (rows: number) => 64 + (Math.max(rows, 1) + 1) * ROW + 110

export function idpPositions(rows: number[]): { x: number; y: number }[] {
  let y = 0
  return rows.map(n => {
    const p = { x: -IDP_W - GAP_X, y }
    y += idpHeight(n) + GAP_Y
    return p
  })
}
