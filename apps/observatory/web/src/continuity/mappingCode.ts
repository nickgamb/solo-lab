import { BUILTIN_ATTRIBUTES, type ClaimMapping, type ContinuitySpec, type ProfileAttribute, type Tier } from '../api'
import { ATTR_NAME, isBuiltin, withProfile } from './mapping'

// The Code tab: the mapping the canvas draws, as JSON. Each key is an S&V
// profile attribute (the destination); its value lists the IdP claims that
// fill it, as "<idp>.<claim>", in chain order: the first IdP with a value
// wins. A source with a directory path is {"from": "<idp>.<claim>",
// "directoryPath": "..."}. A key that isn't built in is a profile attribute;
// [] declares one nothing fills yet.
//
//   {
//     "email": ["auth0.email", "keycloak.email"],
//     "department": [{"from": "auth0.https://sv/department", "directoryPath": "app_metadata.department"}, "keycloak.department"]
//   }

// username is the verified email at every IdP: never mapped
const SHOWN_BUILTINS = BUILTIN_ATTRIBUTES.filter(a => a !== 'username')

type Source = string | { from: string; directoryPath: string }

export function toCode(spec: ContinuitySpec): string {
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const declared = (spec.profile?.attributes ?? []).map(a => a.name)
  const mapped = idps.flatMap(t => (t.claims ?? []).map(c => c.attribute))
  const keys = [...new Set([...SHOWN_BUILTINS, ...declared, ...mapped])].filter(a => a !== 'username')
  const lines = keys.map(attr => {
    const sources: Source[] = []
    for (const t of idps) {
      const c = t.claims?.find(m => m.attribute === attr)
      if (!c) continue
      const from = `${t.name}.${c.claim}`
      sources.push(c.directoryPath ? { from, directoryPath: c.directoryPath } : from)
    }
    return `  ${JSON.stringify(attr)}: [${sources.map(s => JSON.stringify(s).replace(/","/g, '", "').replace(/":"/g, '": "')).join(', ')}]`
  })
  return `{\n${lines.join(',\n')}\n}\n`
}

// applyCode returns the spec with the document's mapping applied (each IdP's
// claims, the profile's attributes), or throws a message naming the key at
// fault. Everything else in the spec passes through.
export function applyCode(spec: ContinuitySpec, text: string): ContinuitySpec {
  let doc: unknown
  try { doc = JSON.parse(text) } catch (e) { throw new Error(`JSON: ${(e as Error).message}`) }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('expected an object: { "<attribute>": ["<idp>.<claim>", ...] }')
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const order = idps.map(t => t.name)
  const claims = new Map<string, ClaimMapping[]>(order.map(n => [n, []]))
  const attrs: string[] = []
  for (const [attr, value] of Object.entries(doc as Record<string, unknown>)) {
    if (attr === 'username') throw new Error('username: the verified email at every IdP, never mapped')
    if (!ATTR_NAME.test(attr)) throw new Error(`"${attr}": an attribute name starts with a letter; letters, digits, _ . - only`)
    if (!Array.isArray(value)) throw new Error(`${attr}: expected a list of "<idp>.<claim>"`)
    if (!isBuiltin(attr)) attrs.push(attr)
    let last = -1
    for (const [i, s] of value.entries()) {
      const at = `${attr}[${i}]`
      let from: unknown = s, directoryPath: string | undefined
      if (s && typeof s === 'object' && !Array.isArray(s)) {
        const o = s as Record<string, unknown>
        const extra = Object.keys(o).filter(k => k !== 'from' && k !== 'directoryPath')
        if (extra.length) throw new Error(`${at}: unknown field ${extra.join(', ')} (expected from, directoryPath)`)
        from = o.from
        if (o.directoryPath != null && typeof o.directoryPath !== 'string') throw new Error(`${at}.directoryPath: expected a string`)
        directoryPath = (o.directoryPath as string | undefined)?.trim() || undefined
      }
      if (typeof from !== 'string') throw new Error(`${at}: expected "<idp>.<claim>"`)
      const dot = from.indexOf('.')
      const idp = dot > 0 ? from.slice(0, dot) : '', claim = dot > 0 ? from.slice(dot + 1).trim() : ''
      if (!idp || !claim) throw new Error(`${at}: "${from}" is not "<idp>.<claim>"`)
      const pos = order.indexOf(idp)
      if (pos < 0) throw new Error(`${at}: no IdP "${idp}" in the chain (${order.join(', ') || 'none'}; add IdPs in the rule builder)`)
      if (pos === last) throw new Error(`${attr}: ${idp} is listed twice; each IdP fills an attribute from one claim`)
      if (pos < last) throw new Error(`${attr}: list sources in chain order (${order.join(', ')}): the first IdP with a value wins`)
      last = pos
      claims.get(idp)!.push(directoryPath ? { claim, attribute: attr, directoryPath } : { claim, attribute: attr })
    }
  }
  // attributes keep their display name and multivalued setting
  const prev = new Map((spec.profile?.attributes ?? []).map(a => [a.name, a]))
  const attributes: ProfileAttribute[] = attrs.map(name => prev.get(name) ?? { name })
  const next = withProfile(spec, { ...spec.profile, attributes })
  return {
    ...next, tiers: next.tiers.map(t => {
      if (t.type !== 'oidc') return t
      const out: Tier = { ...t, claims: claims.get(t.name) }
      if (!out.claims?.length) delete out.claims
      return out
    }),
  }
}
