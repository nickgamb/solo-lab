import { useCallback, useEffect, useRef, useState } from 'react'

// Mirrors server/model.go.
export type Ref = { apiVersion: string; kind: string; namespace?: string; name: string }
export type Pod = { name: string; phase: string; ready: boolean; node?: string; zone?: string; ip?: string; restarts: number; started?: string }
export type Group = { id: string; label: string; domain?: string; order: number }
export type NodeKind =
  | 'gateway' | 'waypoint' | 'agent' | 'mcp' | 'idp' | 'db' | 'llm' | 'ui' | 'controller'
  | 'substrate' | 'workload' | 'external' | 'cluster' | 'tool'
export type LabNode = {
  id: string; kind: NodeKind; label: string; sub?: string; group: string; namespace?: string
  ref?: Ref; related?: Ref[]; pods?: Pod[]; badges?: string[]; products?: string[]; status: 'ok' | 'warn' | 'down' | 'idle'
  identity?: string[]; summary?: Record<string, unknown>
}
export type EdgeKind = 'http' | 'mcp' | 'a2a' | 'llm' | 'oidc' | 'mesh' | 'db' | 'telemetry' | 'control' | 'substrate'
export type LabEdge = { id: string; source: string; target: string; kind: EdgeKind; label?: string; declared: boolean; observed: boolean; of?: string[]; hosts?: string[] }
export type Graph = { version: number; groups: Group[]; nodes: LabNode[]; edges: LabEdge[] }
export type Traffic = {
  id: string; time: string; kind: string; reporter: string; source?: string; target?: string; via?: string
  identity?: string; user?: string; method?: string; path?: string; status?: number; durationMs?: number
  summary: string; outcome: 'ok' | 'denied' | 'error' | 'info'; attrs?: Record<string, string>; tokens?: Token[]
}
// a credential a request carried, as its claims (the token itself stays on the server)
export type Token = { source: string; verified: boolean; fingerprint?: string; header?: Record<string, unknown>; claims: Record<string, unknown> }
export type EdgeStats = { rps: number; errors: number; l4: number }
export type Stats = { windowSeconds: number; edges: Record<string, EdgeStats>; rps: number; errRate: number }
export type SubActor = {
  actorId: string; atespace: string; status: string; actorTemplateName: string; actorTemplateNamespace: string
  ateomPodName?: string; ateomPodNamespace?: string; workerPoolName?: string; latestSnapshot?: string
}
export type SubWorker = { workerNamespace: string; workerPool: string; workerPod: string; actorId?: string; actorTemplate?: string; ip?: string }
export type Substrate = {
  enabled: boolean; error?: string; time: string
  workerPools: { namespace: string; name: string; replicas: number }[]
  actorTemplates: { namespace: string; name: string; phase: string; harnessName?: string }[]
  actors: SubActor[]; workers: SubWorker[]
}
// Mirrors apps/continuity/api/v1alpha1 (IdentityContinuity).
// an IdP's attribute (path) paired with the broker's profile attribute
export type AttributeMapping = { attribute: string; path: string }
export type Directory = {
  type: 'scim' | 'auth0' | 'keycloak'; url: string; credentialsRef: { name: string }
  scopes?: string[]; audience?: string; clientID?: string
}
export const ATTRIBUTE_TYPES = ['string', 'integer', 'number', 'boolean', 'date', 'email', 'uri'] as const
export type AttributeType = typeof ATTRIBUTE_TYPES[number]
export type ProfileAttribute = { name: string; displayName?: string; type?: AttributeType; multivalued?: boolean }
export type Profile = { attributes?: ProfileAttribute[] }
export type Sync = { schedule: string; suspend?: boolean; credentialsRef?: { name: string } }
export type SyncStatus = {
  cronJob?: string; lastRun?: string; lastSuccess?: string; users?: number; updated?: number; written?: number; created?: number; failed?: number; message?: string
  schemas?: Record<string, string[]> // each IdP's attribute paths, as its directory last showed them
}
// the broker's built-in profile attributes: always in its profile
export const BUILTIN_ATTRIBUTES = ['username', 'email', 'firstName', 'lastName'] as const
// never synced: the broker's own name for the user
export const NEVER_SYNCED = ['username'] as const
export type Tier = {
  name: string; displayName?: string; type: 'oidc' | 'local'; enabled?: boolean; drain?: boolean
  oidc?: { issuer: string; clientID: string; clientAuth?: string; clientSecretRef?: { name: string; key?: string } }
  failoverWhen?: { unreachable?: boolean; serverError?: boolean; invalidDiscovery?: boolean; latencyAboveMs?: number }
  attributes?: AttributeMapping[]
  directory?: Directory
}
export type TierStatus = {
  name: string; type?: string; healthy?: boolean; configured?: boolean; partitioned?: boolean; latencyMs?: number
  lastProbe?: string; reason?: string; message?: string; redirectURI?: string; consecutiveFailures?: number; consecutiveSuccesses?: number
}
export type ContinuitySpec = {
  broker?: { keycloak?: { url?: string; realm?: string; credentialsRef?: { name: string } } }
  egress?: { namespace: string; waypoint: string }
  tiers: Tier[]
  health?: { intervalSeconds?: number; timeoutSeconds?: number; unhealthyThreshold?: number; healthyThreshold?: number }
  failback?: 'Automatic' | 'Manual'
  profile?: Profile
  sync?: Sync
}
export type IdentityContinuity = {
  metadata: { name: string; namespace: string; resourceVersion?: string }
  spec: ContinuitySpec
  status?: { active?: string; activeSince?: string; broker?: { issuer?: string }; tiers?: TierStatus[]; transitions?: { time: string; from: string; to: string; reason: string }[]; sync?: SyncStatus }
}
export type SignInPath = { instance: string; name: string; hosts: string[]; entry?: string; broker: string; app: string; after: string[][] }
export type ContinuityView = { items: IdentityContinuity[]; partitions: { tier: string; namespace: string; name: string; since?: string; by?: string; path?: string }[]; paths: SignInPath[] }
export type Me = { name: string; email?: string; groups: string[] }
// Mirrors server/models.go: the AI gateway's model chain.
// ollama (or another custom provider's override), openai, anthropic; other: a kind the editor doesn't write
export type ModelKind = 'openai' | 'anthropic' | 'ollama' | 'other' | (string & {})
export type ModelStats = { calls: number; errors: number; lastServed?: string; lastStatus?: number }
export type ModelProvider = {
  group: number; name: string; kind: ModelKind; model: string; host?: string; port?: number; secret?: string
  outage?: { host?: string; port?: number; by?: string; since?: string }; stats: ModelStats
}
export type FailoverRules = {
  on5xx: boolean; on429: boolean; condition: string; custom: boolean
  consecutiveFailures: number; duration: string; retryAttempts: number; retryCodes: number[]
}
export type ModelView = {
  edition: 'enterprise' | 'oss'
  backend: { namespace: string; name: string; resourceVersion: string; policies: string[] } | null
  gateway?: string
  external?: { name: string; hosts: string[] } // the route for callers outside the mesh (API keys)
  providers: ModelProvider[]
  rules: FailoverRules
  callers?: { namespace: string; serviceAccount: string; nodes?: string[] }[]
  declared?: { provider?: string; model?: string; fallback?: string }
  unattributed: ModelStats
  enterprise: { budgets: string[]; rateLimits: string[] }
}
// what a save writes: the providers in priority order, and the failover rules
export type ProviderIn = { name: string; kind: 'openai' | 'anthropic' | 'ollama'; model: string; host?: string; port?: number; secret?: string }
export type RulesIn = { on5xx: boolean; on429: boolean; consecutiveFailures: number; duration: string; retryAttempts: number }

const TRAFFIC_KEEP = 3000

export type Lab = {
  connected: boolean
  graph?: Graph
  stats?: Stats
  substrate?: Substrate
  continuity?: ContinuityView
  traffic: Traffic[]
  onTraffic: (fn: (t: Traffic) => void) => () => void
}

// useLab holds one server-sent-event stream for the whole app. The server
// replays the latest snapshot of each topic on connect, so a reconnect heals.
export function useLab(): Lab {
  const [connected, setConnected] = useState(false)
  const [graph, setGraph] = useState<Graph>()
  const [stats, setStats] = useState<Stats>()
  const [substrate, setSubstrate] = useState<Substrate>()
  const [continuity, setContinuity] = useState<ContinuityView>()
  const [traffic, setTraffic] = useState<Traffic[]>([])
  const listeners = useRef(new Set<(t: Traffic) => void>())
  const pending = useRef<Traffic[]>([])

  useEffect(() => {
    fetch('/api/traffic?limit=1000').then(r => (r.ok ? r.json() : [])).then((h: Traffic[]) => {
      setTraffic(prev => dedupe([...prev, ...(h ?? [])]))
    }).catch(() => {})
    const es = new EventSource('/api/stream')
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    es.addEventListener('graph', e => setGraph(JSON.parse((e as MessageEvent).data)))
    es.addEventListener('stats', e => setStats(JSON.parse((e as MessageEvent).data)))
    es.addEventListener('substrate', e => setSubstrate(JSON.parse((e as MessageEvent).data)))
    es.addEventListener('continuity', e => setContinuity(JSON.parse((e as MessageEvent).data)))
    es.addEventListener('traffic', e => {
      const t: Traffic = JSON.parse((e as MessageEvent).data)
      pending.current.push(t)
      listeners.current.forEach(fn => fn(t))
    })
    // batch list updates so a burst of requests doesn't re-render per event
    const flush = setInterval(() => {
      if (!pending.current.length) return
      const batch = pending.current.reverse()
      pending.current = []
      setTraffic(prev => dedupe([...batch, ...prev]))
    }, 250)
    return () => { es.close(); clearInterval(flush) }
  }, [])

  // stable, so subscribers don't resubscribe on every render
  const onTraffic = useCallback((fn: (t: Traffic) => void) => {
    listeners.current.add(fn)
    return () => { listeners.current.delete(fn) }
  }, [])
  return { connected, graph, stats, substrate, continuity, traffic, onTraffic }
}

// one row per id, newest first (times can differ in precision, so compare
// them as instants, not text)
function dedupe(ts: Traffic[]) {
  const seen = new Set<string>()
  return ts.filter(t => (seen.has(t.id) ? false : (seen.add(t.id), true)))
    .sort((a, b) => (Date.parse(b.time) || 0) - (Date.parse(a.time) || 0)).slice(0, TRAFFIC_KEEP)
}

// An API call that failed, with its HTTP status (409: a conflict to resolve).
export class ApiError extends Error {
  readonly status: number
  constructor(message: string, status: number) { super(message); this.status = status }
}

// Writes carry a JSON body unless the caller says otherwise (the server
// refuses writes without a JSON or YAML content type).
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body !== undefined && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const r = await fetch(path, { ...init, headers })
  const text = await r.text()
  if (!r.ok) throw new ApiError(text.trim() || r.statusText, r.status)
  const ct = r.headers.get('content-type') ?? ''
  return (ct.includes('json') ? JSON.parse(text) : text) as T
}

// Reads one live object for the editor, and whether the signed-in admin may
// change it (the server asks the API server; workloads, RBAC and Secrets
// are read-only here).
export async function getResource(qs: URLSearchParams): Promise<{ yaml: string; writable: boolean }> {
  const r = await fetch(`/api/resource?${qs}`)
  const text = await r.text()
  if (!r.ok) throw new ApiError(text.trim() || r.statusText, r.status)
  return { yaml: text, writable: r.headers.get('X-Writable') === 'true' }
}

// Writes an IdentityContinuity's spec. The resourceVersion it was read at
// makes the server refuse (409) if someone else changed it since; the
// result is the stored object, with its new resourceVersion.
export const putContinuity = (ns: string, name: string, resourceVersion: string | undefined, spec: ContinuitySpec) =>
  api<IdentityContinuity>(`/api/continuity/${ns}/${name}`, { method: 'PUT', body: JSON.stringify({ resourceVersion, spec }) })

// a write refused because the object changed since it was read
export const isConflict = (e: unknown) => e instanceof ApiError && e.status === 409

// Writes keys of a Secret in an IdentityContinuity's namespace. Write-only:
// nothing reads a Secret's values back.
export const putSecret = (ns: string, name: string, body: { key?: string; value?: string; data?: Record<string, string>; for?: string }) =>
  api(`/api/continuity/${ns}/secrets/${encodeURIComponent(name)}`, { method: 'PUT', body: JSON.stringify(body) })

export const refKey = (r: Ref) => `${r.apiVersion}/${r.kind}/${r.namespace ?? ''}/${r.name}`

export const getModels = () => api<ModelView>('/api/models')

// Rewrites the model chain, at the resourceVersion it was read at (409 if it
// changed since); the result is the chain as stored.
export const putModels = (ns: string, name: string, resourceVersion: string, providers: ProviderIn[], rules: RulesIn) =>
  api<ModelView>(`/api/models/${ns}/${name}`, { method: 'PUT', body: JSON.stringify({ resourceVersion, providers, rules }) })

// Writes a model provider's API key. Write-only: nothing reads it back.
export const putModelSecret = (ns: string, name: string, value: string) =>
  api(`/api/models/${ns}/secrets/${encodeURIComponent(name)}`, { method: 'PUT', body: JSON.stringify({ value }) })

// Cuts (a simulated outage) or restores one provider of the chain.
export const cutModel = (ns: string, name: string, provider: string, cut: boolean) =>
  api(`/api/models/${ns}/${name}/outage`, { method: 'POST', body: JSON.stringify({ provider, cut }) })
