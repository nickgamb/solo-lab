import { useState } from 'react'
import { useSync, type TestResult } from './syncContext'

// TestConnection runs the saved directory's check where the sync runs: a
// token from the IdP, then a read of its users (a count, never records) and
// of its attribute schema, which fills the IdP's node.
export function TestConnection({ idp }: { idp: string }) {
  const c = useSync()
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<TestResult>()
  const saved = c.directorySaved(idp)
  const run = async () => { setBusy(true); setRes(undefined); setRes(await c.testDirectory(idp)); setBusy(false) }
  return (
    <div className="cm-test">
      <button className="btn small nodrag" disabled={!saved || busy} onClick={run}
        title={saved ? 'Get a token, count the users and read the attribute schema, as the sync does (its account, credentials and network path)' : 'Save the directory and its credentials first: the test runs the saved settings where the sync runs'}>
        {busy ? 'Testing…' : 'Test connection'}
      </button>
      {!saved && !res && <span className="small subtle">save first</span>}
      {res && <span className={`small ${res.ok ? 'ok-text' : 'danger-text'}`} role="status">{res.text}</span>}
    </div>
  )
}
