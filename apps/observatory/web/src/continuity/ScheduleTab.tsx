import { useEffect, useState } from 'react'
import { api, type ContinuitySpec, type IdentityContinuity } from '../api'
import { cronError, DAYS, describeCron, detectPreset, hhmm, nextRuns, presetCron, type Preset } from './cron'
import { withSync } from './mapping'

type Kind = Preset['kind']
const DEFAULT = '0 2 * * *'
const fmtRun = (d: Date) => `${DAYS[d.getUTCDay()].slice(0, 3)} ${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`
const when = (s?: string) => (s ? new Date(s).toLocaleString() : 'never')
const parseTime = (v: string) => { const [h, m] = v.split(':').map(Number); return { h: h || 0, m: m || 0 } }

// ScheduleTab: when the directory sync runs, and what it last did.
export function ScheduleTab({ ic, spec, setSpec, dirty }: {
  ic: IdentityContinuity; spec: ContinuitySpec; setSpec: (f: (s: ContinuitySpec) => ContinuitySpec) => void; dirty: boolean
}) {
  const sync = spec.sync
  const on = !!sync && !sync.suspend
  const schedule = sync?.schedule ?? DEFAULT
  const p = detectPreset(schedule)
  // the preset follows the schedule (a reset, the live object), except
  // that Custom, once chosen, stays until another preset is
  const [custom, setCustom] = useState(false)
  const kind: Kind = custom ? 'custom' : p.kind
  const [run, setRun] = useState<{ ok: boolean; text: string }>()
  const [running, setRunning] = useState(false)
  const [now, setNow] = useState(() => new Date())
  useEffect(() => { const t = setInterval(() => setNow(new Date()), 60_000); return () => clearInterval(t) }, [])
  const err = cronError(schedule)
  const st = ic.status?.sync

  const setSchedule = (s: string) => setSpec(sp => withSync(sp, { ...(sp.sync ?? {}), schedule: s }))
  const toggle = (v: boolean) => setSpec(sp => {
    if (!v) return sp.sync ? withSync(sp, { ...sp.sync, suspend: true }) : sp
    if (!sp.sync) return withSync(sp, { schedule: DEFAULT })
    const next = { ...sp.sync }
    delete next.suspend
    return withSync(sp, next)
  })
  // the controls show the current schedule when it fits the preset, else a sensible default
  const daily = p.kind === 'daily' || p.kind === 'weekly' ? { h: p.h, m: p.m } : { h: 2, m: 0 }
  const hours = p.kind === 'hours' ? p.n : 6
  const weekly = { d: p.kind === 'weekly' ? p.d : 1, ...daily }
  const pick = (k: Kind) => {
    setCustom(k === 'custom')
    if (k === 'daily') setSchedule(presetCron({ kind: 'daily', ...daily }))
    if (k === 'hours') setSchedule(presetCron({ kind: 'hours', n: hours }))
    if (k === 'weekly') setSchedule(presetCron({ kind: 'weekly', ...weekly }))
  }

  const runNow = async () => {
    setRunning(true); setRun(undefined)
    try {
      // no body, but the server's write guard wants a JSON content type
      const r = await api<{ job?: string }>(`/api/continuity/${ic.metadata.namespace}/${ic.metadata.name}/sync`,
        { method: 'POST', headers: { 'Content-Type': 'application/json' } })
      setRun({ ok: true, text: r?.job ? `Started job ${r.job}.` : 'Started.' })
    } catch (e) { setRun({ ok: false, text: (e as Error).message }) } finally { setRunning(false) }
  }

  return (
    <div className="cm-sched scroll">
      <section className="cm-card">
        <div className="row">
          <h3 className="grow">Directory sync</h3>
          <label className="tog" title="Off suspends the CronJob and keeps the schedule">
            <input type="checkbox" checked={on} onChange={e => toggle(e.target.checked)} />Scheduled sync
          </label>
        </div>
        <p className="subtle small">Reads each employee's profile from the primary IdP into S&amp;V's profile, then writes it to each failover IdP, creating the user there if the primary has them (they're emailed to set their own password). Never passwords, never the username.</p>
        {!sync ? <p className="subtle small">No scheduled sync yet. Turn it on to run daily at 02:00 UTC.</p> : (
          <div className={on ? 'cm-presets' : 'cm-presets off'}>
            <div className="seg" role="radiogroup" aria-label="Schedule">
              {([['daily', 'Daily'], ['hours', 'Every N hours'], ['weekly', 'Weekly'], ['custom', 'Custom (cron)']] as const).map(([k, label]) => (
                <button key={k} role="radio" aria-checked={kind === k} className={kind === k ? 'on' : ''} onClick={() => pick(k)} title={`${label} schedule`}>{label}</button>
              ))}
            </div>
            {kind === 'daily' && (
              <label className="row small">Daily at
                <input className="field cm-time" type="time" value={hhmm(daily.h, daily.m)} onChange={e => setSchedule(presetCron({ kind: 'daily', ...parseTime(e.target.value) }))} />(UTC)
              </label>
            )}
            {kind === 'hours' && (
              <label className="row small">Every
                <select className="field cm-num" value={hours} onChange={e => setSchedule(presetCron({ kind: 'hours', n: Number(e.target.value) }))}>
                  {Array.from({ length: 12 }, (_, i) => i + 1).map(n => <option key={n} value={n}>{n}</option>)}
                </select>hours, on the hour (UTC)
              </label>
            )}
            {kind === 'weekly' && (
              <label className="row small">Weekly on
                <select className="field cm-day" value={weekly.d} onChange={e => setSchedule(presetCron({ kind: 'weekly', ...weekly, d: Number(e.target.value) }))}>
                  {DAYS.map((d, i) => <option key={d} value={i}>{d}</option>)}
                </select>at
                <input className="field cm-time" type="time" value={hhmm(weekly.h, weekly.m)}
                  onChange={e => setSchedule(presetCron({ kind: 'weekly', d: weekly.d, ...parseTime(e.target.value) }))} />(UTC)
              </label>
            )}
            {kind === 'custom' && (
              <label className="small cm-stack">Cron (minute hour day-of-month month day-of-week, UTC)
                <input className={`field mono${err ? ' invalid' : ''}`} value={schedule} aria-invalid={!!err} onChange={e => setSchedule(e.target.value)} spellCheck={false} />
              </label>
            )}
            {err ? <div className="note bad">{err}</div> : (
              <div className="cm-cron">
                <span className="mono">{schedule}</span>
                <span>{describeCron(schedule)}</span>
                <span className="subtle small">next: {nextRuns(schedule, now, 3).map(fmtRun).join(' · ') || 'never'}</span>
              </div>
            )}
          </div>
        )}
      </section>

      <section className="cm-card">
        <div className="row">
          <h3 className="grow">Last run</h3>
          <button className="btn small" disabled={dirty || running} onClick={runNow}
            title={dirty ? 'Save first: a run uses the saved mapping' : 'Start a sync job now'}>Run now</button>
        </div>
        {run && <div className={`note ${run.ok ? 'ok' : 'bad'}`}>{run.text}</div>}
        {st ? (
          <dl className="kv">
            <dt>CronJob</dt><dd className="mono">{st.cronJob ?? 'not created yet'}</dd>
            <dt>Last run</dt><dd>{when(st.lastRun)}</dd>
            <dt>Last success</dt><dd>{when(st.lastSuccess)}</dd>
            <dt>Users</dt><dd>{st.users ?? 0} · {st.updated ?? 0} S&amp;V profiles updated · {st.written ?? 0} failover accounts written · {st.created ?? 0} created · <span className={st.failed ? 'danger-text' : ''}>{st.failed ?? 0} failed</span></dd>
            {st.message && <><dt>Message</dt><dd>{st.message}</dd></>}
          </dl>
        ) : <p className="subtle small">The sync hasn't run yet.</p>}
      </section>

    </div>
  )
}
