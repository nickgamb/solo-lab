// The routing rules' Code view: raw CEL under a rule header, read back as
// it was written. Run with: npm test.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fromCode, toCode, sameRules, type RoutingRule } from '../src/continuity/routingCode.ts'

const rules: RoutingRule[] = [
  { name: 'ledgerline', idps: ['gluu', 'okta', 'keycloak'], description: 'Sign-ins for Ledgerline',
    when: 'request.uri.matches("[?&]resource=https(%3A|:)(%2F|/){2}mcp.sterling.lab")' },
  { name: 'ai-clients', idps: ['keycloak'], when: 'request.uri.contains("client_id=sv-mcp-client") &&\n  request.headers["user-agent"] != ""' },
]

test('routing: round trip keeps CEL as written', () => {
  const back = fromCode(toCode(rules))
  assert.deepEqual(back.errors, [])
  assert.ok(back.rules && sameRules(back.rules, rules))
  assert.equal(back.rules![1].when, rules[1].when)
})

test('routing: errors by line, unknown IdPs only warned', () => {
  const r = fromCode('rule Bad -> \n  true\n  # trailing note\nnot a rule\n', ['keycloak'])
  assert.equal(r.rules, undefined)
  assert.deepEqual(r.errors.map(e => e.line), [1, 1, 4])
  const w = fromCode('rule a -> nope, keycloak\n  true\n', ['keycloak'])
  assert.equal(w.rules?.length, 1)
  assert.match(w.errors[0].message, /isn't in the chain/)
  assert.match(fromCode('rule a -> keycloak\n').errors[0].message, /no CEL/)
})
