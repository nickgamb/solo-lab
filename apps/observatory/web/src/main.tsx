import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'

// A lazily loaded piece (the editor) can fail to load when the page is older
// than the session: the edge's sign-in expired, or a deploy renamed the
// file. A reload re-runs the sign-in and fetches the current build; the flag
// keeps it to one try a minute.
window.addEventListener('vite:preloadError', e => {
  let last = 0
  try { last = Number(sessionStorage.getItem('obs.reloaded') ?? 0) } catch { /* private mode */ }
  if (Date.now() - last < 60_000) return
  e.preventDefault()
  try { sessionStorage.setItem('obs.reloaded', String(Date.now())) } catch { /* private mode */ }
  location.reload()
})

// Fetch the editor while the session is fresh, so Advanced opens instantly
// later and doesn't depend on the session then.
window.addEventListener('load', () => setTimeout(() => { import('./topology/ConfigEditor').catch(() => {}) }, 2500))

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
