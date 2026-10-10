import { useEffect, useRef } from 'react'
import { Editor, type OnMount } from '@monaco-editor/react'
import { monacoTheme } from '../monaco'

export type Marker = { line: number; col: number; message: string }

// A Code tab's editor: the directory sync's JSON, the assurance rules' HCL,
// the routing rules' CEL.
// Monaco is large, so each window loads this the first time its Code tab
// opens. Markers are the document's errors, by position.
export default function CodeEditor({ value, onChange, language = 'json', markers, readOnly }: {
  value: string; onChange: (v: string) => void; language?: string; markers?: Marker[]; readOnly?: boolean
}) {
  const ref = useRef<Parameters<OnMount>>(undefined)
  useEffect(() => {
    const [ed, monaco] = ref.current ?? []
    const model = ed?.getModel()
    if (!model || !monaco) return
    monaco.editor.setModelMarkers(model, 'rules', (markers ?? []).map(m => ({
      startLineNumber: m.line, startColumn: m.col, endLineNumber: m.line, endColumn: model.getLineMaxColumn(Math.min(m.line, model.getLineCount())),
      message: m.message, severity: monaco.MarkerSeverity.Error,
    })))
  }, [markers, value])
  return (
    <Editor height="100%" value={value} onChange={v => onChange(v ?? '')} language={language} theme={monacoTheme()}
      onMount={(ed, monaco) => { ref.current = [ed, monaco] }}
      options={{ readOnly, minimap: { enabled: false }, fontFamily: 'DM Mono', fontSize: 12, tabSize: 2, scrollBeyondLastLine: false, automaticLayout: true }} />
  )
}
