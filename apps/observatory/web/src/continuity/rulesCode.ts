import type { AssurancePolicy, AssuranceView, PolicyPoint, ProfileSpec, RuleSchema, TierAssurance, WorkloadRef } from '../api.ts'
import { type Doc, type HclObject, type Item, type Value, HclError, obj, parse, print } from './hcl.ts'

// The Assurance rules window's working copy, and its Code tab: the same, as
// an HCL document.
//
//   default {                      # the chain's default rule
//     minimum     = "AAL1"
//     idps        = []             # [] : every IdP in the chain
//     break_glass = false
//     enforced_at = []
//   }
//
//   idp "keycloak" {               # what its sign-ins prove
//     otherwise = "AAL1"
//     acr       = { aal2 = "AAL2" }
//     amr       = { hwk = { level = "AAL3", phishing_resistant = true } }
//   }
//
//   rule "ledger" {                # a rule; what it leaves out is the default's
//     criticality = "Critical"
//     mode        = "Enforce"
//     workloads   = ["apps/ledger"]
//     minimum     = "AAL2"
//     enforced_at = ["apps/ledger-caller"]
//   }
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

const levelValue = (l: { level: string; phishingResistant?: boolean }): Value =>
  l.phishingResistant ? obj([['level', l.level], ['phishing_resistant', true]]) : l.level

function idpBody(a?: TierAssurance): Doc {
  const body: Doc = []
  if (a?.default) body.push({ kind: 'attr', key: 'otherwise', value: a.default })
  for (const claim of ['acr', 'amr'] as const) {
    const rows = (a?.levels ?? []).filter(l => l[claim]).map(l => [l[claim]!, levelValue(l)] as [string, Value])
    if (rows.length) body.push({ kind: 'attr', key: claim, value: obj(rows) })
  }
  return body
}

export function refOf(v: AssuranceView, key: string): string {
  const p = v.policyPoints.find(x => pointKey(x) === key)
  if (!p) return key
  const ambiguous = v.policyPoints.filter(x => x.namespace === p.namespace && x.name === p.name).length > 1
  return ambiguous ? key : `${p.namespace}/${p.name}`
}

const enforcedAt = (d: Draft, v: AssuranceView, rule: string): Value =>
  Object.entries(d.points).filter(([, r]) => r === rule).map(([k]) => refOf(v, k)).sort()

export function toCode(d: Draft, v: AssuranceView): string {
  const s = v.schema
  const crit = (c: string) => (s?.criticality.indexOf(c) ?? -1) < 0 ? 99 : s!.criticality.indexOf(c)
  const doc: Doc = [
    { kind: 'comment', text: `${v.namespace}/${v.name}` },
    { kind: 'comment', text: `IdPs, in failover order: ${v.idps.map(i => i.name).join(', ')}${v.active ? `; ${v.active} is active` : ''}` },
    { kind: 'blank' },
  ]
  const p = d.policy
  doc.push({ kind: 'block', type: 'default', labels: [], body: [
    { kind: 'attr', key: 'minimum', value: p.minimum ?? s?.defaults.minimum ?? null },
    { kind: 'attr', key: 'phishing_resistant', value: !!p.phishingResistant },
    { kind: 'attr', key: 'max_age', value: p.maxAge ?? null },
    { kind: 'attr', key: 'idps', value: p.allowedIdPs ?? [] },
    { kind: 'attr', key: 'break_glass', value: !!p.allowBreakGlass },
    { kind: 'attr', key: 'sessions', value: p.sessions ?? s?.defaults.sessions ?? null },
    { kind: 'attr', key: 'enforced_at', value: enforcedAt(d, v, '') },
  ] })
  for (const i of v.idps) {
    doc.push({ kind: 'blank' })
    const notes = [i.displayName && i.displayName !== i.name ? i.displayName : '', i.type, i.name === v.active ? 'active' : '', i.enabled ? '' : 'disabled'].filter(Boolean)
    doc.push({ kind: 'block', type: 'idp', labels: [i.name], body: idpBody(d.idps[i.name]), comment: notes.join(' · ') })
  }
  const names = Object.keys(d.rules).filter(n => d.rules[n]).sort((a, b) => crit(d.rules[a]!.criticality) - crit(d.rules[b]!.criticality) || a.localeCompare(b))
  for (const n of names) {
    const r = d.rules[n]!
    const body: Doc = []
    const put = (key: string, value: Value | undefined) => { if (value !== undefined && value !== '' && !(Array.isArray(value) && !value.length)) body.push({ kind: 'attr', key, value }) }
    put('description', r.description)
    put('criticality', r.criticality)
    put('mode', r.mode)
    put('owner', r.owner)
    put('obligations', r.obligations)
    put('workloads', r.workloads?.map(w => `${w.namespace}/${w.serviceAccount}`))
    put('clients', r.clients)
    const rules: Doc = []
    const rule = (key: string, value: Value | undefined) => { if (value !== undefined) rules.push({ kind: 'attr', key, value }) }
    rule('minimum', r.assurance?.minimum)
    rule('phishing_resistant', r.assurance?.phishingResistant)
    rule('max_age', r.assurance?.maxAge)
    rule('idps', r.allowedIdPs?.length ? r.allowedIdPs : undefined)
    rule('break_glass', r.allowBreakGlass)
    rule('sessions', r.sessions)
    if (rules.length) body.push({ kind: 'blank' }, ...rules)
    body.push({ kind: 'blank' }, { kind: 'attr', key: 'enforced_at', value: enforcedAt(d, v, n) })
    doc.push({ kind: 'blank' }, { kind: 'block', type: 'rule', labels: [n], body })
  }
  return print(doc) + '\n'
}

// --- reading

export type CodeError = { line: number; col: number; message: string }

const loose = (s: string) => s.toLowerCase().replace(/[\s_-]/g, '')
const NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

// fromCode: the document as a draft, or every error in it (by position).
export function fromCode(src: string, v: AssuranceView, continuity: string): { draft: Draft } | { errors: CodeError[] } {
  let items: Item[]
  try {
    items = parse(src)
  } catch (e) {
    if (e instanceof HclError) return { errors: [{ line: e.line, col: e.col, message: e.message }] }
    throw e
  }
  const errors: CodeError[] = []
  const err = (at: { line: number; col: number }, message: string) => { errors.push({ line: at.line, col: at.col, message }) }
  const s = v.schema
  const choice = (at: { line: number; col: number }, field: string, val: Value, options: string[] | undefined): string | undefined => {
    if (typeof val !== 'string') { err(at, `${field} is text: one of ${(options ?? []).map(o => `"${o}"`).join(', ')}`); return undefined }
    if (!options?.length) return val
    const hit = options.find(o => loose(o) === loose(val))
    if (!hit) err(at, `${field} "${val}" isn't one of ${options.map(o => `"${o}"`).join(', ')}`)
    return hit
  }
  const bool = (at: { line: number; col: number }, field: string, val: Value): boolean | undefined => {
    if (typeof val !== 'boolean') { err(at, `${field} is true or false`); return undefined }
    return val
  }
  const text = (at: { line: number; col: number }, field: string, val: Value): string | undefined => {
    if (typeof val !== 'string') { err(at, `${field} is text, in quotes`); return undefined }
    return val
  }
  const list = (at: { line: number; col: number }, field: string, val: Value): string[] => {
    if (!Array.isArray(val) || val.some(x => typeof x !== 'string')) { err(at, `${field} is a list of text: ["a", "b"]`); return [] }
    return val as string[]
  }
  const duration = (at: { line: number; col: number }, field: string, val: Value): string | undefined => {
    if (val === null) return undefined
    if (typeof val !== 'string' || !DURATION.test(val)) { err(at, `${field} is a duration, like "12h" or "90m", or null`); return undefined }
    return val
  }
  const upstreams = v.idps.filter(i => i.type !== 'local').map(i => i.name)
  const idpList = (at: { line: number; col: number }, field: string, val: Value): string[] => {
    const names = list(at, field, val)
    for (const n of names) if (!upstreams.includes(n)) err(at, `${field}: "${n}" isn't one of the chain's IdPs (${upstreams.join(', ')})`)
    return names
  }
  const points: Draft['points'] = {}
  for (const p of v.policyPoints) if (!p.extAuth) points[pointKey(p)] = null
  const enforce = (at: { line: number; col: number }, val: Value, rule: string) => {
    for (const ref of list(at, 'enforced_at', val)) {
      const hits = v.policyPoints.filter(p => !p.extAuth && (`${p.namespace}/${p.name}` === ref || pointKey(p) === ref))
      if (hits.length !== 1) {
        const usable = v.policyPoints.filter(p => !p.extAuth).map(p => refOf(v, pointKey(p)))
        err(at, hits.length ? `enforced_at: "${ref}" is more than one policy: write it as "<Kind> ${ref}"`
          : `enforced_at: "${ref}" isn't a gateway policy that takes the broker's tokens${usable.length ? ` (${usable.join(', ')})` : ''}`)
        continue
      }
      const k = pointKey(hits[0])
      if (points[k] !== null && points[k] !== rule) err(at, `enforced_at: ${ref} is already enforcing ${points[k] || 'the default'}; a policy asks for one rule`)
      points[k] = rule
    }
  }
  const only = (b: { body: Item[] }, allowed: string[], where: string) => {
    for (const it of b.body) {
      if (it.kind === 'block') err(it, `${where} has no ${it.type} block inside it`)
      else if (!allowed.includes(it.key)) err(it, `${where} has no ${it.key}: it has ${allowed.join(', ')}`)
    }
  }
  const attr = (b: { body: Item[] }, key: string) => b.body.find((i): i is Extract<Item, { kind: 'attr' }> => i.kind === 'attr' && i.key === key)

  const draft: Draft = { policy: {}, idps: {}, rules: {}, points }
  let sawDefault = false
  const seenIdp = new Set<string>()
  for (const it of items) {
    if (it.kind === 'attr') { err(it, `${it.key} belongs inside a block: default, idp or rule`); continue }
    if (it.type === 'default') {
      if (sawDefault) { err(it, 'there is one default block'); continue }
      sawDefault = true
      if (it.labels.length) err(it, 'default takes no name: default { ... }')
      only(it, ['minimum', 'phishing_resistant', 'max_age', 'idps', 'break_glass', 'sessions', 'enforced_at'], 'default')
      const p: AssurancePolicy = {}
      let a
      if ((a = attr(it, 'minimum')) && a.value !== null) p.minimum = choice(a, 'minimum', a.value, s?.levels)
      if ((a = attr(it, 'phishing_resistant')) && bool(a, 'phishing_resistant', a.value)) p.phishingResistant = true
      if ((a = attr(it, 'max_age'))) p.maxAge = duration(a, 'max_age', a.value)
      if ((a = attr(it, 'idps'))) { const l = idpList(a, 'idps', a.value); if (l.length) p.allowedIdPs = l }
      if ((a = attr(it, 'break_glass')) && bool(a, 'break_glass', a.value)) p.allowBreakGlass = true
      if ((a = attr(it, 'sessions')) && a.value !== null) p.sessions = choice(a, 'sessions', a.value, s?.sessions)
      if ((a = attr(it, 'enforced_at'))) enforce(a, a.value, '')
      draft.policy = p
    } else if (it.type === 'idp') {
      const name = it.labels[0]
      if (it.labels.length !== 1) { err(it, 'idp takes the IdP\'s name: idp "<name>" { ... }'); continue }
      if (!v.idps.some(i => i.name === name)) { err(it, `"${name}" isn't one of the chain's IdPs (${v.idps.map(i => i.name).join(', ')}); IdPs are added in the chain itself`); continue }
      if (seenIdp.has(name)) { err(it, `idp "${name}" appears twice`); continue }
      seenIdp.add(name)
      only(it, ['otherwise', 'acr', 'amr'], `idp "${name}"`)
      const ta: TierAssurance = {}
      let a
      if ((a = attr(it, 'otherwise')) && a.value !== null) ta.default = choice(a, 'otherwise', a.value, s?.levels)
      for (const claim of ['acr', 'amr'] as const) {
        if (!(a = attr(it, claim))) continue
        const o = a.value
        if (!o || typeof o !== 'object' || Array.isArray(o)) { err(a, `${claim} maps each ${claim} value to a level: { "<value>" = "<level>" }`); continue }
        for (const e of (o as HclObject).entries) {
          let level: string | undefined, pr = false
          if (typeof e.value === 'string') level = choice(e, `${claim} "${e.key}"`, e.value, s?.levels)
          else if (e.value && typeof e.value === 'object' && !Array.isArray(e.value)) {
            for (const f of e.value.entries) {
              if (f.key === 'level') level = choice(f, 'level', f.value, s?.levels)
              else if (f.key === 'phishing_resistant') pr = !!bool(f, 'phishing_resistant', f.value)
              else err(f, `${claim} "${e.key}" has level and phishing_resistant, not ${f.key}`)
            }
            if (!level) err(e, `${claim} "${e.key}" needs a level`)
          } else err(e, `${claim} "${e.key}" is a level, or { level = ..., phishing_resistant = true }`)
          if (level) (ta.levels ??= []).push({ [claim]: e.key, level, ...(pr ? { phishingResistant: true } : {}) })
        }
      }
      draft.idps[name] = ta.default || ta.levels ? ta : undefined
    } else if (it.type === 'rule') {
      const name = it.labels[0]
      if (it.labels.length !== 1 || !NAME.test(name ?? '')) { err(it, 'rule takes a name of lowercase letters, digits and -: rule "<name>" { ... }'); continue }
      if (name in draft.rules) { err(it, `rule "${name}" appears twice`); continue }
      only(it, ['description', 'criticality', 'mode', 'owner', 'obligations', 'workloads', 'clients',
        'minimum', 'phishing_resistant', 'max_age', 'idps', 'break_glass', 'sessions', 'enforced_at'], `rule "${name}"`)
      const r: ProfileSpec = { continuity, criticality: '' }
      let a
      if ((a = attr(it, 'description'))) r.description = text(a, 'description', a.value)
      if ((a = attr(it, 'criticality'))) r.criticality = choice(a, 'criticality', a.value, s?.criticality) ?? ''
      else err(it, `rule "${name}" needs a criticality: ${(s?.criticality ?? []).map(c => `"${c}"`).join(', ')}`)
      if ((a = attr(it, 'mode'))) r.mode = choice(a, 'mode', a.value, s?.modes)
      if ((a = attr(it, 'owner'))) r.owner = text(a, 'owner', a.value)
      if ((a = attr(it, 'obligations'))) { const l = list(a, 'obligations', a.value); if (l.length) r.obligations = l }
      if ((a = attr(it, 'workloads'))) {
        const ws: WorkloadRef[] = []
        for (const w of list(a, 'workloads', a.value)) {
          const m = /^([a-z0-9-]+)\/([a-z0-9.-]+)$/.exec(w)
          if (!m) err(a, `workloads: "${w}" is "<namespace>/<service account>"`)
          else ws.push({ namespace: m[1], serviceAccount: m[2] })
        }
        if (ws.length) r.workloads = ws
      }
      if ((a = attr(it, 'clients'))) { const l = list(a, 'clients', a.value); if (l.length) r.clients = l }
      const as: NonNullable<ProfileSpec['assurance']> = {}
      if ((a = attr(it, 'minimum')) && a.value !== null) as.minimum = choice(a, 'minimum', a.value, s?.levels)
      if ((a = attr(it, 'phishing_resistant')) && a.value !== null) as.phishingResistant = bool(a, 'phishing_resistant', a.value)
      if ((a = attr(it, 'max_age'))) as.maxAge = duration(a, 'max_age', a.value)
      if (Object.values(as).some(x => x !== undefined)) r.assurance = as
      if ((a = attr(it, 'idps')) && a.value !== null) { const l = idpList(a, 'idps', a.value); if (l.length) r.allowedIdPs = l }
      if ((a = attr(it, 'break_glass')) && a.value !== null) r.allowBreakGlass = bool(a, 'break_glass', a.value)
      if ((a = attr(it, 'sessions')) && a.value !== null) r.sessions = choice(a, 'sessions', a.value, s?.sessions)
      if ((a = attr(it, 'enforced_at'))) enforce(a, a.value, name)
      draft.rules[name] = r
    } else err(it, `there's no ${it.type} block: default, idp or rule`)
  }
  if (!sawDefault) errors.push({ line: 1, col: 1, message: 'the default block is missing: default { ... }' })
  for (const i of v.idps) if (!seenIdp.has(i.name)) draft.idps[i.name] = undefined
  // a rule the document leaves out is removed, and so is its enforcement
  for (const r of v.rules) if (!(r.name in draft.rules)) draft.rules[r.name] = null
  return errors.length ? { errors } : { draft: normalize(draft) }
}

