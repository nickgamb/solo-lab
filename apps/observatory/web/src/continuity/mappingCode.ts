import { BUILTIN_ATTRIBUTES, type AttributeMapping, type ContinuitySpec, type ProfileAttribute, type Tier } from '../api'
import { ATTR_NAME, isBuiltin, withProfile } from './mapping'

// The Code tab: the directory sync's attribute mapping, as JSON. Each key is
// an attribute of S&V's profile on the broker; its value lists the IdPs'
// attributes paired with it, as "<idp>.<attribute path>", in chain order. The
// primary's (the chain's first IdP) is read into the profile; each
// failover's is written from it. A key that isn't built in is a profile
// attribute; [] declares one nothing maps yet.
//
//   {
//     "email": ["auth0.email", "keycloak.email"],
//     "department": ["auth0.user_metadata.department", "keycloak.department"]
//   }

// username is the broker's own name for the user: never synced
const SHOWN_BUILTINS = BUILTIN_ATTRIBUTES.filter(a => a !== 'username')

export function toCode(spec: ContinuitySpec): string {
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const declared = (spec.profile?.attributes ?? []).map(a => a.name)
  const mapped = idps.flatMap(t => (t.attributes ?? []).map(m => m.attribute))
  const keys = [...new Set([...SHOWN_BUILTINS, ...declared, ...mapped])].filter(a => a !== 'username')
  const lines = keys.map(attr => {
    const sources = idps.flatMap(t => {
      const m = t.attributes?.find(x => x.attribute === attr)
      return m ? [JSON.stringify(`${t.name}.${m.path}`)] : []
    })
    return `  ${JSON.stringify(attr)}: [${sources.join(', ')}]`
  })
  return `{\n${lines.join(',\n')}\n}\n`
}

// applyCode returns the spec with the document's mapping applied (each IdP's
// attribute mapping, the profile's attributes), or throws a message naming
// the key at fault. Everything else in the spec passes through.
export function applyCode(spec: ContinuitySpec, text: string): ContinuitySpec {
  let doc: unknown
  try { doc = JSON.parse(text) } catch (e) { throw new Error(`JSON: ${(e as Error).message}`) }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('expected an object: { "<attribute>": ["<idp>.<attribute>", ...] }')
  const order = spec.tiers.filter(t => t.type === 'oidc').map(t => t.name)
  const maps = new Map<string, AttributeMapping[]>(order.map(n => [n, []]))
  const attrs: string[] = []
  for (const [attr, value] of Object.entries(doc as Record<string, unknown>)) {
    if (attr === 'username') throw new Error("username: the broker's own name for the user, never synced")
    if (!ATTR_NAME.test(attr)) throw new Error(`"${attr}": an attribute name starts with a letter; letters, digits, _ . - only`)
    if (!Array.isArray(value)) throw new Error(`${attr}: expected a list of "<idp>.<attribute>"`)
    if (!isBuiltin(attr)) attrs.push(attr)
    let last = -1
    for (const [i, s] of value.entries()) {
      const at = `${attr}[${i}]`
      if (typeof s !== 'string') throw new Error(`${at}: expected "<idp>.<attribute>"`)
      const dot = s.indexOf('.')
      const idp = dot > 0 ? s.slice(0, dot) : '', path = dot > 0 ? s.slice(dot + 1).trim() : ''
      if (!idp || !path) throw new Error(`${at}: "${s}" is not "<idp>.<attribute>"`)
      const pos = order.indexOf(idp)
      if (pos < 0) throw new Error(`${at}: no IdP "${idp}" configured (${order.join(', ') || 'none'}; add IdPs in the rule builder)`)
      if (pos === last) throw new Error(`${attr}: ${idp} is listed twice; each IdP pairs one of its attributes with it`)
      if (pos < last) throw new Error(`${attr}: list IdPs in chain order (${order.join(', ')}): the primary first`)
      last = pos
      maps.get(idp)!.push({ attribute: attr, path })
    }
  }
  // attributes keep their display name
  const prev = new Map((spec.profile?.attributes ?? []).map(a => [a.name, a]))
  const attributes: ProfileAttribute[] = attrs.map(name => prev.get(name) ?? { name })
  const next = withProfile(spec, { ...spec.profile, attributes })
  return {
    ...next, tiers: next.tiers.map(t => {
      if (t.type !== 'oidc') return t
      const out: Tier = { ...t, attributes: maps.get(t.name) }
      if (!out.attributes?.length) delete out.attributes
      return out
    }),
  }
}
