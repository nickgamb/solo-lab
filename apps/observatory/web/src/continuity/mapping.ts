import { BUILTIN_ATTRIBUTES, type AttributeMapping, type ContinuitySpec, type Directory, type Profile, type ProfileAttribute, type Sync, type Tier } from '../api'
import { describeCron } from './cron'

// Pure edits on a working copy of an IdentityContinuity spec. Each touches
// only the profile, the sync, or an IdP's attribute mapping and directory;
// everything else in the spec passes through untouched.

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

function withMappings(t: Tier, attributes: AttributeMapping[]): Tier {
  const next: Tier = { ...t, attributes }
  if (!attributes.length) delete next.attributes
  return next
}

// one mapping per (IdP, profile attribute): a new path for the same attribute replaces the old one
export const setMapping = (spec: ContinuitySpec, idp: string, path: string, attribute: string) =>
  withTier(spec, idp, t => {
    const ms = [...(t.attributes ?? [])]
    const i = ms.findIndex(m => m.attribute === attribute)
    if (i >= 0 && ms[i].path === path) return t
    if (i >= 0) ms[i] = { attribute, path }
    else ms.push({ attribute, path })
    return withMappings(t, ms)
  })

export const removeMapping = (spec: ContinuitySpec, idp: string, attribute: string) =>
  withTier(spec, idp, t => withMappings(t, (t.attributes ?? []).filter(m => m.attribute !== attribute)))

export const setDirectory = (spec: ContinuitySpec, tier: string, d: Directory | undefined) =>
  withTier(spec, tier, t => {
    const next = { ...t, directory: d }
    if (!d) delete next.directory
    return next
  })

// a profile attribute without its defaults (type string, one value)
function tidy(a: ProfileAttribute): ProfileAttribute {
  const out: ProfileAttribute = { ...a }
  if (!out.displayName) delete out.displayName
  if (!out.type || out.type === 'string') delete out.type
  if (!out.multivalued) delete out.multivalued
  return out
}

export const addAttribute = (spec: ContinuitySpec, a: ProfileAttribute) =>
  withProfile(spec, { ...spec.profile, attributes: [...(spec.profile?.attributes ?? []), tidy(a)] })

export const updateAttribute = (spec: ContinuitySpec, a: ProfileAttribute) =>
  withProfile(spec, { ...spec.profile, attributes: (spec.profile?.attributes ?? []).map(x => (x.name === a.name ? tidy(a) : x)) })

// removing an attribute removes every mapping into it, so none is left dangling
export function removeAttribute(spec: ContinuitySpec, name: string): ContinuitySpec {
  const next = withProfile(spec, { ...spec.profile, attributes: (spec.profile?.attributes ?? []).filter(a => a.name !== name) })
  return { ...next, tiers: next.tiers.map(t => (t.attributes?.some(m => m.attribute === name) ? withMappings(t, t.attributes.filter(m => m.attribute !== name)) : t)) }
}

export function attributeError(name: string, spec: ContinuitySpec): string | undefined {
  if (!ATTR_NAME.test(name)) return 'starts with a letter; letters, digits, _ . - only'
  if (isBuiltin(name) || spec.profile?.attributes?.some(a => a.name === name)) return 'already in the profile'
  return undefined
}

// one line for the rule builder: which IdPs sync and when
export function syncSummary(spec: ContinuitySpec): string {
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const withDir = idps.filter(t => t.directory && t.attributes?.length).length
  const sync = !spec.sync ? 'no schedule' : spec.sync.suspend ? 'paused'
    : describeCron(spec.sync.schedule).replace(/^./, c => c.toLowerCase()) || spec.sync.schedule
  return `${withDir} of ${idps.length} IdPs mapped · ${sync}`
}

// the kind of directory an IdP likely has, from its issuer, before one is set
export function kindOf(issuer = ''): Directory['type'] {
  if (/\.auth0\.com\/?$/.test(issuer)) return 'auth0'
  if (/\/realms\/[^/]+\/?$/.test(issuer)) return 'keycloak'
  return 'scim'
}

// where a directory of that kind usually is for that issuer (SCIM varies by
// product: none suggested)
export function suggestDirectory(type: Directory['type'], issuer = ''): { url: string; audience?: string } {
  const base = issuer.replace(/\/$/, '')
  if (type === 'auth0') return { url: `${base}/api/v2`, audience: `${base}/api/v2/` }
  if (type === 'keycloak' && /\/realms\/[^/]+$/.test(base)) return { url: base.replace(/\/realms\/([^/]+)$/, '/admin/realms/$1') }
  return { url: '' }
}
