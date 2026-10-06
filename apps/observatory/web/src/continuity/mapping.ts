import { BUILTIN_ATTRIBUTES, type ClaimMapping, type ContinuitySpec, type Directory, type Profile, type ProfileAttribute, type Sync, type Tier } from '../api'
import { describeCron } from './cron'

// Pure edits on a working copy of an IdentityContinuity spec. Each touches
// only profile, sync, or a tier's claims and directory; everything else in
// the spec passes through untouched.

export const ATTR_NAME = /^[a-zA-Z][a-zA-Z0-9_.-]*$/
export const clone = <T,>(v: T): T => JSON.parse(JSON.stringify(v))
export const credentialsName = (tier: string) => `directory-${tier}`

// JSON with sorted keys, so key order never reads as an edit
export function stable(v: unknown): string {
  return JSON.stringify(v, (_, x) => (x && typeof x === 'object' && !Array.isArray(x)
    ? Object.fromEntries(Object.keys(x).sort().map(k => [k, (x as Record<string, unknown>)[k]])) : x))
}

export const isBuiltin = (name: string) => (BUILTIN_ATTRIBUTES as readonly string[]).includes(name)

export function withProfile(spec: ContinuitySpec, p: Profile): ContinuitySpec {
  const profile: Profile = { ...p }
  if (!profile.attributes?.length) delete profile.attributes
  if (!profile.tokenClients?.length) delete profile.tokenClients
  const next: ContinuitySpec = { ...spec, profile }
  if (!Object.keys(profile).length) delete next.profile
  return next
}

export function withSync(spec: ContinuitySpec, sync: Sync | undefined): ContinuitySpec {
  const next = { ...spec, sync }
  if (!sync) delete next.sync
  return next
}

function withTier(spec: ContinuitySpec, name: string, f: (t: Tier) => Tier): ContinuitySpec {
  return { ...spec, tiers: spec.tiers.map(t => (t.name === name ? f(t) : t)) }
}

function withClaims(t: Tier, claims: ClaimMapping[]): Tier {
  const next: Tier = { ...t, claims }
  if (!claims.length) delete next.claims
  return next
}

// one mapping per (tier, attribute): a new claim for the same attribute replaces the old one
export const setMapping = (spec: ContinuitySpec, tier: string, claim: string, attribute: string) =>
  withTier(spec, tier, t => {
    const claims = [...(t.claims ?? [])]
    const i = claims.findIndex(c => c.attribute === attribute)
    if (i >= 0 && claims[i].claim === claim) return t
    if (i >= 0) claims[i] = { claim, attribute }
    else claims.push({ claim, attribute })
    return withClaims(t, claims)
  })

export const removeMapping = (spec: ContinuitySpec, tier: string, attribute: string) =>
  withTier(spec, tier, t => withClaims(t, (t.claims ?? []).filter(c => c.attribute !== attribute)))

export const setDirectoryPath = (spec: ContinuitySpec, tier: string, attribute: string, path: string) =>
  withTier(spec, tier, t => withClaims(t, (t.claims ?? []).map(c => {
    if (c.attribute !== attribute) return c
    const next: ClaimMapping = { ...c, directoryPath: path.trim() }
    if (!next.directoryPath) delete next.directoryPath
    return next
  })))

export const setDirectory = (spec: ContinuitySpec, tier: string, d: Directory | undefined) =>
  withTier(spec, tier, t => {
    const next = { ...t, directory: d }
    if (!d) delete next.directory
    return next
  })

export const addAttribute = (spec: ContinuitySpec, name: string) =>
  withProfile(spec, { ...spec.profile, attributes: [...(spec.profile?.attributes ?? []), { name }] })

export const toggleMultivalued = (spec: ContinuitySpec, name: string) =>
  withProfile(spec, { ...spec.profile, attributes: (spec.profile?.attributes ?? []).map(a => {
    if (a.name !== name) return a
    const next: ProfileAttribute = { ...a, multivalued: !a.multivalued }
    if (!next.multivalued) delete next.multivalued
    return next
  }) })

// removing an attribute removes every mapping into it, so none is left dangling
export function removeAttribute(spec: ContinuitySpec, name: string): ContinuitySpec {
  const next = withProfile(spec, { ...spec.profile, attributes: (spec.profile?.attributes ?? []).filter(a => a.name !== name) })
  return { ...next, tiers: next.tiers.map(t => (t.claims?.some(c => c.attribute === name) ? withClaims(t, t.claims.filter(c => c.attribute !== name)) : t)) }
}

export function attributeError(name: string, spec: ContinuitySpec): string | undefined {
  if (!ATTR_NAME.test(name)) return 'starts with a letter; letters, digits, _ . - only'
  if (isBuiltin(name) || spec.profile?.attributes?.some(a => a.name === name)) return 'already in the profile'
  return undefined
}

// one line for the rule builder: how much is mapped and when the sync runs
export function claimsSummary(spec: ContinuitySpec): string {
  const oidc = spec.tiers.filter(t => t.type === 'oidc')
  const mapped = oidc.filter(t => t.claims?.length).length
  const sync = !spec.sync ? 'no scheduled sync' : spec.sync.suspend ? 'sync paused'
    : `sync ${describeCron(spec.sync.schedule).replace(/^./, c => c.toLowerCase()) || spec.sync.schedule}`
  return `${mapped} of ${oidc.length} IdPs mapped · ${sync}`
}
