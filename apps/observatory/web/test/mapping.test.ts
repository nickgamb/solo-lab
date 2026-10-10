// The directory sync's Code tab: the attribute mapping as YAML, applied to
// the spec. Run with: npm test.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { applyCode, MappingError, toCode } from '../src/continuity/mappingCode.ts'
import type { ContinuitySpec } from '../src/api.ts'

// a chain whose names have nothing to do with the lab's
const spec = {
  tiers: [
    { name: 'north', type: 'oidc', attributes: [{ attribute: 'email', path: 'mail' }, { attribute: 'team', path: 'meta.team' }] },
    { name: 'south', type: 'oidc', attributes: [{ attribute: 'email', path: 'email' }] },
    { name: 'admins', type: 'local' },
  ],
  profile: { attributes: [{ name: 'team', displayName: 'Team' }] },
} as unknown as ContinuitySpec

const fail = (text: string) => {
  try { applyCode(spec, text) } catch (e) { assert.ok(e instanceof MappingError); return `${e.line}:${e.col} ${e.message}` }
  assert.fail('expected an error')
}

test('mapping: each attribute on one line, IdPs in chain order', () => {
  const code = toCode(spec)
  assert.match(code, /^email: \[north\.mail, south\.email\]$/m)
  assert.match(code, /^team: \[north\.meta\.team\]$/m)
  assert.doesNotMatch(code, /^username:/m)
  assert.equal(applyCode(spec, code), spec)
})

test('mapping: an edit and a new attribute, read back', () => {
  const code = toCode(spec).replace('team: [north.meta.team]', 'team: [north.meta.team, south.team]') + 'region:\n  - north.loc\n'
  const out = applyCode(spec, code)
  assert.deepEqual(out.tiers[1].attributes, [{ attribute: 'email', path: 'email' }, { attribute: 'team', path: 'team' }])
  assert.deepEqual(out.tiers[0].attributes?.map(a => a.attribute), ['email', 'team', 'region'])
  assert.deepEqual(out.profile?.attributes, [{ name: 'team', displayName: 'Team' }, { name: 'region' }])
})

test('mapping: errors name the key and where it is', () => {
  assert.equal(fail('email: [south.email, north.mail]\n'), '1:22 email: list IdPs in chain order (north, south): the primary first')
  assert.equal(fail('email: [north.mail]\nteam:\n  - west.team\n'), '3:5 team[0]: no IdP "west" configured (north, south; add IdPs in the rule builder)')
  assert.equal(fail('email: north.mail\n'), '1:1 email: expected a list of <idp>.<attribute>')
  assert.equal(fail('9lives: []\n'), '1:1 "9lives": an attribute name starts with a letter; letters, digits, _ . - only')
  assert.match(fail('email: []\nemail: []\n'), /^2:1 YAML: /)
  assert.match(fail('- email\n'), /^1:1 expected a map/)
})
