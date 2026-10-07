import { useState } from 'react'
import type { Directory } from '../api'
import { useSync } from './syncContext'
import { TestConnection } from './TestConnection'
import { credentialsName, DIRECTORY_URL, kindOf, noFill, suggestDirectory } from './mapping'

const TYPES: { v: Directory['type']; label: string }[] = [
  { v: 'scim', label: 'SCIM 2.0' }, { v: 'auth0', label: 'Auth0 Management API' }, { v: 'keycloak', label: 'Keycloak Admin API' },
]

// DirectoryForm says where the sync reads a tier's user records. The client
// id and secret go straight to a Secret (directory-<tier>) and are never
// read back; they live in this form only until they're written or staged.
// Both are always written together: the server replaces the Secret's keys
// as a set.
export function DirectoryForm({ tier, issuer, dir, onClose }: { tier: string; issuer?: string; dir?: Directory; onClose: () => void }) {
  const c = useSync()
  // a new directory starts from what the IdP's issuer suggests
  const first = dir ? undefined : suggestDirectory(kindOf(issuer), issuer)
  const [type, setType] = useState<Directory['type']>(dir?.type ?? kindOf(issuer))
  const [url, setUrl] = useState(dir?.url ?? first?.url ?? '')
  const [scopes, setScopes] = useState((dir?.scopes ?? []).join(' '))
  const [audience, setAudience] = useState(dir?.audience ?? first?.audience ?? '')
  const pick = (t: Directory['type']) => {
    setType(t)
    const s = suggestDirectory(t, issuer)
    // auth0 needs an audience: start from the issuer's
    if (!audience.trim() && s.audience) setAudience(s.audience)
    if (dir) return
    setUrl(s.url)
  }
  const [id, setId] = useState('')
  const [secret, setSecret] = useState('')
  const [state, setState] = useState<{ ok: boolean; text: string }>()
  const secretName = credentialsName(tier)
  // as the CRD allows: https, or plain http to a cluster Service
  const urlOk = DIRECTORY_URL.test(url.trim())
  const audienceMissing = type === 'auth0' && !audience.trim()

  const apply = () => {
    const creds = !!(id && secret)
    const d: Directory = { type, url: url.trim(), credentialsRef: creds ? { name: secretName } : dir?.credentialsRef ?? { name: secretName } }
    if (dir?.clientID) d.clientID = dir.clientID
    const sc = scopes.split(/[\s,]+/).filter(Boolean)
    if (sc.length) d.scopes = sc
    if (type === 'auth0' && audience.trim()) d.audience = audience.trim()
    c.setDirectory(tier, d)
    if (creds) c.stageCredentials(tier, id, secret)
    onClose()
  }
  const write = async () => {
    setState(undefined)
    try {
      await c.writeCredentials(tier, id, secret)
      setId(''); setSecret('')
      setState({ ok: true, text: `Written to Secret ${secretName}.` })
    } catch (e) { setState({ ok: false, text: (e as Error).message }) }
  }

  return (
    <div className="cm-dir-form nodrag nowheel" onKeyDown={e => { if (e.key === 'Escape') { e.stopPropagation(); onClose() } }}>
      <label>type
        <select className="field" value={type} onChange={e => pick(e.target.value as Directory['type'])}>
          {TYPES.map(t => <option key={t.v} value={t.v}>{t.label}</option>)}
        </select>
      </label>
      <label>url<input className={`field${url && !urlOk ? ' invalid' : ''}`} value={url} placeholder="https://…" onChange={e => setUrl(e.target.value)}
        title="https://…, or http:// only to a cluster Service (…svc)" /></label>
      {url && !urlOk && <span className="small danger-text">https://…, or http:// only to a cluster Service (<span className="mono">&lt;name&gt;.&lt;namespace&gt;.svc</span>)</span>}
      <label>scopes<input className="field" value={scopes} placeholder="optional, space separated" onChange={e => setScopes(e.target.value)} /></label>
      {type === 'auth0' && <label>audience<input className={`field${audienceMissing ? ' invalid' : ''}`} value={audience} placeholder="https://<tenant>/api/v2/" onChange={e => setAudience(e.target.value)} /></label>}
      {audienceMissing && <span className="small danger-text">an Auth0 directory needs the Management API audience</span>}
      <div className="cm-creds">
        <span className="subtle small">credentials · Secret <span className="mono">{secretName}</span> (write-only)</span>
        <input className="field" autoComplete="off" data-1p-ignore data-lpignore="true" placeholder="client id" aria-label="Directory client ID" value={id} onChange={e => setId(e.target.value)} />
        <input className="field" type="password" {...noFill} placeholder="client secret" aria-label="Directory client secret" value={secret} onChange={e => setSecret(e.target.value)} />
        <button className="btn small" disabled={!id || !secret} onClick={write} title="Write the client id and secret to the Secret now (both are needed)">Set credentials</button>
        {state && <span className={`small ${state.ok ? 'subtle' : 'danger-text'}`}>{state.text}</span>}
      </div>
      {dir && <TestConnection idp={tier} />}
      <div className="row">
        {dir && <button className="btn small ghost danger-text" onClick={() => { c.setDirectory(tier, undefined); onClose() }} title="Stop syncing this IdP">Remove</button>}
        <span className="grow" />
        <button className="btn small ghost" onClick={onClose} title="Discard these directory edits">Cancel</button>
        <button className="btn small primary" onClick={apply}
          disabled={!urlOk || audienceMissing || (!!id !== !!secret)}
          title={id && secret ? 'Apply; the credentials are written when you save' : id || secret ? 'Enter both the client id and the secret, or neither' : 'Apply to the working copy'}>Apply</button>
      </div>
    </div>
  )
}
