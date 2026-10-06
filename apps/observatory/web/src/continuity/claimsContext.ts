import { createContext, useContext } from 'react'
import type { Directory } from '../api'

// What the canvas's nodes and edges can do to the working copy. Passed by
// context rather than in node data, so node data stays plain values.
export type ClaimsActions = {
  addClaim: (tier: string, claim: string) => void
  dropClaim: (tier: string, claim: string) => void
  setDirectory: (tier: string, d: Directory | undefined) => void
  stageCredentials: (tier: string, id: string, secret: string) => void
  writeCredentials: (tier: string, id: string, secret: string) => Promise<void>
  addAttribute: (name: string) => string | undefined
  removeAttribute: (name: string) => void
  toggleMultivalued: (name: string) => void
  removeMapping: (tier: string, attribute: string) => void
  setPath: (tier: string, attribute: string, path: string) => void
  openEdge: (id: string | undefined) => void
  setTokenClients: (clients: string[]) => void
  directorySaved: (tier: string) => boolean
  testDirectory: (tier: string) => Promise<{ ok: boolean; text: string }>
}

export const ClaimsContext = createContext<ClaimsActions | null>(null)

export function useClaims(): ClaimsActions {
  const c = useContext(ClaimsContext)
  if (!c) throw new Error('useClaims outside ClaimsContext')
  return c
}
