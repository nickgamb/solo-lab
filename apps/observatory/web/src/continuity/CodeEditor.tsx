import { Editor } from '@monaco-editor/react'
import { monacoTheme } from '../monaco'

// The Code tab's JSON editor. Monaco is large, so the directory sync window
// loads this the first time the tab opens.
export default function CodeEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <Editor height="100%" value={value} onChange={v => onChange(v ?? '')} language="json" theme={monacoTheme()}
      options={{ minimap: { enabled: false }, fontFamily: 'DM Mono', fontSize: 12, tabSize: 2, scrollBeyondLastLine: false, automaticLayout: true }} />
  )
}
