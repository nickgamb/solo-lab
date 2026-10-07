import { BUILTIN_ATTRIBUTES, type AttributeMapping, type ContinuitySpec, type ProfileAttribute, type Tier } from '../api'
import { ATTR_NAME, isBuiltin, withProfile } from './mapping'

// The Code tab: the directory sync's attribute mapping, as JSON. Each key is
// an attribute of the broker's profile; its value lists the IdPs'
// attributes paired with it, as "<idp>.<attribute path>", in chain order. The
// primary's (the chain's first IdP) is read into the profile; each
// failover's is written from it. A key that isn't built in is a profile
// attribute; [] declares one nothing maps yet.
//
//   {
//     "email": ["auth0.email", "keycloak.email"],
//     "department": ["auth0.user_metadata.department", "keycloak.department"]
//   }

// username is the broker's own name for the user: never synced, so it's
// listed only when something maps it (to be removed)
const SHOWN_BUILTINS = BUILTIN_ATTRIBUTES.filter(a => a !== 'username')

export function toCode(spec: ContinuitySpec): string {
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const declared = (spec.profile?.attributes ?? []).map(a => a.name)
  const mapped = idps.flatMap(t => (t.attributes ?? []).map(m => m.attribute))
  const keys = [...new Set([...SHOWN_BUILTINS, ...declared, ...mapped])]
  const lines = keys.map(attr => {
    const sources = idps.flatMap(t => {
      const m = t.attributes?.find(x => x.attribute === attr)
      return m ? [JSON.stringify(`${t.name}.${m.path}`)] : []
    })
    return `  ${JSON.stringify(attr)}: [${sources.join(', ')}]`
  })
  return `{\n${lines.join(',\n')}\n}\n`
}

// what a mapping says, order aside: each attribute's (IdP, path) pairs, and
// the attributes the profile lists (as toCode shows them)
function meaning(pairs: [string, string, string][], attrs: string[]): string {
  return JSON.stringify([pairs.map(p => p.join('\u0000')).sort(), [...new Set(attrs)].sort()])
}

function current(spec: ContinuitySpec): string {
  const idps = spec.tiers.filter(t => t.type === 'oidc')
  const pairs = idps.flatMap(t => (t.attributes ?? []).map(m => [m.attribute, t.name, m.path] as [string, string, string]))
  const attrs = [...(spec.profile?.attributes ?? []).map(a => a.name), ...pairs.map(p => p[0])].filter(a => !isBuiltin(a))
  return meaning(pairs, attrs)
}

// the document's meaning, read leniently (undefined when it isn't the
// expected shape; applyCode then says what's wrong)
function proposed(doc: Record<string, unknown>): string | undefined {
  const pairs: [string, string, string][] = []
  for (const [attr, value] of Object.entries(doc)) {
    if (!Array.isArray(value)) return undefined
    for (const s of value) {
      const dot = typeof s === 'string' ? s.indexOf('.') : -1
      if (dot <= 0) return undefined
      pairs.push([attr, s.slice(0, dot), s.slice(dot + 1).trim()])
    }
  }
  return meaning(pairs, Object.keys(doc).filter(a => !isBuiltin(a)))
}

// applyCode returns the spec with the document's mapping applied (each IdP's
// attribute mapping, the profile's attributes), or throws a message naming
// the key at fault. A document that means what the spec already says
// returns the spec itself; otherwise mappings and attributes that stay keep
// their place. Everything else in the spec passes through.
export function applyCode(spec: ContinuitySpec, text: string): ContinuitySpec {
  let doc: unknown
  try { doc = JSON.parse(text) } catch (e) { throw new Error(`JSON: ${(e as Error).message}`) }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('expected an object: { "<attribute>": ["<idp>.<attribute>", ...] }')
  if (proposed(doc as Record<string, unknown>) === current(spec)) return spec
  const order = spec.tiers.filter(t => t.type === 'oidc').map(t => t.name)
  const maps = new Map<string, Map<string, string>>(order.map(n => [n, new Map()]))
  const attrs: string[] = []
  for (const [attr, value] of Object.entries(doc as Record<string, unknown>)) {
    if (attr === 'username' && !(Array.isArray(value) && !value.length)) {
      throw new Error("username: the broker's own name for the user, never synced; remove this key (that removes its mappings)")
    }
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
      maps.get(idp)!.set(attr, path)
    }
  }
  // attributes that stay keep their place and display name; new ones follow
  const prev = spec.profile?.attributes ?? []
  const keep = new Set(attrs)
  const attributes: ProfileAttribute[] = [...prev.filter(a => keep.has(a.name)), ...attrs.filter(n => !prev.some(a => a.name === n)).map(name => ({ name }))]
  const next = withProfile(spec, { ...spec.profile, attributes })
  return {
    ...next, tiers: next.tiers.map(t => {
      if (t.type !== 'oidc') return t
      const want = maps.get(t.name)!
      const had = t.attributes ?? []
      const ms: AttributeMapping[] = [
        ...had.filter(m => want.has(m.attribute)).map(m => ({ ...m, path: want.get(m.attribute)! })),
        ...[...want].filter(([a]) => !had.some(m => m.attribute === a)).map(([attribute, path]) => ({ attribute, path })),
      ]
      const out: Tier = { ...t, attributes: ms }
      if (!ms.length) delete out.attributes
      return out
    }),
  }
}
