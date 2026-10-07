import { memo, useState } from 'react'
import type { Token, Traffic } from '../api'

const time = (s: string) => {
  const d = new Date(s)
  return d.toLocaleTimeString([], { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

const KIND: Record<string, string> = { http: 'HTTP', mcp: 'MCP', a2a: 'A2A', llm: 'LLM', oidc: 'OIDC', lifecycle: 'K8S', substrate: 'SUBSTRATE', continuity: 'IDP', model: 'MODEL' }

// TrafficRow is one entry of the inspector: a summary line that expands to
// the parsed fields and the raw record.
export const TrafficRow = memo(({ t, names, self, compact, onOpenNode }: {
  t: Traffic; names: Map<string, string>; self?: string; compact?: boolean; onOpenNode?: (id: string) => void
}) => {
  const [open, setOpen] = useState(false)
  const [raw, setRaw] = useState(false)
  const nm = (id?: string) => (id ? names.get(id) ?? id.replace(/^wl:/, '') : '')
  const dir = self ? (t.source === self ? 'out' : 'in') : undefined
  const path = [t.source, t.via, t.target].filter(Boolean) as string[]
  return (
    <div className={`trow o-${t.outcome}${open ? ' open' : ''}`}>
      <button className="tline" onClick={() => setOpen(v => !v)}>
        <span className="mono subtle ttime">{time(t.time)}</span>
        <span className={`tdir ${dir ?? ''}`}>{dir === 'out' ? '↗' : dir === 'in' ? '↘' : ''}</span>
        <span className={`chip tk k-${t.kind}`}>{KIND[t.kind] ?? t.kind}</span>
        <span className="grow ellipsis tsum">
          {!!t.tokens?.length && <span className="tkey" title={`carries ${t.tokens.length === 1 ? 'a token' : `${t.tokens.length} tokens`}: expand to inspect`}><KeyIcon />{t.tokens.length > 1 ? t.tokens.length : ''}</span>}
          {t.summary}
        </span>
        {!compact && <span className="subtle ellipsis tpath">{path.map(nm).join(' → ')}</span>}
        {t.status ? <span className={`chip ${t.outcome === 'ok' ? 'ok' : t.outcome === 'info' ? '' : 'bad'}`}>{t.status}</span> : <span className={`chip ${t.outcome === 'error' ? 'bad' : t.outcome === 'ok' ? 'ok' : ''}`}>{t.outcome}</span>}
        {t.durationMs ? <span className="mono subtle tdur">{t.durationMs >= 1000 ? `${(t.durationMs / 1000).toFixed(1)}s` : `${Math.round(t.durationMs)}ms`}</span> : <span className="tdur" />}
      </button>
      {open && (
        <div className="tdetail">
          <div className="row">
            <div className="seg small">
              <button className={!raw ? 'on' : ''} onClick={() => setRaw(false)}>Details</button>
              <button className={raw ? 'on' : ''} onClick={() => setRaw(true)}>Raw JSON</button>
            </div>
          </div>
          {raw ? (
            <pre className="raw scroll">{JSON.stringify(t, null, 2)}</pre>
          ) : (
            <>
            {t.tokens?.map((tk, i) => <TokenCard key={tk.fingerprint ?? i} tk={tk} />)}
            <dl className="kv">
              {path.length > 0 && <><dt>path</dt><dd>{path.map((id, i) => (
                <span key={id + i}>{i > 0 && ' → '}{onOpenNode ? <button className="link" onClick={() => onOpenNode(id)}>{nm(id)}</button> : nm(id)}</span>))}</dd></>}
              {t.identity && <><dt>caller identity</dt><dd className="mono small">{t.identity}</dd></>}
              {t.user && <><dt>user</dt><dd className="mono small">{t.user}</dd></>}
              {t.method && <><dt>request</dt><dd className="mono small">{t.method} {t.path}</dd></>}
              <dt>reporter</dt><dd className="mono small">{t.reporter}</dd>
              {Object.entries(t.attrs ?? {}).filter(([k]) => !k.startsWith('resource.')).map(([k, v]) => (
                <span key={k} style={{ display: 'contents' }}><dt className="mono small">{k}</dt><dd className="mono small">{v}</dd></span>
              ))}
            </dl>
            </>
          )}
        </div>
      )}
    </div>
  )
})

const KeyIcon = () => (
  <svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden><path d="M7 14a3 3 0 1 1 0-6 3 3 0 0 1 0 6zm5.6-4A6 6 0 1 0 12.6 14H15v3h3v-3h2v-4z" /></svg>
)

// the claims people look for first, in reading order
const LEAD: [string, string][] = [['sub', 'subject'], ['preferred_username', 'user'], ['iss', 'issuer'], ['aud', 'audience'],
  ['azp', 'issued to'], ['scope', 'scope'], ['groups', 'groups'], ['act', 'acting for'], ['may_act', 'may act'], ['typ', 'type']]
const TIMES = ['exp', 'iat', 'nbf', 'auth_time']

const show = (v: unknown) => (v && typeof v === 'object' ? (Array.isArray(v) ? v.join(', ') : JSON.stringify(v)) : String(v))
const when = (v: unknown) => {
  const n = Number(v)
  if (!n) return show(v)
  const d = new Date(n * 1000), s = Math.round((d.getTime() - Date.now()) / 1000)
  const rel = Math.abs(s) < 90 ? `${Math.abs(s)}s` : Math.abs(s) < 5400 ? `${Math.round(Math.abs(s) / 60)}m` : `${Math.round(Math.abs(s) / 3600)}h`
  return `${d.toLocaleTimeString([], { hour12: false })} (${s >= 0 ? 'in ' + rel : rel + ' ago'})`
}

// TokenCard: one credential on the request, decoded. Delegation (act) and
// audience are what tell you whose authority the call runs on, and for where.
function TokenCard({ tk }: { tk: Token }) {
  const [all, setAll] = useState(false)
  const c = tk.claims
  const typ = String(tk.header?.typ ?? c.typ ?? '')
  const kind = /id-jag/i.test(typ) ? 'ID-JAG (cross-app assertion)' : c.nonce || c.at_hash ? 'ID token' : 'Access token'
  return (
    <div className="token">
      <div className="row">
        <span className="tkey"><KeyIcon /></span>
        <b>{kind}</b>
        <span className={tk.verified ? 'chip ok' : 'chip'}>{tk.verified ? 'verified' : 'decoded, not verified here'}</span>
        <span className="grow subtle small">{tk.source}</span>
        {tk.fingerprint && <span className="mono subtle small" title="SHA-256 of the token; the token itself is never stored">{tk.fingerprint}</span>}
      </div>
      <dl className="kv">
        {LEAD.filter(([k]) => c[k] !== undefined).map(([k, label]) => (
          <span key={k} style={{ display: 'contents' }}><dt>{label}</dt><dd className="mono small">{show(c[k])}</dd></span>
        ))}
        {TIMES.filter(k => c[k] !== undefined).map(k => (
          <span key={k} style={{ display: 'contents' }}><dt>{k === 'exp' ? 'expires' : k === 'iat' ? 'issued' : k}</dt><dd className="mono small">{when(c[k])}</dd></span>
        ))}
      </dl>
      <button className="btn ghost small" onClick={() => setAll(v => !v)}>{all ? 'Hide claims' : 'All claims'}</button>
      {all && <pre className="raw scroll">{JSON.stringify({ header: tk.header, claims: c }, null, 2)}</pre>}
    </div>
  )
}
