import { useCallback, useEffect, useState } from 'react'
import { api, useLab, type Me } from './api'
import wordmark from './assets/solo-wordmark.svg?raw'
import { Topology } from './topology/Topology'
import { TrafficTab } from './traffic/TrafficTab'
import { ContinuityTab } from './continuity/ContinuityTab'
import { ModelsTab } from './models/ModelsTab'
import './app.css'

type Tab = 'topology' | 'traffic' | 'continuity' | 'models'
const TABS: { id: Tab; label: string }[] = [
  { id: 'topology', label: 'Topology' },
  { id: 'traffic', label: 'Traffic' },
  { id: 'continuity', label: 'Identity Continuity' },
  { id: 'models', label: 'Model Continuity' },
]

const initialTab = (): Tab => {
  const t = location.hash.slice(1) as Tab
  return TABS.some(x => x.id === t) ? t : 'topology'
}

export default function App() {
  const lab = useLab()
  const [tab, setTab] = useState<Tab>(initialTab)
  const [me, setMe] = useState<Me>()
  const [theme, setTheme] = useState(() => {
    try { return localStorage.getItem('obs.theme') ?? 'dark' } catch { return 'dark' }
  })
  const [focus, setFocus] = useState<string>() // node id opened from another tab

  useEffect(() => { api<Me>('/api/me').then(setMe).catch(() => {}) }, [])
  useEffect(() => { location.hash = tab }, [tab])
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    try { localStorage.setItem('obs.theme', theme) } catch { /* private mode */ }
  }, [theme])

  const s = lab.stats
  // stable, so the traffic rows it's passed to stay memoized
  const open = useCallback((id: string) => { setFocus(id); setTab('topology') }, [])

  return (
    <div className="app">
      <header className="top">
        <div className="brand">
          <span className="wordmark" dangerouslySetInnerHTML={{ __html: wordmark }} />
          <span className="product">Observatory</span>
        </div>
        <nav className="tabs">
          {TABS.map(t => (
            <button key={t.id} className={t.id === tab ? 'tab active' : 'tab'} onClick={() => setTab(t.id)}>{t.label}</button>
          ))}
        </nav>
        <div className="status">
          <span className="pill stat" title="requests per second, all gateways and waypoints">{(s?.rps ?? 0).toFixed(1)} req/s</span>
          <span className="pill stat" title="share of requests denied or failed">{((s?.errRate ?? 0) * 100).toFixed(1)}% err</span>
          <span className="pill"><span className={lab.connected ? 'dot ok' : 'dot bad'} />{lab.connected ? 'Live' : 'Reconnecting…'}</span>
          <button className="btn ghost small" title="Theme" onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>{theme === 'dark' ? '☾' : '☀'}</button>
          {me && (
            <span className="user" title={me.groups.join(', ')}>
              <span className="avatar">{me.name.slice(0, 1).toUpperCase()}</span>
              <span className="muted">{me.name}</span>
              <a className="subtle" href="/logout">Sign out</a>
            </span>
          )}
        </div>
      </header>
      <main className="body">
        {tab === 'topology' && <Topology lab={lab} focus={focus} onFocused={() => setFocus(undefined)} />}
        {tab === 'traffic' && <TrafficTab lab={lab} onOpenNode={open} />}
        {tab === 'continuity' && <ContinuityTab lab={lab} />}
        {tab === 'models' && <ModelsTab lab={lab} />}
      </main>
    </div>
  )
}
