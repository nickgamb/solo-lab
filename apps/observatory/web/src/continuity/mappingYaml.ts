import { parse, stringify } from 'yaml'
import type { ClaimMapping, ContinuitySpec, Directory, Profile, ProfileAttribute, Sync, Tier } from '../api'
import { cronError } from './cron'
import { ATTR_NAME, credentialsName, isBuiltin, withProfile, withSync } from './mapping'

// The Code tab's view of a spec: the profile, the sync schedule, and each
// OIDC tier's claim mappings and directory. Tiers are keyed by name; the
// rest of the spec is edited elsewhere.

export function toYaml(spec: ContinuitySpec): string {
  const doc = {
    profile: { attributes: spec.profile?.attributes ?? [], tokenClients: spec.profile?.tokenClients ?? [] },
    sync: spec.sync ? { schedule: spec.sync.schedule, suspend: spec.sync.suspend ?? false, credentialsRef: spec.sync.credentialsRef } : undefined,
    tiers: spec.tiers.filter(t => t.type === 'oidc').map(t => ({ name: t.name, claims: t.claims ?? [], directory: t.directory })),
  }
  return stringify(doc, { lineWidth: 0 })
}

type Obj = Record<string, unknown>

function obj(v: unknown, at: string, keys: string[]): Obj {
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${at}: expected a mapping`)
  const extra = Object.keys(v).filter(k => !keys.includes(k))
  if (extra.length) throw new Error(`${at}: unknown field ${extra.join(', ')} (expected ${keys.join(', ')})`)
  return v as Obj
}
function arr(v: unknown, at: string): unknown[] {
  if (v == null) return []
  if (!Array.isArray(v)) throw new Error(`${at}: expected a list`)
  return v
}
function str(v: unknown, at: string, required = true): string | undefined {
  if (v == null || v === '') { if (required) throw new Error(`${at}: required`); return undefined }
  if (typeof v !== 'string') throw new Error(`${at}: expected a string`)
  return v
}
function bool(v: unknown, at: string): boolean | undefined {
  if (v == null) return undefined
  if (typeof v !== 'boolean') throw new Error(`${at}: expected true or false`)
  return v
}

function readProfile(v: unknown): Profile {
  if (v == null) return {}
  const p = obj(v, 'profile', ['attributes', 'tokenClients'])
  const seen = new Set<string>()
  const attributes = arr(p.attributes, 'profile.attributes').map((x, i): ProfileAttribute => {
    const at = `profile.attributes[${i}]`
    const a = obj(x, at, ['name', 'displayName', 'multivalued'])
    const name = str(a.name, `${at}.name`)!
    if (!ATTR_NAME.test(name)) throw new Error(`${at}.name: "${name}" must start with a letter; letters, digits, _ . - only`)
    if (isBuiltin(name)) throw new Error(`${at}.name: "${name}" is built in; it is always in the profile`)
    if (seen.has(name)) throw new Error(`${at}.name: "${name}" appears twice`)
    seen.add(name)
    const out: ProfileAttribute = { name }
    const displayName = str(a.displayName, `${at}.displayName`, false)
    const multivalued = bool(a.multivalued, `${at}.multivalued`)
    if (displayName !== undefined) out.displayName = displayName
    if (multivalued !== undefined) out.multivalued = multivalued
    return out
  })
  const tokenClients = arr(p.tokenClients, 'profile.tokenClients').map((x, i) => str(x, `profile.tokenClients[${i}]`)!)
  return { attributes, tokenClients }
}

function readSync(v: unknown, prev?: Sync): Sync | undefined {
  if (v == null) return undefined
  const s = obj(v, 'sync', ['schedule', 'suspend', 'credentialsRef'])
  const schedule = str(s.schedule, 'sync.schedule')!
  const bad = cronError(schedule)
  if (bad) throw new Error(`sync.schedule: ${bad}`)
  const suspend = bool(s.suspend, 'sync.suspend')
  // an explicit "suspend: false" survives only if the spec already had it
  const out: Sync = suspend ? { schedule, suspend: true } : prev?.suspend === false ? { schedule, suspend: false } : { schedule }
  if (s.credentialsRef != null) {
    const ref = obj(s.credentialsRef, 'sync.credentialsRef', ['name'])
    out.credentialsRef = { name: str(ref.name, 'sync.credentialsRef.name')! }
  }
  return out
}

function readDirectory(v: unknown, at: string, tier: string): Directory | undefined {
  if (v == null) return undefined
  const d = obj(v, at, ['type', 'url', 'credentialsRef', 'clientID', 'scopes', 'audience'])
  const type = str(d.type, `${at}.type`)
  if (type !== 'scim' && type !== 'auth0' && type !== 'keycloak') throw new Error(`${at}.type: one of scim, auth0, keycloak`)
  const url = str(d.url, `${at}.url`)!
  if (!/^https?:\/\//.test(url)) throw new Error(`${at}.url: expected an http(s) URL`)
  const ref = d.credentialsRef == null ? undefined : obj(d.credentialsRef, `${at}.credentialsRef`, ['name'])
  const out: Directory = { type, url, credentialsRef: { name: str(ref?.name, `${at}.credentialsRef.name`, false) ?? credentialsName(tier) } }
  const scopes = arr(d.scopes, `${at}.scopes`).map((x, i) => str(x, `${at}.scopes[${i}]`)!)
  const audience = str(d.audience, `${at}.audience`, false)
  const clientID = str(d.clientID, `${at}.clientID`, false)
  if (scopes.length) out.scopes = scopes
  if (clientID !== undefined) out.clientID = clientID
  if (audience !== undefined) out.audience = audience
  return out
}

// applyYaml returns the spec with the document's profile, sync and named
// tiers' claims and directories applied, or throws a message naming the
// offending field.
export function applyYaml(spec: ContinuitySpec, text: string): ContinuitySpec {
  let doc: unknown
  try { doc = parse(text) } catch (e) { throw new Error(`YAML: ${(e as Error).message}`) }
  const d = obj(doc ?? {}, 'document', ['profile', 'sync', 'tiers'])
  const profile = readProfile(d.profile)
  const attrs = new Set((profile.attributes ?? []).map(a => a.name))
  let next = withSync(withProfile(spec, profile), readSync(d.sync, spec.sync))
  const oidc = new Set(spec.tiers.filter(t => t.type === 'oidc').map(t => t.name))
  const named = new Set<string>()
  arr(d.tiers, 'tiers').forEach((x, i) => {
    const at = `tiers[${i}]`
    const t = obj(x, at, ['name', 'claims', 'directory'])
    const name = str(t.name, `${at}.name`)!
    if (!oidc.has(name)) throw new Error(`${at}.name: no OIDC tier "${name}" (tiers are added in the rule builder)`)
    if (named.has(name)) throw new Error(`${at}.name: "${name}" appears twice`)
    named.add(name)
    const mapped = new Set<string>()
    const claims = arr(t.claims, `${at}.claims`).map((y, j): ClaimMapping => {
      const cat = `${at}.claims[${j}]`
      const c = obj(y, cat, ['claim', 'attribute', 'directoryPath'])
      const claim = str(c.claim, `${cat}.claim`)!
      const attribute = str(c.attribute, `${cat}.attribute`)!
      if (!isBuiltin(attribute) && !attrs.has(attribute)) throw new Error(`${cat}.attribute: "${attribute}" is not in the profile`)
      if (mapped.has(attribute)) throw new Error(`${cat}.attribute: "${attribute}" is mapped twice for ${name}`)
      mapped.add(attribute)
      const directoryPath = str(c.directoryPath, `${cat}.directoryPath`, false)
      return directoryPath ? { claim, attribute, directoryPath } : { claim, attribute }
    })
    const directory = readDirectory(t.directory, `${at}.directory`, name)
    next = {
      ...next, tiers: next.tiers.map(tt => {
        if (tt.name !== name) return tt
        const out: Tier = { ...tt, claims, directory }
        if (!claims.length) delete out.claims
        if (!directory) delete out.directory
        return out
      }),
    }
  })
  return next
}
