import type { ModelProvider } from '../api'

// Reading the model chain: names, durations and each provider's health.

export const KIND_LABEL: Record<string, string> = { ollama: 'Ollama', openai: 'OpenAI', anthropic: 'Anthropic' }
export const kindLabel = (p: { kind: string; host?: string }) =>
  p.kind === 'openai' && p.host ? 'OpenAI-compatible' : KIND_LABEL[p.kind] ?? p.kind

// where a provider's calls go: its host, or the cloud API's own
export const endpoint = (p: { kind: string; host?: string; port?: number }) =>
  p.host ? `${p.host}:${p.port}` : p.kind === 'anthropic' ? 'api.anthropic.com' : p.kind === 'openai' ? 'api.openai.com' : ''

// a duration as the gateway writes it (30s, 1m30s, 500ms), in whole seconds
export function seconds(d: string): number {
  let s = 0
  for (const [, n, u] of d.matchAll(/(\d+)(ms|h|m|s)/g)) s += Number(n) * ({ h: 3600, m: 60, s: 1, ms: 0.001 }[u] ?? 0)
  return Math.round(s)
}

export type Health = { cls: 'ok' | 'bad' | 'warn' | 'idle'; text: string }

// health reads a provider's recent calls: cut (a simulated outage), its last
// call failed, it answers, or nothing has gone to it lately.
export function health(p: ModelProvider): Health {
  const s = p.stats
  if (p.outage) return { cls: 'bad', text: 'Outage (simulated)' }
  if (s.calls && (s.lastStatus === 429 || (s.lastStatus ?? 0) >= 500)) return { cls: 'bad', text: `Errors · ${s.errors}/${s.calls}` }
  if (s.calls) return { cls: s.errors ? 'warn' : 'ok', text: s.errors ? `Healthy · ${s.errors}/${s.calls} failed` : `Healthy · ${s.calls} calls` }
  return { cls: 'idle', text: 'Idle' }
}

// serving is the provider that answered the latest call, if one did lately
// and isn't cut since
export function serving(ps: ModelProvider[]): ModelProvider | undefined {
  let best: ModelProvider | undefined
  for (const p of ps) {
    const s = p.stats
    if (p.outage || !s.lastServed || (s.lastStatus ?? 200) >= 400) continue
    if (!best || Date.parse(s.lastServed) > Date.parse(best.stats.lastServed!)) best = p
  }
  return best
}

// a call's cost in dollars, to two significant digits; empty when unpriced
export function usd(v?: string): string {
  const f = Number(v)
  if (v === undefined || v === '' || !Number.isFinite(f) || f < 0) return ''
  if (f === 0) return '$0'
  if (f >= 0.01) return `$${f.toFixed(2)}`
  return `$${f.toFixed(-Math.floor(Math.log10(f)) + 1)}`
}
