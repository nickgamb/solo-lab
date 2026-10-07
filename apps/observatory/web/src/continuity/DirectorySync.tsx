import { lazy, Suspense, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { api, isConflict, putContinuity, putSecret, type ContinuitySpec, type IdentityContinuity } from '../api'
import { SyncCanvas } from './SyncCanvas'
import { SyncContext, type SyncActions, type TestResult } from './syncContext'
import { cronError } from './cron'
import * as m from './mapping'
import { applyCode, toCode } from './mappingCode'
import { ScheduleTab } from './ScheduleTab'
import { Guard } from '../topology/DetailsPanel'

// Monaco is large; the Code tab loads it the first time it opens.
const CodeEditor = lazy(() => import('./CodeEditor'))

type Tab = 'canvas' | 'code' | 'schedule'
type Creds = { id: string; secret: string }
const TABS: [Tab, string][] = [['canvas', 'Canvas'], ['code', 'Code'], ['schedule', 'Schedule']]

// DirectorySync edits the directory sync: how each IdP's profile attributes
// pair with S&V's profile on the broker (the primary's read in, the
// failovers' written out), where each IdP's directory is, and when the sync
// runs. It works on a copy of the spec; Save writes the whole spec back, so
// every field it doesn't edit passes through as it was.
export function DirectorySync({ ic, onClose, onSaved }: { ic: IdentityContinuity; onClose: () => void; onSaved?: (ic: IdentityContinuity) => void }) {
  const ns = ic.metadata.namespace
  const [spec, setSpec] = useState<ContinuitySpec>(() => m.clone(ic.spec))
  const [base, setBase] = useState(() => m.stable(ic.spec))
  // the resourceVersion the working copy started from: Save is refused if
  // the object changed since
  const [rv, setRv] = useState(ic.metadata.resourceVersion)
  // each IdP's attribute schema, as its directory test read it
  const [schemas, setSchemas] = useState<Record<string, string[]>>({})
  // staged directory credentials, written on Save; a ref so they never sit in rendered state
  const creds = useRef(new Map<string, Creds>())
  const [credsVersion, setCredsVersion] = useState(0)
  const positions = useRef(new Map<string, { x: number; y: number }>())
  const [tab, setTab] = useState<Tab>('canvas')
  const [code, setCode] = useState('')
  const [codeBase, setCodeBase] = useState('')
  const [codeErr, setCodeErr] = useState<string>()
  const [msg, setMsg] = useState<{ ok: boolean; text: string; conflict?: boolean }>()
  const [busy, setBusy] = useState(false)
  const dialog = useRef<HTMLDivElement>(null)
  const latest = useRef(spec)
  useEffect(() => { latest.current = spec }, [spec])
  // false once the window has closed: a running directory test stops polling
  const open = useRef(true)
  useEffect(() => { open.current = true; return () => { open.current = false } }, [])

  const pending = useMemo(() => new Set(creds.current.keys()), [credsVersion]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = m.stable(spec) !== base || pending.size > 0 || (tab === 'code' && code !== codeBase)
  const schedErr = spec.sync ? cronError(spec.sync.schedule) : undefined

  // start over from an object: the live one, or the one a save stored
  const start = (o: IdentityContinuity) => {
    const s = m.clone(o.spec)
    setSpec(s); setBase(m.stable(o.spec)); setRv(o.metadata.resourceVersion); setCodeErr(undefined)
    if (tab === 'code') { const y = toCode(s); setCode(y); setCodeBase(y) }
  }

  // follow the live object unless there are local edits (an open Code tab
  // with no edits of its own follows too)
  useEffect(() => {
    if (dirty) return
    setRv(ic.metadata.resourceVersion)
    if (m.stable(ic.spec) !== base) start(ic)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ic.spec, ic.metadata.resourceVersion])

  const writeCredentials = async (tier: string, id: string, secret: string) => {
    const name = m.credentialsName(tier)
    // both keys in one write: the server replaces the Secret's keys as a set
    await putSecret(ns, name, { data: { 'client-id': id, 'client-secret': secret }, for: 'directory' })
    if (creds.current.delete(tier)) setCredsVersion(v => v + 1)
    setSpec(s => {
      const d = s.tiers.find(t => t.name === tier)?.directory
      return d && d.credentialsRef?.name !== name ? m.setDirectory(s, tier, { ...d, credentialsRef: { name } }) : s
    })
  }

  // a directory test runs as the sync itself (a Job), so it proves the path
  // the sync will take; poll its Job for the result
  const testDirectory = async (tier: string): Promise<TestResult> => {
    try {
      const { job } = await api<{ job: string }>(`/api/continuity/${ns}/${ic.metadata.name}/directory-test`, { method: 'POST', body: JSON.stringify({ tier }) })
      for (let i = 0; i < 60; i++) {
        await new Promise(r => setTimeout(r, 2000))
        if (!open.current) return { ok: false, text: 'window closed' }
        const j = await api<{ done: boolean; ok?: boolean; result?: { ok: boolean; users: number; message: string; attributes?: string[] } }>(`/api/continuity/${ns}/jobs/${job}`)
        if (!j.done) continue
        if (!open.current) return { ok: false, text: 'window closed' }
        const attrs = j.result?.attributes ?? []
        if (attrs.length) setSchemas(x => ({ ...x, [tier]: attrs }))
        const ok = j.result?.ok ?? !!j.ok
        const text = j.result?.message ?? (ok ? 'connected' : `test job ${job} failed`)
        return { ok, text: attrs.length ? `${text}; ${attrs.length} attributes listed` : text, attributes: attrs }
      }
      return { ok: false, text: `test job ${job} still running after 2 minutes` }
    } catch (e) { return { ok: false, text: (e as Error).message } }
  }

  const actions = useMemo<SyncActions>(() => ({
    setDirectory: (tier, d) => setSpec(s => m.setDirectory(s, tier, d)),
    stageCredentials: (tier, id, secret) => { creds.current.set(tier, { id, secret }); setCredsVersion(v => v + 1) },
    writeCredentials,
    addAttribute: a => {
      const err = m.attributeError(a.name, latest.current)
      if (!err) setSpec(s => m.addAttribute(s, a))
      return err
    },
    updateAttribute: a => setSpec(s => m.updateAttribute(s, a)),
    removeAttribute: name => setSpec(s => m.removeAttribute(s, name)),
    removeMapping: (tier, attribute) => setSpec(s => m.removeMapping(s, tier, attribute)),
    directorySaved: tier => {
      const saved = ic.spec.tiers.find(t => t.name === tier)?.directory
      const now = latest.current.tiers.find(t => t.name === tier)?.directory
      return !!saved && m.stable(saved) === m.stable(now) && !creds.current.has(tier)
    },
    testDirectory,
  }), [ns, ic.spec]) // eslint-disable-line react-hooks/exhaustive-deps

  // leaving the Code tab applies it; a bad document keeps you there
  const fromCode = (): ContinuitySpec | undefined => {
    if (code === codeBase) return spec
    try { const s = applyCode(spec, code); setSpec(s); setCodeErr(undefined); return s } catch (e) { setCodeErr((e as Error).message); return undefined }
  }
  const go = (next: Tab): boolean => {
    if (next === tab) return true
    const s = tab === 'code' ? fromCode() : spec
    if (!s) return false
    if (next === 'code') { const y = toCode(s); setCode(y); setCodeBase(y); setCodeErr(undefined) }
    setTab(next)
    return true
  }
  // arrow keys move along the tabs
  const onTabKey = (e: ReactKeyboardEvent) => {
    const i = TABS.findIndex(([t]) => t === tab)
    const j = e.key === 'ArrowRight' ? (i + 1) % TABS.length : e.key === 'ArrowLeft' ? (i + TABS.length - 1) % TABS.length
      : e.key === 'Home' ? 0 : e.key === 'End' ? TABS.length - 1 : -1
    if (j < 0) return
    e.preventDefault()
    if (go(TABS[j][0])) document.getElementById(`cm-tab-${TABS[j][0]}`)?.focus()
  }

  const save = async () => {
    let s = spec
    if (tab === 'code') {
      const applied = fromCode()
      if (!applied) return
      s = applied
    }
    setBusy(true); setMsg(undefined)
    try {
      const out = await putContinuity(ns, ic.metadata.name, rv, s)
      start(out)
      onSaved?.(out)
      // the directories are saved: now their staged credentials (one that
      // fails stays staged, and Save tries it again)
      for (const [tier, c] of [...creds.current]) {
        if (out.spec.tiers.find(t => t.name === tier)?.directory) await writeCredentials(tier, c.id, c.secret)
        else if (creds.current.delete(tier)) setCredsVersion(v => v + 1)
      }
      setMsg({ ok: true, text: 'Saved. The sync uses it on its next run.' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message, conflict: isConflict(e) })
    } finally { setBusy(false) }
  }

  const reset = () => {
    start(ic); setMsg(undefined)
    creds.current.clear(); setCredsVersion(v => v + 1)
  }

  const close = () => {
    if (dirty && !window.confirm('Discard the unsaved directory sync changes?')) return
    creds.current.clear()
    onClose()
  }

  // Escape closes; inputs that use Escape stop it
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      close()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })

  // the page behind the window is inert while it's open; focus moves into
  // the window, and back to whatever opened it
  useEffect(() => {
    const root = document.getElementById('root')
    const prev = document.activeElement as HTMLElement | null
    root?.setAttribute('inert', '')
    dialog.current?.focus()
    return () => { root?.removeAttribute('inert'); prev?.focus() }
  }, [])

  return createPortal(
    <div className="cm-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <div className="cm-window" role="dialog" aria-modal="true" aria-labelledby="cm-title" tabIndex={-1} ref={dialog}>
        <header className="cm-header">
          <h2 id="cm-title">Directory sync</h2>
          <span className="mono subtle">{ns}/{ic.metadata.name}</span>
          <div className="tabs" role="tablist" aria-label="Directory sync views" onKeyDown={onTabKey}>
            {TABS.map(([t, label]) => (
              <button key={t} id={`cm-tab-${t}`} role="tab" aria-selected={tab === t} aria-controls={`cm-panel-${t}`} tabIndex={tab === t ? 0 : -1}
                className={tab === t ? 'tab active' : 'tab'} onClick={() => go(t)}
                title={t === 'code' ? 'Advanced: the attribute mapping as JSON' : `${label} view`}>
                {label}{t === 'code' && <span className="cm-adv">advanced</span>}
              </button>
            ))}
          </div>
          <span className="grow" />
          {dirty && <span className="chip warn">unsaved</span>}
          <button className="btn small ghost" disabled={!dirty || busy} onClick={reset} title="Discard edits and start from the live object">Reset</button>
          <button className="btn small primary" disabled={!dirty || busy || !!schedErr} onClick={save}
            title={schedErr ? `Fix the schedule first: ${schedErr}` : 'Write the spec to the cluster'}>Save</button>
          <button className="btn small" onClick={close} title="Close (Esc)">Close</button>
        </header>
        {msg && (
          <div className={`note ${msg.ok ? 'ok' : 'bad'} cm-msg`}>
            {msg.text}
            {msg.conflict && <> <button className="btn small" onClick={reset} title="Discard these edits and start from the live object">Reload</button></>}
          </div>
        )}
        <SyncContext.Provider value={actions}>
          <div className="cm-body" id={`cm-panel-${tab}`} role="tabpanel" aria-labelledby={`cm-tab-${tab}`}>
            {tab === 'canvas' && (
              <SyncCanvas ic={ic} spec={spec} schemas={schemas} pending={pending} positions={positions.current}
                onMap={(idp, path, attribute) => setSpec(s => m.setMapping(s, idp, path, attribute))} />
            )}
            {tab === 'code' && (
              <div className="cm-code">
                <p className="subtle small">Each S&amp;V profile attribute, and each IdP's attribute paired with it as <span className="mono">"idp.attribute"</span>, in chain order: the primary's is read into the profile, the failovers' are written from it. A new key adds an attribute to the profile. Leaving this tab applies it.</p>
                <div className="monaco">
                  <Guard>
                    <Suspense fallback={<div className="subtle small">Loading editor…</div>}>
                      <CodeEditor value={code} onChange={setCode} />
                    </Suspense>
                  </Guard>
                </div>
                {codeErr && <div className="note bad" role="alert">{codeErr}</div>}
              </div>
            )}
            {tab === 'schedule' && <ScheduleTab ic={ic} spec={spec} setSpec={setSpec} dirty={dirty} />}
          </div>
        </SyncContext.Provider>
      </div>
    </div>,
    document.body,
  )
}
