// The Code tab's language: HCL as people write it, and the assurance rules
// it says. Run with: npm test (node --test, no dependencies).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { parse, print, HclError } from '../src/continuity/hcl.ts'
import { draftOf, fromCode, shortDuration, toCode, same, type Draft } from '../src/continuity/rulesCode.ts'
import type { AssuranceView } from '../src/api.ts'

test('hcl: blocks, attributes, comments, values', () => {
  const items = parse(`# a comment
rule "ledger" {   // another
  minimum = "AAL2"
  clients = ["web",
    "cli",]
  acr     = { "urn:gold" = "AAL2", silver = { level = "AAL1", phishing_resistant = false } }
  max_age = null /* inline */ 
  n = -1.5
}
`)
  assert.equal(items.length, 1)
  const b = items[0]
  assert.equal(b.kind, 'block')
  if (b.kind !== 'block') return
  assert.deepEqual([b.type, b.labels], ['rule', ['ledger']])
  const get = (k: string) => b.body.find(i => i.kind === 'attr' && i.key === k)
  assert.deepEqual((get('clients') as { value: unknown }).value, ['web', 'cli'])
  assert.equal((get('max_age') as { value: unknown }).value, null)
  assert.equal((get('n') as { value: unknown }).value, -1.5)
})

test('hcl: errors say where and what', () => {
  const at = (src: string) => { try { parse(src); return '' } catch (e) { assert.ok(e instanceof HclError); return `${e.line}:${e.col} ${e.message}` } }
  assert.match(at('rule "x" {\n  minimum = AAL2\n}'), /^2:13 AAL2 isn't a value: put text in quotes/)
  assert.match(at('rule "x" {\n  a = 1\n'), /never closed/)
  assert.match(at('a = "x\n'), /string is never closed/)
  assert.match(at('rule "x" {\n  a = 1\n  a = 2\n}'), /^3:3 a is set twice/)
  assert.match(at('a = [1 2]'), /expected , or \]/)
})

test('hcl: prints what it reads, aligned', () => {
  const src = print([{ kind: 'block', type: 'rule', labels: ['x'], body: [
    { kind: 'attr', key: 'minimum', value: 'AAL2' }, { kind: 'attr', key: 'phishing_resistant', value: true }] }])
  assert.equal(src, 'rule "x" {\n  minimum            = "AAL2"\n  phishing_resistant = true\n}')
  assert.deepEqual(parse(src).length, 1)
})

test('durations as people write them', () => {
  assert.equal(shortDuration('12h0m0s'), '12h')
  assert.equal(shortDuration('1h30m0s'), '1h30m')
  assert.equal(shortDuration('90m0s'), '90m')
  assert.equal(shortDuration('0s'), '0s')
})

// a chain whose names have nothing to do with the lab's
const view: AssuranceView = {
  namespace: 'id', name: 'firm', resourceVersion: '1', active: 'north',
  schema: { criticality: ['Gold', 'Silver'], levels: ['AAL1', 'AAL2', 'AAL3'], sessions: ['Any', 'ActiveIdPOnly'], modes: ['Enforce', 'ReportOnly', 'Off'],
    defaults: { mode: 'Enforce', minimum: 'AAL1', sessions: 'Any' } },
  policy: { minimum: 'AAL1', sessions: 'Any' },
  idps: [
    { name: 'north', displayName: 'North IdP', type: 'oidc', enabled: true, healthy: true,
      assurance: { default: 'AAL1', levels: [{ acr: 'urn:gold', level: 'AAL2' }, { amr: 'hwk', level: 'AAL3', phishingResistant: true }] } },
    { name: 'south', type: 'oidc', enabled: true, healthy: true },
    { name: 'admins', type: 'local', enabled: true, healthy: true, assurance: { default: 'AAL1' } },
  ],
  rules: [{ name: 'ledger', resourceVersion: '5', spec: { continuity: 'firm', criticality: 'Gold', mode: 'Enforce', workloads: [{ namespace: 'apps', serviceAccount: 'ledger' }],
    assurance: { minimum: 'AAL2', maxAge: '12h0m0s' }, allowedIdPs: ['north'] } }],
  policyPoints: [
    { apiVersion: 'agentgateway.dev/v1alpha1', kind: 'AgentgatewayPolicy', namespace: 'apps', name: 'ledger-caller', resourceVersion: '3', rule: 'ledger' },
    { apiVersion: 'agentgateway.dev/v1alpha1', kind: 'AgentgatewayPolicy', namespace: 'apps', name: 'notes-caller', resourceVersion: '4', rule: null },
    { apiVersion: 'agentgateway.dev/v1alpha1', kind: 'AgentgatewayPolicy', namespace: 'apps', name: 'other', resourceVersion: '4', rule: null, extAuth: 'x/y' },
  ],
  uncovered: [], options: { workloads: [], clients: [] },
}

test('rules: the document round-trips', () => {
  const d = draftOf(view)
  const code = toCode(d, view)
  assert.match(code, /^default \{$/m)
  assert.match(code, /^idp "north" \{ # North IdP · oidc · active$/m)
  assert.match(code, /"urn:gold" = "AAL2"/)
  assert.match(code, /hwk = \{ level = "AAL3", phishing_resistant = true \}/)
  assert.match(code, /^rule "ledger" \{$/m)
  assert.match(code, /max_age += "12h"/)
  assert.match(code, /enforced_at = \["apps\/ledger-caller"\]/)
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  assert.ok(same((back as { draft: Draft }).draft, d), `${JSON.stringify((back as { draft: Draft }).draft)}\n${JSON.stringify(d)}`)
})

test('rules: an edit, a new rule and enforcement, read back', () => {
  const code = toCode(draftOf(view), view)
    .replace('break_glass        = false', 'break_glass        = true')
    + '\nrule "notes" {\n  criticality = "silver"\n  mode = "report_only"\n  minimum = "aal2"\n  enforced_at = ["apps/notes-caller"]\n}\n'
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  const d = (back as { draft: Draft }).draft
  assert.equal(d.policy.allowBreakGlass, true)
  assert.deepEqual(d.rules.notes, { continuity: 'firm', criticality: 'Silver', mode: 'ReportOnly', assurance: { minimum: 'AAL2' } })
  assert.equal(d.points['AgentgatewayPolicy apps/notes-caller'], 'notes')
})

test('rules: a rule left out is removed, and so is its enforcement', () => {
  const code = toCode(draftOf(view), view).replace(/rule "ledger" \{[\s\S]*\}\n$/, '')
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  const d = (back as { draft: Draft }).draft
  assert.equal(d.rules.ledger, null)
  assert.equal(d.points['AgentgatewayPolicy apps/ledger-caller'], null)
})

test('rules: errors name the line and the choices', () => {
  const bad = `default {
  minimum = "AAL9"
  idps = ["west"]
}
idp "nowhere" {}
rule "Bad Name" {}
rule "x" {
  criticality = "Gold"
  minimun = "AAL2"
  enforced_at = ["apps/other"]
}
`
  const back = fromCode(bad, view, 'firm')
  assert.ok('errors' in back)
  const msgs = (back as { errors: { line: number; message: string }[] }).errors.map(e => `${e.line} ${e.message}`)
  assert.ok(msgs.some(m => m.startsWith('2 minimum "AAL9" isn\'t one of "AAL1", "AAL2", "AAL3"')), msgs.join('\n'))
  assert.ok(msgs.some(m => m.startsWith('3 idps: "west" isn\'t one of the chain\'s IdPs (north, south)')), msgs.join('\n'))
  assert.ok(msgs.some(m => m.startsWith('5 "nowhere" isn\'t one of the chain\'s IdPs')), msgs.join('\n'))
  assert.ok(msgs.some(m => m.startsWith('6 rule takes a name')), msgs.join('\n'))
  assert.ok(msgs.some(m => m.startsWith('9 rule "x" has no minimun')), msgs.join('\n'))
  assert.ok(msgs.some(m => m.startsWith('10 enforced_at: "apps/other" isn\'t a gateway policy')), msgs.join('\n'))
})
