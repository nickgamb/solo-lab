import type { IdentityContinuity } from '../api'

// The identity fabric's routing rules as people write them: each rule's
// CEL as it is, with no quoting to escape.
//
//   rule ledgerline -> gluu, okta, keycloak
//     # Sign-ins for Ledgerline Research go to an IdP that meets AAL2
//     request.uri.matches("[?&]resource=https(%3A|:)(%2F|/){2}mcp.sterling.lab")
//
// A rule is a header line (its name, then the IdPs in order of preference),
// then its indented body: comment lines are its description, the rest its
// CEL. First match wins; a sign-in no rule matches goes to the active IdP.

export type RoutingRule = { name: string; when: string; idps: string[]; description?: string }
export type CodeError = { line: number; col: number; message: string }

const NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const HEADER = /^rule\s+(\S+)\s*->\s*(.*)$/

export function toCode(rules: RoutingRule[]): string {
  const head = [
    '# Which IdP each sign-in goes to, decided at the gateway. First match wins;',
    '# the first IdP in a rule that can sign people in now takes the sign-in.',
    '# Anything no rule matches goes to the active IdP.',
    '',
  ]
  return head.concat(rules.flatMap(r => [
    `rule ${r.name} -> ${r.idps.join(', ')}`,
    ...(r.description ? r.description.split('\n').map(l => `  # ${l}`) : []),
    ...r.when.split('\n').map(l => `  ${l}`),
    '',
  ])).join('\n')
}

// fromCode: the rules, or every error, by line. Known IdPs, when given,
// flag a name that isn't in the chain.
export function fromCode(src: string, known?: string[]): { rules?: RoutingRule[]; errors: CodeError[] } {
  const errors: CodeError[] = []
  const rules: (RoutingRule & { line: number })[] = []
  let cur: (RoutingRule & { line: number; desc: string[]; cel: string[] }) | undefined
  const done = () => {
    if (!cur) return
    const when = cur.cel.join('\n').trim()
    if (!when) errors.push({ line: cur.line, col: 1, message: `rule ${cur.name}: no CEL (indent it under the rule)` })
    rules.push({ name: cur.name, idps: cur.idps, when, line: cur.line, ...(cur.desc.length ? { description: cur.desc.join('\n') } : {}) })
    cur = undefined
  }
  src.split('\n').forEach((raw, i) => {
    const line = i + 1
    const text = raw.replace(/\s+$/, '')
    if (!text) { if (cur) cur.cel.push(''); return }
    if (!/^\s/.test(text)) {
      if (text.startsWith('#')) return
      done()
      const m = HEADER.exec(text)
      if (!m) { errors.push({ line, col: 1, message: 'expected: rule <name> -> <idp>, <idp>...' }); return }
      const name = m[1], idps = m[2].split(',').map(s => s.trim()).filter(Boolean)
      if (!NAME.test(name)) errors.push({ line, col: 6, message: `${name}: lowercase letters, digits and dashes` })
      if (rules.some(r => r.name === name)) errors.push({ line, col: 6, message: `${name}: a rule by that name comes earlier` })
      if (!idps.length) errors.push({ line, col: text.indexOf('->') + 1, message: 'name at least one IdP after ->' })
      for (const n of idps) {
        if (known && !known.includes(n)) errors.push({ line, col: text.indexOf(n) + 1, message: `${n} isn't in the chain now: skipped until it is` })
      }
      cur = { name, idps, when: '', line, desc: [], cel: [] }
      return
    }
    if (!cur) { errors.push({ line, col: 1, message: 'an indented line belongs under a rule' }); return }
    const body = text.trim()
    if (body.startsWith('#')) { // before the CEL: its description; after it, a note that isn't kept
      if (!cur.cel.some(l => l.trim())) cur.desc.push(body.replace(/^#\s?/, ''))
      return
    }
    cur.cel.push(text.replace(/^ {2}/, ''))
  })
  done()
  // an IdP outside the chain is a warning, not a reason to refuse the rules
  const fatal = errors.filter(e => !e.message.includes("isn't in the chain"))
  return { rules: fatal.length ? undefined : rules.map(r => ({ name: r.name, idps: r.idps, when: r.when, ...(r.description ? { description: r.description } : {}) })), errors }
}

export const sameRules = (a: RoutingRule[], b: RoutingRule[]) => JSON.stringify(norm(a)) === JSON.stringify(norm(b))
const norm = (rs: RoutingRule[]) => rs.map(r => ({ name: r.name, when: r.when.trim(), idps: r.idps, description: r.description ?? '' }))

// routingSummary: the line next to the button.
export function routingSummary(ic: IdentityContinuity): string {
  const rules = ic.spec.routing?.rules ?? []
  if (!ic.spec.routing) return 'no routing policy: every sign-in goes to the active IdP'
  const st = new Map((ic.status?.routing ?? []).map(r => [r.name, r.idp]))
  return rules.length
    ? rules.map(r => `${r.name} → ${st.get(r.name) || ic.status?.active || '?'}`).join(', ')
    : 'no rules: every sign-in goes to the active IdP'
}
