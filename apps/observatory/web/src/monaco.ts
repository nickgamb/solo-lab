import { loader } from '@monaco-editor/react'
import * as monaco from 'monaco-editor'
import editorWorker from 'monaco-editor/editor/editor.worker?worker'

// Monaco from the bundle, not a CDN: the observatory works offline and
// under a strict CSP. YAML needs only the base editor worker.
self.MonacoEnvironment = { getWorker: () => new editorWorker() }
loader.config({ monaco })
monaco.editor.defineTheme('solo-dark', {
  base: 'vs-dark', inherit: true, rules: [{ token: 'type', foreground: 'b082fb' }, { token: 'string', foreground: 'e6d6ff' }],
  colors: { 'editor.background': '#12012a', 'editor.lineHighlightBackground': '#1f0c40', 'editorGutter.background': '#12012a' },
})

export const monacoTheme = () => (document.documentElement.dataset.theme === 'light' ? 'vs' : 'solo-dark')
