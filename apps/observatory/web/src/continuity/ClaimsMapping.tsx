import { Editor } from '@monaco-editor/react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { api, type ContinuitySpec, type IdentityContinuity } from '../api'
import { monacoTheme } from '../monaco'
import { ClaimsCanvas } from './ClaimsCanvas'
import { ClaimsContext, type ClaimsActions } from './claimsContext'
import { cronError } from './cron'
import * as m from './mapping'
import { applyYaml, toYaml } from './mappingYaml'
import { ScheduleTab } from './ScheduleTab'

type Tab = 'canvas' | 'code' | 'schedule'
type Creds = { id: string; secret: string }

// ClaimsMapping edits how each IdP's claims land in the broker's unified
// profile, where the scheduled sync reads each tier's directory, and when it
// runs. It works on a copy of the spec; Save writes the whole spec back, so
// every field it doesn't edit passes through as it was.
export function ClaimsMapping({ ic, onClose }: { ic: IdentityContinuity; onClose: () => void }) {
  const ns = ic.metadata.namespace
  const [spec, setSpec] = useState<ContinuitySpec>(() => m.clone(ic.spec))
  const [base, setBase] = useState(() => m.stable(ic.spec))
  const [extras, setExtras] = useState<Record<string, string[]>>({})
  // staged directory credentials, written on Save; a ref so they never sit in rendered state
  const creds = useRef(new Map<string, Creds>())
  const [credsVersion, setCredsVersion] = useState(0)
  const positions = useRef(new Map<string, { x: number; y: number }>())
  const [tab, setTab] = useState<Tab>('canvas')
  const [yaml, setYaml] = useState('')
  const [yamlBase, setYamlBase] = useState('')
  const [yamlErr, setYamlErr] = useState<string>()
  const [openEdge, setOpenEdge] = useState<string>()
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const [busy, setBusy] = useState(false)
  const dialog = useRef<HTMLDivElement>(null)
  const latest = useRef(spec)
  useEffect(() => { latest.current = spec }, [spec])

  const pending = useMemo(() => new Set(creds.current.keys()), [credsVersion]) // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = m.stable(spec) !== base || pending.size > 0 || (tab === 'code' && yaml !== yamlBase)
  const schedErr = spec.sync ? cronError(spec.sync.schedule) : undefined

  // follow the live object unless there are local edits
  useEffect(() => {
    const live = m.stable(ic.spec)
    if (!dirty && live !== base) { setSpec(m.clone(ic.spec)); setBase(live) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ic.spec])

  const writeCredentials = async (tier: string, id: string, secret: string) => {
    const name = m.credentialsName(tier)
    // both keys in one write: the server replaces the Secret's keys as a set
    await api(`/api/continuity/${ns}/secret`, { method: 'PUT', body: JSON.stringify({ name, data: { 'client-id': id, 'client-secret': secret }, for: 'directory' }) })
    if (creds.current.delete(tier)) setCredsVersion(v => v + 1)
    setSpec(s => {
      const d = s.tiers.find(t => t.name === tier)?.directory
      return d ? m.setDirectory(s, tier, { ...d, credentialsRef: { name } }) : s
    })
  }

  // a directory test runs as the sync itself (a Job), so it proves the path
  // the sync will take; poll its Job for the result
  const testDirectory = async (tier: string) => {
    try {
      const { job } = await api<{ job: string }>(`/api/continuity/${ns}/${ic.metadata.name}/directory-test`, { method: 'POST', body: JSON.stringify({ tier }) })
      for (let i = 0; i < 60; i++) {
        await new Promise(r => setTimeout(r, 2000))
        const j = await api<{ done: boolean; ok?: boolean; result?: { ok: boolean; users: number; message: string } }>(`/api/continuity/${ns}/jobs/${job}`)
        if (j.done) return { ok: !!j.result?.ok, text: j.result?.message ?? (j.ok ? 'connected' : `test job ${job} failed`) }
      }
      return { ok: false, text: `test job ${job} still running after 2 minutes` }
    } catch (e) { return { ok: false, text: (e as Error).message } }
  }

  const actions = useMemo<ClaimsActions>(() => ({
    addClaim: (tier, claim) => setExtras(x => ({ ...x, [tier]: [...(x[tier] ?? []), claim] })),
    dropClaim: (tier, claim) => setExtras(x => ({ ...x, [tier]: (x[tier] ?? []).filter(c => c !== claim) })),
    setDirectory: (tier, d) => setSpec(s => m.setDirectory(s, tier, d)),
    stageCredentials: (tier, id, secret) => { creds.current.set(tier, { id, secret }); setCredsVersion(v => v + 1) },
    writeCredentials,
    addAttribute: name => {
      const err = m.attributeError(name, latest.current)
      if (!err) setSpec(s => m.addAttribute(s, name))
      return err
    },
    removeAttribute: name => setSpec(s => m.removeAttribute(s, name)),
    toggleMultivalued: name => setSpec(s => m.toggleMultivalued(s, name)),
    removeMapping: (tier, attribute) => { setSpec(s => m.removeMapping(s, tier, attribute)); setOpenEdge(undefined) },
    setPath: (tier, attribute, path) => setSpec(s => m.setDirectoryPath(s, tier, attribute, path)),
    openEdge: setOpenEdge,
    setTokenClients: list => setSpec(s => m.withProfile(s, { ...s.profile, tokenClients: list })),
    directorySaved: tier => {
      const saved = ic.spec.tiers.find(t => t.name === tier)?.directory
      const now = latest.current.tiers.find(t => t.name === tier)?.directory
      return !!saved && m.stable(saved) === m.stable(now) && !creds.current.has(tier)
    },
    testDirectory,
  }), [ns, ic.spec]) // eslint-disable-line react-hooks/exhaustive-deps

  // leaving the Code tab applies it; a bad document keeps you there
  const fromCode = (): ContinuitySpec | undefined => {
    if (yaml === yamlBase) return spec
    try { const s = applyYaml(spec, yaml); setSpec(s); setYamlErr(undefined); return s } catch (e) { setYamlErr((e as Error).message); return undefined }
  }
  const go = (next: Tab) => {
    if (next === tab) return
    const s = tab === 'code' ? fromCode() : spec
    if (!s) return
    if (next === 'code') { const y = toYaml(s); setYaml(y); setYamlBase(y); setYamlErr(undefined) }
    setOpenEdge(undefined)
    setTab(next)
  }

  const save = async () => {
    let s = spec
    if (tab === 'code') {
      const applied = fromCode()
      if (!applied) return
      s = applied
      const y = toYaml(s); setYaml(y); setYamlBase(y)
    }
    setBusy(true); setMsg(undefined)
    try {
      for (const [tier, c] of [...creds.current]) {
        if (s.tiers.find(t => t.name === tier)?.directory) await writeCredentials(tier, c.id, c.secret)
      }
      creds.current.clear(); setCredsVersion(v => v + 1)
      await api(`/api/continuity/${ns}/${ic.metadata.name}`, { method: 'PUT', body: JSON.stringify(s) })
      setBase(m.stable(s))
      setMsg({ ok: true, text: 'Saved. Sign-in uses the new mappings now; the sync uses them on its next run.' })
    } catch (e) { setMsg({ ok: false, text: (e as Error).message }) } finally { setBusy(false) }
  }

  const reset = () => {
    const s = m.clone(ic.spec)
    setSpec(s); setBase(m.stable(ic.spec)); setExtras({}); setOpenEdge(undefined); setMsg(undefined); setYamlErr(undefined)
    creds.current.clear(); setCredsVersion(v => v + 1)
    if (tab === 'code') { const y = toYaml(s); setYaml(y); setYamlBase(y) }
  }

  const close = () => {
    if (dirty && !window.confirm('Discard the unsaved claims mapping changes?')) return
    creds.current.clear()
    onClose()
  }

  // Escape closes (an open popover first); inputs that use Escape stop it
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      if (openEdge) { setOpenEdge(undefined); return }
      close()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })

  // focus moves into the window and back to whatever opened it
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    dialog.current?.focus()
    return () => prev?.focus()
  }, [])

  const tabs: [Tab, string][] = [['canvas', 'Canvas'], ['code', 'Code'], ['schedule', 'Schedule']]
  return createPortal(
    <div className="cm-overlay" onMouseDown={e => { if (e.target === e.currentTarget) close() }}>
      <div className="cm-window" role="dialog" aria-modal="true" aria-labelledby="cm-title" tabIndex={-1} ref={dialog}>
        <header className="cm-header">
          <h2 id="cm-title">Claims mapping</h2>
          <span className="mono subtle">{ns}/{ic.metadata.name}</span>
          <div className="tabs" role="tablist">
            {tabs.map(([t, label]) => (
              <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? 'tab active' : 'tab'} onClick={() => go(t)}
                title={t === 'code' ? 'Advanced: the mapping as YAML' : `${label} view`}>
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
        {msg && <div className={`note ${msg.ok ? 'ok' : 'bad'} cm-msg`}>{msg.text}</div>}
        <ClaimsContext.Provider value={actions}>
          <div className="cm-body">
            {tab === 'canvas' && (
              <ClaimsCanvas ic={ic} spec={spec} extras={extras} pending={pending} positions={positions.current} openEdge={openEdge}
                onMap={(tier, claim, attribute) => setSpec(s => m.setMapping(s, tier, claim, attribute))} />
            )}
            {tab === 'code' && (
              <div className="cm-code">
                <p className="subtle small">The profile, the sync schedule, and each OIDC tier's claims and directory. Tiers are matched by name; add or rename them in the rule builder. Leaving this tab applies it.</p>
                <div className="monaco">
                  <Editor height="100%" value={yaml} onChange={v => setYaml(v ?? '')} language="yaml" theme={monacoTheme()}
                    options={{ minimap: { enabled: false }, fontFamily: 'DM Mono', fontSize: 12, tabSize: 2, scrollBeyondLastLine: false, automaticLayout: true }} />
                </div>
                {yamlErr && <div className="note bad" role="alert">{yamlErr}</div>}
              </div>
            )}
            {tab === 'schedule' && <ScheduleTab ic={ic} spec={spec} setSpec={setSpec} dirty={dirty} />}
          </div>
        </ClaimsContext.Provider>
      </div>
    </div>,
    document.body,
  )
}
