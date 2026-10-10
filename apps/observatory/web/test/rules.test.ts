// The assurance rules' Code tab: the rules as YAML, read back as the same
// draft. Run with: npm test (node --test).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { draftOf, fromCode, shortDuration, toCode, same, type Draft } from '../src/continuity/rulesCode.ts'
import type { AssuranceView } from '../src/api.ts'

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
  assert.match(code, /^default:$/m)
  assert.match(code, /^ {2}# North IdP · oidc · active\n {2}north:$/m)
  assert.match(code, /^ {6}urn:gold: AAL2$/m)
  assert.match(code, /^ {6}hwk: \{level: AAL3, phishing_resistant: true\}$/m)
  assert.match(code, /^ {2}ledger:$/m)
  assert.match(code, /^ {4}max_age: 12h$/m)
  assert.match(code, /^ {4}workloads: \[apps\/ledger\]$/m)
  assert.match(code, /^ {2}apps\/ledger-caller: ledger$/m)
  assert.match(code, /^ {2}apps\/notes-caller: null$/m)
  assert.doesNotMatch(code, /apps\/other/)
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  assert.ok(same((back as { draft: Draft }).draft, d), `${JSON.stringify((back as { draft: Draft }).draft)}\n${JSON.stringify(d)}`)
})

test('rules: an edit, a new rule and enforcement, read back', () => {
  const code = toCode(draftOf(view), view)
    .replace('  break_glass: false', '  break_glass: true')
    .replace('apps/notes-caller: null', 'apps/notes-caller: notes')
    .replace(/^rules:$/m, 'rules:\n  notes: { criticality: silver, mode: report_only, minimum: aal2 }')
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  const d = (back as { draft: Draft }).draft
  assert.equal(d.policy.allowBreakGlass, true)
  assert.deepEqual(d.rules.notes, { continuity: 'firm', criticality: 'Silver', mode: 'ReportOnly', assurance: { minimum: 'AAL2' } })
  assert.equal(d.points['AgentgatewayPolicy apps/notes-caller'], 'notes')
})

test('rules: the default rule, and a policy left out asks for none', () => {
  const code = toCode(draftOf(view), view).replace(/^ {2}apps\/notes-caller: null\n/m, '').replace('apps/ledger-caller: ledger', 'AgentgatewayPolicy apps/ledger-caller: default')
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  const d = (back as { draft: Draft }).draft
  assert.equal(d.points['AgentgatewayPolicy apps/ledger-caller'], '')
  assert.equal(d.points['AgentgatewayPolicy apps/notes-caller'], null)
})

test('rules: a rule left out is removed, with what asked for it', () => {
  const code = toCode(draftOf(view), view).replace(/^ {2}ledger:\n( {4}.*\n|\n)*/m, '').replace('apps/ledger-caller: ledger', 'apps/ledger-caller: null')
  const back = fromCode(code, view, 'firm')
  assert.ok('draft' in back, JSON.stringify(back))
  const d = (back as { draft: Draft }).draft
  assert.equal(d.rules.ledger, null)
  assert.equal(d.points['AgentgatewayPolicy apps/ledger-caller'], null)
  // still asked for: an error, at the policy
  const left = fromCode(toCode(draftOf(view), view).replace(/^ {2}ledger:\n( {4}.*\n|\n)*/m, ''), view, 'firm')
  assert.ok('errors' in left)
  assert.match(left.errors[0].message, /^policy_points: apps\/ledger-caller asks for "ledger", which isn't one of the rules here/)
})

test('rules: errors name the line, the column and the choices', () => {
  const bad = `default:
  minimum: AAL9
  idps: [west]
idps:
  nowhere: {}
rules:
  Bad Name: {}
  x:
    criticality: Gold
    minimun: AAL2
policy_points:
  apps/other: x
  apps/notes-caller: nothing
extra: 1
`
  const back = fromCode(bad, view, 'firm')
  assert.ok('errors' in back)
  const msgs = back.errors.map(e => `${e.line}:${e.col} ${e.message}`)
  const has = (prefix: string) => assert.ok(msgs.some(m => m.startsWith(prefix)), `${prefix}\n${msgs.join('\n')}`)
  has('2:3 minimum "AAL9" isn\'t one of "AAL1", "AAL2", "AAL3"')
  has('3:3 idps: "west" isn\'t one of the chain\'s IdPs (north, south)')
  has('5:3 "nowhere" isn\'t one of the chain\'s IdPs')
  has('7:3 rule takes a name')
  has('10:5 rule "x" has no minimun')
  has('12:3 policy_points: "apps/other" isn\'t a gateway policy')
  has('13:3 policy_points: apps/notes-caller asks for "nothing"')
  has('14:1 there\'s no extra')
})

test('rules: YAML that does not parse says where', () => {
  const back = fromCode('default:\n  minimum: AAL1\n  minimum: AAL2\n', view, 'firm')
  assert.ok('errors' in back)
  assert.deepEqual([back.errors[0].line, back.errors[0].col], [3, 3])
  const r = fromCode('rules: [x\n', view, 'firm')
  assert.ok('errors' in r && r.errors.length)
})
