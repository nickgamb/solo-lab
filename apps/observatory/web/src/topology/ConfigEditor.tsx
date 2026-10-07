import { DiffEditor, Editor } from '@monaco-editor/react'
import { useEffect, useState } from 'react'
import { api, ApiError, type Ref } from '../api'
import { monacoTheme as theme } from '../monaco'

const q = (r: Ref) => new URLSearchParams({ apiVersion: r.apiVersion, kind: r.kind, namespace: r.namespace ?? '', name: r.name }).toString()

// ConfigEditor edits one live object. Dry-run shows what the API server
// would store; Apply is a server-side apply made as the signed-in admin.
export function ConfigEditor({ target }: { target: Ref }) {
  const [original, setOriginal] = useState<string>()
  const [text, setText] = useState('')
  const [preview, setPreview] = useState<string>()
  const [msg, setMsg] = useState<{ kind: 'ok' | 'bad'; text: string }>()
  const [busy, setBusy] = useState(false)
  // an apply refused because the object changed since it was loaded, or
  // because another manager owns a field: shown, and applied only if asked
  const [conflict, setConflict] = useState(false)

  const load = () => api<string>(`/api/resource?${q(target)}`).then(y => { setOriginal(y); setText(y); setPreview(undefined); setConflict(false) })
    .catch(e => setMsg({ kind: 'bad', text: String(e.message ?? e) }))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load() }, [])

  const send = async (dry: boolean, force = false) => {
    setBusy(true); setMsg(undefined); setConflict(false)
    const qs = [dry && 'dryRun=true', force && 'force=true'].filter(Boolean).join('&')
    try {
      const out = await api<string>(`/api/resource${qs ? '?' + qs : ''}`, { method: 'POST', body: text, headers: { 'Content-Type': 'application/yaml' } })
      if (dry) { setPreview(out); setMsg({ kind: 'ok', text: 'Dry run accepted. Review what would be stored, then apply.' }) }
      else { setOriginal(out); setText(out); setPreview(undefined); setMsg({ kind: 'ok', text: 'Applied.' }) }
    } catch (e) {
      const conflicted = e instanceof ApiError && e.status === 409
      setConflict(conflicted)
      setMsg({ kind: 'bad', text: conflicted
        ? `Conflict: ${e.message}. Reload to start from the current object, or apply anyway to overwrite it and take over those fields.`
        : String((e as Error).message ?? e) })
    } finally { setBusy(false) }
  }

  if (original === undefined) return msg ? <div className={`note ${msg.kind}`}>{msg.text}</div> : <div className="subtle small">Loading…</div>
  const dirty = text !== original
  return (
    <div className="cfg">
      <div className="monaco">
        {preview !== undefined ? (
          <DiffEditor height="100%" original={original} modified={preview} language="yaml" theme={theme()}
            options={{ readOnly: true, renderSideBySide: false, automaticLayout: true, minimap: { enabled: false }, fontFamily: 'DM Mono', fontSize: 12, scrollBeyondLastLine: false }} />
        ) : (
          <Editor height="100%" value={text} onChange={v => setText(v ?? '')} language="yaml" theme={theme()}
            options={{ minimap: { enabled: false }, fontFamily: 'DM Mono', fontSize: 12, tabSize: 2, scrollBeyondLastLine: false, automaticLayout: true }} />
        )}
      </div>
      {msg && <div className={`note ${msg.kind}`}>{msg.text}</div>}
      <div className="row">
        <button className="btn small ghost" onClick={load} disabled={busy}>Reload</button>
        <span className="grow" />
        {preview !== undefined && <button className="btn small" onClick={() => setPreview(undefined)}>Back to edit</button>}
        <button className="btn small" disabled={busy || !dirty} onClick={() => send(true)}>Dry run</button>
        {conflict && <button className="btn small danger" disabled={busy} onClick={() => send(false, true)}>Apply anyway</button>}
        <button className="btn small primary" disabled={busy || !dirty} onClick={() => send(false)}>Apply</button>
      </div>
    </div>
  )
}
