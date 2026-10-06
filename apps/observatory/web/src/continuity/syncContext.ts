import { createContext, useContext } from 'react'
import type { Directory } from '../api'

export type TestResult = { ok: boolean; text: string; attributes?: string[] }

// What the canvas's nodes and edges can do to the working copy. Passed by
// context rather than in node data, so node data stays plain values.
export type SyncActions = {
  addPath: (idp: string, path: string) => void
  dropPath: (idp: string, path: string) => void
  setDirectory: (idp: string, d: Directory | undefined) => void
  stageCredentials: (idp: string, id: string, secret: string) => void
  writeCredentials: (idp: string, id: string, secret: string) => Promise<void>
  addAttribute: (name: string) => string | undefined
  removeAttribute: (name: string) => void
  toggleMultivalued: (name: string) => void
  removeMapping: (idp: string, attribute: string) => void
  directorySaved: (idp: string) => boolean
  testDirectory: (idp: string) => Promise<TestResult>
}

export const SyncContext = createContext<SyncActions | null>(null)

export function useSync(): SyncActions {
  const c = useContext(SyncContext)
  if (!c) throw new Error('useSync outside SyncContext')
  return c
}
