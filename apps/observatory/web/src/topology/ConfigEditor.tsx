import { DiffEditor, Editor, loader } from '@monaco-editor/react'
import * as monaco from 'monaco-editor'
import editorWorker from 'monaco-editor/editor/editor.worker?worker'
import { useEffect, useState } from 'react'
import { api, type Ref } from '../api'

// Monaco from the bundle, not a CDN: the observatory works offline and
// under a strict CSP.
self.MonacoEnvironment = { getWorker: () => new editorWorker() }
loader.config({ monaco })
monaco.editor.defineTheme('solo-dark', {
  base: 'vs-dark', inherit: true, rules: [{ token: 'type', foreground: 'b082fb' }, { token: 'string', foreground: 'e6d6ff' }],
  colors: { 'editor.background': '#12012a', 'editor.lineHighlightBackground': '#1f0c40', 'editorGutter.background': '#12012a' },
})

const theme = () => (document.documentElement.dataset.theme === 'light' ? 'vs' : 'solo-dark')
const q = (r: Ref) => new URLSearchParams({ apiVersion: r.apiVersion, kind: r.kind, namespace: r.namespace ?? '', name: r.name }).toString()

// ConfigEditor edits one live object. Dry-run shows what the API server
// would store; Apply is a server-side apply made as the signed-in admin.
export function ConfigEditor({ target }: { target: Ref }) {
  const [original, setOriginal] = useState<string>()
  const [text, setText] = useState('')
  const [preview, setPreview] = useState<string>()
  const [msg, setMsg] = useState<{ kind: 'ok' | 'bad'; text: string }>()
  const [busy, setBusy] = useState(false)

  const load = () => api<string>(`/api/resource?${q(target)}`).then(y => { setOriginal(y); setText(y); setPreview(undefined) })
    .catch(e => setMsg({ kind: 'bad', text: String(e.message ?? e) }))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load() }, [])

  const send = async (dry: boolean) => {
    setBusy(true); setMsg(undefined)
    try {
      const out = await api<string>(`/api/resource${dry ? '?dryRun=true' : ''}`, { method: 'POST', body: text, headers: { 'Content-Type': 'application/yaml' } })
      if (dry) { setPreview(out); setMsg({ kind: 'ok', text: 'Dry run accepted. Review what would be stored, then apply.' }) }
      else { setOriginal(out); setText(out); setPreview(undefined); setMsg({ kind: 'ok', text: 'Applied.' }) }
    } catch (e) {
      setMsg({ kind: 'bad', text: String((e as Error).message ?? e) })
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
        <button className="btn small primary" disabled={busy || !dirty} onClick={() => send(false)}>Apply</button>
      </div>
    </div>
  )
}
