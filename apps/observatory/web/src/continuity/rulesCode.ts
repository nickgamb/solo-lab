import { Document, isMap, isNode, isScalar, isSeq, LineCounter, parseDocument, type Node } from 'yaml'
import type { AssurancePolicy, AssuranceView, PolicyPoint, ProfileSpec, RuleSchema, TierAssurance, WorkloadRef } from '../api.ts'

// The Assurance rules window's working copy, and its Code tab: the same, as
// a YAML document.
//
//   default:                       # the chain's default rule
//     minimum: AAL1
//     idps: []                     # []: every IdP in the chain
//     break_glass: false
//
//   idps:                          # what each IdP's sign-ins prove
//     keycloak:
//       otherwise: AAL1
//       acr: { aal2: AAL2 }
//       amr: { hwk: { level: AAL3, phishing_resistant: true } }
//
//   rules:                         # what a rule leaves out is the default's
//     ledger:
//       criticality: Critical
//       mode: Enforce
//       workloads: [apps/ledger]
//       minimum: AAL2
//
//   policy_points:                 # each gateway policy, and the rule it asks for
//     apps/ledger-caller: ledger   # default: the default rule; null: none
//
// Choices (levels, criticality, modes, sessions) are the installed CRDs',
// written as they are there; case and _ don't matter when reading.

export type Draft = {
  policy: AssurancePolicy
  idps: Record<string, TierAssurance | undefined>
  rules: Record<string, ProfileSpec | null> // null: removed
  points: Record<string, string | null> // policy point (pointKey) -> the rule it asks for ("" the default), null: none
}

export const idpLabel = (i: { name: string; displayName?: string }) => i.displayName || i.name

// criticality's place in the CRD's list (first: most critical), as a class
export const critClass = (s: RuleSchema | undefined, c: string) => {
  const i = s?.criticality.indexOf(c) ?? -1
  return `crit c${i < 0 ? 'x' : Math.min(i, 2)}`
}

export const phaseChip = (phase?: string) => (phase === 'FailedClosed' ? 'chip bad' : phase === 'Degraded' ? 'chip warn' : 'chip ok')

// a rule the gate enforces (not report-only, not off)
export const enforced = (p: { mode?: string }) => !p.mode || p.mode === 'Enforce'

export const pointKey = (p: Pick<PolicyPoint, 'kind' | 'namespace' | 'name'>) => `${p.kind} ${p.namespace}/${p.name}`

export const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v ?? null)) as T

export function draftOf(v: AssuranceView): Draft {
  const d: Draft = { policy: clone(v.policy ?? {}), idps: {}, rules: {}, points: {} }
  for (const i of v.idps) d.idps[i.name] = i.assurance ? clone(i.assurance) : undefined
  for (const r of v.rules) d.rules[r.name] = clone(r.spec)
  for (const p of v.policyPoints) if (!p.extAuth) d.points[pointKey(p)] = p.rule
  return normalize(d)
}

// a Go duration as the API returns it ("12h0m0s") as people write it ("12h")
export function shortDuration(s?: string): string | undefined {
  if (!s) return undefined
  const out = s.replace(/(?<![0-9.])0+(ns|us|µs|ms|s|m|h)/g, '').replace(/^$/, '0s')
  return out || s
}
export const DURATION = /^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$/

function normalize(d: Draft): Draft {
  if (d.policy.maxAge) d.policy.maxAge = shortDuration(d.policy.maxAge)
  for (const r of Object.values(d.rules)) if (r?.assurance?.maxAge) r.assurance.maxAge = shortDuration(r.assurance.maxAge)
  return d
}

// same meaning: key order and unset fields aside
export function same(a: unknown, b: unknown): boolean {
  return canon(a) === canon(b)
}
function canon(v: unknown): string {
  const strip = (x: unknown): unknown => {
    if (Array.isArray(x)) return x.map(strip)
    if (x && typeof x === 'object') {
      const o: Record<string, unknown> = {}
      for (const k of Object.keys(x).sort()) {
        const y = strip((x as Record<string, unknown>)[k])
        if (y === undefined || (Array.isArray(y) && !y.length) || (y && typeof y === 'object' && !Array.isArray(y) && !Object.keys(y).length)) continue
        o[k] = y
      }
      return o
    }
    return x
  }
  return JSON.stringify(strip(v))
}


// --- printing

const levelValue = (l: { level: string; phishingResistant?: boolean }) =>
  l.phishingResistant ? { level: l.level, phishing_resistant: true } : l.level

// Maps, not objects, wherever keys are names: an object would put names
// that look like numbers first.
function idpBody(a?: TierAssurance): Map<string, unknown> {
  const body = new Map<string, unknown>()
  if (a?.default) body.set('otherwise', a.default)
  for (const claim of ['acr', 'amr'] as const) {
    const rows = (a?.levels ?? []).filter(l => l[claim]).map(l => [l[claim]!, levelValue(l)] as [string, unknown])
    if (rows.length) body.set(claim, new Map(rows))
  }
  return body
}

export function refOf(v: AssuranceView, key: string): string {
  const p = v.policyPoints.find(x => pointKey(x) === key)
  if (!p) return key
  const ambiguous = v.policyPoints.filter(x => x.namespace === p.namespace && x.name === p.name).length > 1
  return ambiguous ? key : `${p.namespace}/${p.name}`
}

export function toCode(d: Draft, v: AssuranceView): string {
  const s = v.schema
  const crit = (c: string) => (s?.criticality.indexOf(c) ?? -1) < 0 ? 99 : s!.criticality.indexOf(c)
  const doc = new Document()
  // lists on one line while they fit; a level with phishing_resistant too
  const node = (x: unknown) => {
    const n = doc.createNode(x)
    if (isSeq(n)) n.flow = n.items.map(i => String(isScalar(i) ? i.value : '')).join(', ').length <= 70
    if (isMap(n) && n.items.some(p => isScalar(p.key) && p.key.value === 'phishing_resistant')) n.flow = true
    return n
  }
  const p = d.policy
  const def = new Map<string, unknown>([
    ['minimum', p.minimum ?? s?.defaults.minimum ?? null],
    ['phishing_resistant', !!p.phishingResistant],
    ['max_age', p.maxAge ?? null],
    ['idps', node(p.allowedIdPs ?? [])],
    ['break_glass', !!p.allowBreakGlass],
    ['sessions', p.sessions ?? s?.defaults.sessions ?? null],
  ])
  const idps = new Map(v.idps.map(i => {
    const body = idpBody(d.idps[i.name])
    for (const claim of ['acr', 'amr']) {
      const m = body.get(claim) as Map<string, unknown> | undefined
      if (m) body.set(claim, new Map([...m].map(([k, l]) => [k, node(l)])))
    }
    return [i.name, body]
  }))
  const names = Object.keys(d.rules).filter(n => d.rules[n]).sort((a, b) => crit(d.rules[a]!.criticality) - crit(d.rules[b]!.criticality) || a.localeCompare(b))
  const rules = new Map(names.map(n => {
    const r = d.rules[n]!
    const body = new Map<string, unknown>()
    const put = (key: string, value: unknown) => { if (value !== undefined && value !== '' && !(Array.isArray(value) && !value.length)) body.set(key, Array.isArray(value) ? node(value) : value) }
    put('description', r.description)
    put('criticality', r.criticality)
    put('mode', r.mode)
    put('owner', r.owner)
    put('obligations', r.obligations)
    put('workloads', r.workloads?.map(w => `${w.namespace}/${w.serviceAccount}`))
    put('clients', r.clients)
    const rule = (key: string, value: unknown) => { if (value !== undefined) body.set(key, Array.isArray(value) ? node(value) : value) }
    rule('minimum', r.assurance?.minimum)
    rule('phishing_resistant', r.assurance?.phishingResistant)
    rule('max_age', r.assurance?.maxAge)
    rule('idps', r.allowedIdPs?.length ? r.allowedIdPs : undefined)
    rule('break_glass', r.allowBreakGlass)
    rule('sessions', r.sessions)
    return [n, body]
  }))
  const points = new Map(Object.keys(d.points).map(k => [refOf(v, k), d.points[k] === '' ? 'default' : d.points[k]] as [string, string | null])
    .sort(([a], [b]) => a.localeCompare(b)))
  doc.contents = doc.createNode(new Map<string, unknown>([['default', def], ['idps', idps], ['rules', rules], ['policy_points', points]]))

  doc.commentBefore = ` ${v.namespace}/${v.name}\n IdPs, in failover order: ${v.idps.map(i => i.name).join(', ')}${v.active ? `; ${v.active} is active` : ''}`
  const pair = (m: unknown, key: string) => (isMap(m) ? m.items.find(x => isScalar(x.key) && x.key.value === key) : undefined)
  const top = (key: string, comment: string) => {
    const k = pair(doc.contents, key)?.key
    if (isScalar(k)) { k.commentBefore = comment; k.spaceBefore = key !== 'default' }
  }
  top('default', ' the chain\'s default rule (idps: [] is every IdP in the chain)')
  top('idps', ' what each IdP\'s sign-ins prove')
  top('rules', ' each rule, most critical first; what it leaves out is the default\'s')
  top('policy_points', ' each gateway policy that takes the broker\'s tokens, and the rule it asks\n the gate for: a rule, default (the default rule) or null (none)')
  for (const i of v.idps) {
    const notes = [i.displayName && i.displayName !== i.name ? i.displayName : '', i.type, i.name === v.active ? 'active' : '', i.enabled ? '' : 'disabled'].filter(Boolean)
    const k = pair(doc.getIn(['idps']), i.name)?.key
    if (isScalar(k) && notes.length) k.commentBefore = ` ${notes.join(' · ')}`
  }
  names.forEach((n, i) => {
    const k = pair(doc.getIn(['rules']), n)?.key
    if (isScalar(k) && i) k.spaceBefore = true
  })
  return doc.toString({ flowCollectionPadding: false, lineWidth: 0 })
}

// --- reading

export type CodeError = { line: number; col: number; message: string }

type At = { line: number; col: number }
// a key in a map: where it is, its value's node, and that value as data
type Field = { key: string; at: At; node: unknown; value: unknown }

const loose = (s: string) => s.toLowerCase().replace(/[\s_-]/g, '')
const NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const SECTIONS = ['default', 'idps', 'rules', 'policy_points']

// fromCode: the document as a draft, or every error in it (by position).
export function fromCode(src: string, v: AssuranceView, continuity: string): { draft: Draft } | { errors: CodeError[] } {
  const lc = new LineCounter()
  const doc = parseDocument(src, { lineCounter: lc, prettyErrors: false })
  if (doc.errors.length) return { errors: doc.errors.map(e => ({ ...lc.linePos(e.pos[0]), message: e.message.split('\n')[0] })) }
  const errors: CodeError[] = []
  const err = (at: At, message: string) => { errors.push({ line: at.line, col: at.col, message }) }
  const pos = (n: unknown): At => (isNode(n) && n.range ? lc.linePos(n.range[0]) : { line: 1, col: 1 })
  // a map's keys and values; nothing (a key with no value) is an empty map
  const fields = (n: unknown, at: At, where: string): Field[] => {
    if (n === null || n === undefined || (isScalar(n) && n.value === null)) return []
    if (!isMap(n)) { err(at, `${where} is a map: key: value, one per line`); return [] }
    const out: Field[] = []
    for (const p of n.items) {
      const k = isScalar(p.key) ? p.key.value : undefined
      if (typeof k !== 'string' && typeof k !== 'number' && typeof k !== 'boolean') { err(pos(p.key), `${where}: a key is a name`); continue }
      out.push({ key: String(k), at: pos(p.key), node: p.value, value: isNode(p.value) ? (p.value as Node).toJS(doc) : null })
    }
    return out
  }
  const get = (fs: Field[], key: string) => fs.find(f => f.key === key)
  const only = (fs: Field[], allowed: string[], where: string) => {
    for (const f of fs) if (!allowed.includes(f.key)) err(f.at, `${where} has no ${f.key}: it has ${allowed.join(', ')}`)
  }
  const s = v.schema
  const choice = (at: At, field: string, val: unknown, options: string[] | undefined): string | undefined => {
    if (typeof val !== 'string') { err(at, `${field} is text: one of ${(options ?? []).join(', ')}`); return undefined }
    if (!options?.length) return val
    const hit = options.find(o => loose(o) === loose(val))
    if (!hit) err(at, `${field} "${val}" isn't one of ${options.map(o => `"${o}"`).join(', ')}`)
    return hit
  }
  const bool = (at: At, field: string, val: unknown): boolean | undefined => {
    if (typeof val !== 'boolean') { err(at, `${field} is true or false`); return undefined }
    return val
  }
  const text = (at: At, field: string, val: unknown): string | undefined => {
    if (typeof val !== 'string') { err(at, `${field} is text`); return undefined }
    return val
  }
  const list = (at: At, field: string, val: unknown): string[] => {
    if (!Array.isArray(val) || val.some(x => typeof x !== 'string')) { err(at, `${field} is a list of text: [a, b]`); return [] }
    return val as string[]
  }
  const duration = (at: At, field: string, val: unknown): string | undefined => {
    if (val === null) return undefined
    if (typeof val !== 'string' || !DURATION.test(val)) { err(at, `${field} is a duration, like 12h or 90m, or null`); return undefined }
    return val
  }
  const upstreams = v.idps.filter(i => i.type !== 'local').map(i => i.name)
  const idpList = (at: At, field: string, val: unknown): string[] => {
    const names = list(at, field, val)
    for (const n of names) if (!upstreams.includes(n)) err(at, `${field}: "${n}" isn't one of the chain's IdPs (${upstreams.join(', ')})`)
    return names
  }

  const top = doc.contents
  const sections = fields(top, pos(top), 'the document')
  for (const f of sections) if (!SECTIONS.includes(f.key)) err(f.at, `there's no ${f.key}: the document has ${SECTIONS.join(', ')}`)
  const section = (key: string) => { const f = get(sections, key); return f ? fields(f.node, f.at, key) : [] }

  const points: Draft['points'] = {}
  for (const p of v.policyPoints) if (!p.extAuth) points[pointKey(p)] = null
  const draft: Draft = { policy: {}, idps: {}, rules: {}, points }

  const def = get(sections, 'default')
  if (!def) errors.push({ line: 1, col: 1, message: 'default is missing: the chain\'s default rule (default:)' })
  else {
    const fs = section('default')
    only(fs, ['minimum', 'phishing_resistant', 'max_age', 'idps', 'break_glass', 'sessions'], 'default')
    const p: AssurancePolicy = {}
    let a
    if ((a = get(fs, 'minimum')) && a.value !== null) p.minimum = choice(a.at, 'minimum', a.value, s?.levels)
    if ((a = get(fs, 'phishing_resistant')) && bool(a.at, 'phishing_resistant', a.value)) p.phishingResistant = true
    if ((a = get(fs, 'max_age'))) p.maxAge = duration(a.at, 'max_age', a.value)
    if ((a = get(fs, 'idps'))) { const l = idpList(a.at, 'idps', a.value); if (l.length) p.allowedIdPs = l }
    if ((a = get(fs, 'break_glass')) && bool(a.at, 'break_glass', a.value)) p.allowBreakGlass = true
    if ((a = get(fs, 'sessions')) && a.value !== null) p.sessions = choice(a.at, 'sessions', a.value, s?.sessions)
    draft.policy = p
  }

  const seenIdp = new Set<string>()
  for (const it of section('idps')) {
    const name = it.key
    if (!v.idps.some(i => i.name === name)) { err(it.at, `"${name}" isn't one of the chain's IdPs (${v.idps.map(i => i.name).join(', ')}); IdPs are added in the chain itself`); continue }
    seenIdp.add(name)
    const fs = fields(it.node, it.at, `idp "${name}"`)
    only(fs, ['otherwise', 'acr', 'amr'], `idp "${name}"`)
    const ta: TierAssurance = {}
    let a
    if ((a = get(fs, 'otherwise')) && a.value !== null) ta.default = choice(a.at, 'otherwise', a.value, s?.levels)
    for (const claim of ['acr', 'amr'] as const) {
      if (!(a = get(fs, claim))) continue
      if (a.value !== null && !isMap(a.node)) { err(a.at, `${claim} maps each ${claim} value to a level: { <value>: <level> }`); continue }
      for (const e of fields(a.node, a.at, claim)) {
        let level: string | undefined, pr = false
        if (typeof e.value === 'string') level = choice(e.at, `${claim} "${e.key}"`, e.value, s?.levels)
        else if (isMap(e.node)) {
          for (const f of fields(e.node, e.at, `${claim} "${e.key}"`)) {
            if (f.key === 'level') level = choice(f.at, 'level', f.value, s?.levels)
            else if (f.key === 'phishing_resistant') pr = !!bool(f.at, 'phishing_resistant', f.value)
            else err(f.at, `${claim} "${e.key}" has level and phishing_resistant, not ${f.key}`)
          }
          if (!level) err(e.at, `${claim} "${e.key}" needs a level`)
        } else err(e.at, `${claim} "${e.key}" is a level, or { level: <level>, phishing_resistant: true }`)
        if (level) (ta.levels ??= []).push({ [claim]: e.key, level, ...(pr ? { phishingResistant: true } : {}) })
      }
    }
    draft.idps[name] = ta.default || ta.levels ? ta : undefined
  }

  for (const it of section('rules')) {
    const name = it.key
    if (!NAME.test(name)) { err(it.at, `rule takes a name of lowercase letters, digits and -, not "${name}"`); continue }
    if (name === 'default') { err(it.at, 'rule "default": policy_points calls the default rule default; give this rule another name'); continue }
    const fs = fields(it.node, it.at, `rule "${name}"`)
    only(fs, ['description', 'criticality', 'mode', 'owner', 'obligations', 'workloads', 'clients',
      'minimum', 'phishing_resistant', 'max_age', 'idps', 'break_glass', 'sessions'], `rule "${name}"`)
    const r: ProfileSpec = { continuity, criticality: '' }
    let a
    if ((a = get(fs, 'description'))) r.description = text(a.at, 'description', a.value)
    if ((a = get(fs, 'criticality'))) r.criticality = choice(a.at, 'criticality', a.value, s?.criticality) ?? ''
    else err(it.at, `rule "${name}" needs a criticality: ${(s?.criticality ?? []).join(', ')}`)
    if ((a = get(fs, 'mode'))) r.mode = choice(a.at, 'mode', a.value, s?.modes)
    if ((a = get(fs, 'owner'))) r.owner = text(a.at, 'owner', a.value)
    if ((a = get(fs, 'obligations'))) { const l = list(a.at, 'obligations', a.value); if (l.length) r.obligations = l }
    if ((a = get(fs, 'workloads'))) {
      const ws: WorkloadRef[] = []
      for (const w of list(a.at, 'workloads', a.value)) {
        const m = /^([a-z0-9-]+)\/([a-z0-9.-]+)$/.exec(w)
        if (!m) err(a.at, `workloads: "${w}" is <namespace>/<service account>`)
        else ws.push({ namespace: m[1], serviceAccount: m[2] })
      }
      if (ws.length) r.workloads = ws
    }
    if ((a = get(fs, 'clients'))) { const l = list(a.at, 'clients', a.value); if (l.length) r.clients = l }
    const as: NonNullable<ProfileSpec['assurance']> = {}
    if ((a = get(fs, 'minimum')) && a.value !== null) as.minimum = choice(a.at, 'minimum', a.value, s?.levels)
    if ((a = get(fs, 'phishing_resistant')) && a.value !== null) as.phishingResistant = bool(a.at, 'phishing_resistant', a.value)
    if ((a = get(fs, 'max_age'))) as.maxAge = duration(a.at, 'max_age', a.value)
    if (Object.values(as).some(x => x !== undefined)) r.assurance = as
    if ((a = get(fs, 'idps')) && a.value !== null) { const l = idpList(a.at, 'idps', a.value); if (l.length) r.allowedIdPs = l }
    if ((a = get(fs, 'break_glass')) && a.value !== null) r.allowBreakGlass = bool(a.at, 'break_glass', a.value)
    if ((a = get(fs, 'sessions')) && a.value !== null) r.sessions = choice(a.at, 'sessions', a.value, s?.sessions)
    draft.rules[name] = r
  }

  // each gateway policy, and the rule it asks for: one the document has
  const usable = v.policyPoints.filter(p => !p.extAuth).map(p => refOf(v, pointKey(p)))
  const set = new Set<string>()
  for (const it of section('policy_points')) {
    const ref = it.key
    const hits = v.policyPoints.filter(p => !p.extAuth && (`${p.namespace}/${p.name}` === ref || pointKey(p) === ref))
    if (hits.length !== 1) {
      err(it.at, hits.length ? `policy_points: "${ref}" is more than one policy: write it as "<Kind> ${ref}"`
        : `policy_points: "${ref}" isn't a gateway policy that takes the broker's tokens${usable.length ? ` (${usable.join(', ')})` : ''}`)
      continue
    }
    const k = pointKey(hits[0])
    let rule: string | null
    if (it.value === null) rule = null
    else if (it.value === 'default') rule = ''
    else if (typeof it.value === 'string' && Object.hasOwn(draft.rules, it.value)) rule = it.value
    else {
      const names = Object.keys(draft.rules)
      err(it.at, typeof it.value === 'string' ? `policy_points: ${ref} asks for "${it.value}", which isn't one of the rules here${names.length ? ` (${names.join(', ')})` : ''}`
        : `policy_points: ${ref} asks for a rule by name, default (the default rule) or null (none)`)
      continue
    }
    if (set.has(k)) err(it.at, `policy_points: ${ref} is already enforcing ${points[k] === null ? 'nothing' : points[k] || 'the default'}; a policy asks for one rule`)
    set.add(k)
    points[k] = rule
  }

  for (const i of v.idps) if (!seenIdp.has(i.name)) draft.idps[i.name] = undefined
  // a rule the document leaves out is removed, and so is its enforcement
  for (const r of v.rules) if (!(r.name in draft.rules)) draft.rules[r.name] = null
  return errors.length ? { errors } : { draft: normalize(draft) }
}
