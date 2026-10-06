import { useState } from 'react'
import type { Directory } from '../api'
import { useClaims } from './claimsContext'
import { TestConnection } from './TestConnection'
import { credentialsName } from './mapping'

const TYPES: { v: Directory['type']; label: string }[] = [
  { v: 'scim', label: 'SCIM 2.0' }, { v: 'auth0', label: 'Auth0 Management API' }, { v: 'keycloak', label: 'Keycloak Admin API' },
]

// DirectoryForm says where the sync reads a tier's user records. The client
// id and secret go straight to a Secret (directory-<tier>) and are never
// read back; they live in this form only until they're written or staged.
// Both are always written together: the server replaces the Secret's keys
// as a set.
export function DirectoryForm({ tier, dir, onClose }: { tier: string; dir?: Directory; onClose: () => void }) {
  const c = useClaims()
  const [type, setType] = useState<Directory['type']>(dir?.type ?? 'scim')
  const [url, setUrl] = useState(dir?.url ?? '')
  const [scopes, setScopes] = useState((dir?.scopes ?? []).join(' '))
  const [audience, setAudience] = useState(dir?.audience ?? '')
  const [id, setId] = useState('')
  const [secret, setSecret] = useState('')
  const [state, setState] = useState<{ ok: boolean; text: string }>()
  const secretName = credentialsName(tier)
  const urlOk = /^https?:\/\/\S+$/.test(url.trim())

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
        <select className="field" value={type} onChange={e => setType(e.target.value as Directory['type'])}>
          {TYPES.map(t => <option key={t.v} value={t.v}>{t.label}</option>)}
        </select>
      </label>
      <label>url<input className={`field${url && !urlOk ? ' invalid' : ''}`} value={url} placeholder="https://…" onChange={e => setUrl(e.target.value)} /></label>
      <label>scopes<input className="field" value={scopes} placeholder="optional, space separated" onChange={e => setScopes(e.target.value)} /></label>
      {type === 'auth0' && <label>audience<input className="field" value={audience} placeholder="https://<tenant>/api/v2/" onChange={e => setAudience(e.target.value)} /></label>}
      <div className="cm-creds">
        <span className="subtle small">credentials · Secret <span className="mono">{secretName}</span> (write-only)</span>
        <input className="field" autoComplete="off" placeholder="client id" value={id} onChange={e => setId(e.target.value)} />
        <input className="field" type="password" autoComplete="off" placeholder="client secret" value={secret} onChange={e => setSecret(e.target.value)} />
        <button className="btn small" disabled={!id || !secret} onClick={write} title="Write the client id and secret to the Secret now (both are needed)">Set credentials</button>
        {state && <span className={`small ${state.ok ? 'subtle' : 'danger-text'}`}>{state.text}</span>}
      </div>
      {dir && <TestConnection tier={tier} />}
      <div className="row">
        {dir && <button className="btn small ghost danger-text" onClick={() => { c.setDirectory(tier, undefined); onClose() }} title="Stop syncing this IdP">Remove</button>}
        <span className="grow" />
        <button className="btn small ghost" onClick={onClose} title="Discard these directory edits">Cancel</button>
        <button className="btn small primary" onClick={apply}
          disabled={!urlOk || (!!id !== !!secret)}
          title={id && secret ? 'Apply; the credentials are written when you save' : id || secret ? 'Enter both the client id and the secret, or neither' : 'Apply to the working copy'}>Apply</button>
      </div>
    </div>
  )
}
