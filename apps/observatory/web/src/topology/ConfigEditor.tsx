import { DiffEditor, Editor } from '@monaco-editor/react'
import { useEffect, useState } from 'react'
import { api, ApiError, type Ref } from '../api'
import { monacoTheme as theme } from '../monaco'

const q = (r: Ref) => new URLSearchParams({ apiVersion: r.apiVersion, kind: r.kind, namespace: r.namespace ?? '', name: r.name })
const label = (r: Ref) => `${r.kind} ${r.namespace ? `${r.namespace}/` : ''}${r.name}`
const unquote = (v: string) => v.replace(/\s+#.*$/, '').trim().replace(/^(['"])(.*)\1$/, '$2')

// Which object a YAML document names: its apiVersion, kind and
// metadata.namespace/name, read from block-style YAML (as the editor loads
// it). undefined when it isn't one such document.
function named(text: string): Ref | undefined {
  const out: Partial<Ref> = {}
  let inMeta = false, metaIndent = -1, docs = 0
  for (const line of text.split('\n')) {
    if (/^---\s*$/.test(line)) { if (docs++ > 0 || Object.keys(out).length) return undefined; continue }
    if (!line.trim() || /^\s*#/.test(line)) continue
    const top = /^([A-Za-z]\w*):(.*)$/.exec(line)
    if (top) {
      inMeta = top[1] === 'metadata' && !top[2].trim()
      metaIndent = -1
      if (top[1] === 'apiVersion') out.apiVersion = unquote(top[2])
      if (top[1] === 'kind') out.kind = unquote(top[2])
      continue
    }
    if (!inMeta) continue
    const m = /^(\s+)([A-Za-z]\w*):(.*)$/.exec(line)
    if (!m) continue
    if (metaIndent < 0) metaIndent = m[1].length
    if (m[1].length !== metaIndent) continue
    if (m[2] === 'name') out.name = unquote(m[3])
    if (m[2] === 'namespace') out.namespace = unquote(m[3])
  }
  return out.apiVersion && out.kind && out.name ? (out as Ref) : undefined
}

// what stops an Apply: the document names another object than the one opened
function mismatch(text: string, target: Ref): string | undefined {
  const n = named(text)
  if (!n) return `Apply is blocked: the document must be one object with apiVersion, kind and metadata.name (${label(target)}).`
  const diff = [
    n.apiVersion !== target.apiVersion && `apiVersion ${n.apiVersion}`,
    n.kind !== target.kind && `kind ${n.kind}`,
    (n.namespace ?? '') !== (target.namespace ?? '') && `namespace ${n.namespace || '(none)'}`,
    n.name !== target.name && `name ${n.name}`,
  ].filter(Boolean)
  return diff.length ? `Apply is blocked: this editor applies only ${label(target)}, and the document names ${diff.join(', ')}.` : undefined
}

// ConfigEditor edits one live object. Dry-run shows what the API server
// would store; Apply is a server-side apply made as the signed-in admin, of
// that object only.
export function ConfigEditor({ target }: { target: Ref }) {
  const [original, setOriginal] = useState<string>()
  const [text, setText] = useState('')
  const [preview, setPreview] = useState<string>()
  // the text a dry run last accepted: Apply without one asks first
  const [checked, setChecked] = useState<string>()
  const [msg, setMsg] = useState<{ kind: 'ok' | 'bad'; text: string }>()
  const [busy, setBusy] = useState(false)
  // an apply refused because the object changed since it was loaded, or
  // because another manager owns a field: shown, and applied only if asked
  const [conflict, setConflict] = useState(false)

  const load = () => api<string>(`/api/resource?${q(target)}`).then(y => { setOriginal(y); setText(y); setPreview(undefined); setChecked(undefined); setConflict(false) })
    .catch(e => setMsg({ kind: 'bad', text: String(e.message ?? e) }))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { load() }, [])

  const send = async (dry: boolean, force = false) => {
    if (!dry && checked !== text && !window.confirm(`Apply ${label(target)} without a dry run?`)) return
    setBusy(true); setMsg(undefined); setConflict(false)
    // the server refuses a document naming another object than this one
    const qs = q(target)
    if (dry) qs.set('dryRun', 'true')
    if (force) qs.set('force', 'true')
    try {
      const out = await api<string>(`/api/resource?${qs}`, { method: 'POST', body: text, headers: { 'Content-Type': 'application/yaml' } })
      if (dry) { setPreview(out); setChecked(text); setMsg({ kind: 'ok', text: 'Dry run accepted. Review what would be stored, then apply.' }) }
      else { setOriginal(out); setText(out); setPreview(undefined); setChecked(undefined); setMsg({ kind: 'ok', text: 'Applied.' }) }
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
  const blocked = dirty ? mismatch(text, target) : undefined
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
      {blocked && <div className="note bad">{blocked}</div>}
      {msg && <div className={`note ${msg.kind}`}>{msg.text}</div>}
      <div className="row">
        <button className="btn small ghost" onClick={load} disabled={busy}>Reload</button>
        <span className="grow" />
        {preview !== undefined && <button className="btn small" onClick={() => setPreview(undefined)}>Back to edit</button>}
        <button className="btn small" disabled={busy || !dirty || !!blocked} onClick={() => send(true)}>Dry run</button>
        {conflict && <button className="btn small danger" disabled={busy || !!blocked} onClick={() => send(false, true)}>Apply anyway</button>}
        <button className="btn small primary" disabled={busy || !dirty || !!blocked} onClick={() => send(false)}
          title={checked === text ? `Apply ${label(target)}` : `Apply ${label(target)}; a dry run first shows what would be stored`}>Apply</button>
      </div>
    </div>
  )
}
