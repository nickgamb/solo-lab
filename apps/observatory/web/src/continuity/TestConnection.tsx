import { useState } from 'react'
import { useClaims } from './claimsContext'

// TestConnection runs the saved directory's check where the sync runs: a
// token from the tier, then a read of its users (a count, never records).
export function TestConnection({ tier }: { tier: string }) {
  const c = useClaims()
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<{ ok: boolean; text: string }>()
  const saved = c.directorySaved(tier)
  const run = async () => { setBusy(true); setRes(undefined); setRes(await c.testDirectory(tier)); setBusy(false) }
  return (
    <div className="cm-test">
      <button className="btn small nodrag" disabled={!saved || busy} onClick={run}
        title={saved ? 'Get a token and read the users, as the sync does (its account, credentials and network path)' : 'Save the directory and its credentials first: the test runs the saved settings where the sync runs'}>
        {busy ? 'Testing…' : 'Test connection'}
      </button>
      {!saved && !res && <span className="small subtle">save first</span>}
      {res && <span className={`small ${res.ok ? 'ok-text' : 'danger-text'}`} role="status">{res.text}</span>}
    </div>
  )
}
