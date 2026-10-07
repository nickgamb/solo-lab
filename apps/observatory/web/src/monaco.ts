import { loader } from '@monaco-editor/react'
import * as monaco from 'monaco-editor/editor/editor.api'
import 'monaco-editor/features/bracketMatching/register'
import 'monaco-editor/features/clipboard/register'
import 'monaco-editor/features/codeEditor/register'
import 'monaco-editor/features/codicon/register'
import 'monaco-editor/features/comment/register'
import 'monaco-editor/features/contextmenu/register'
import 'monaco-editor/features/diffEditor/register'
import 'monaco-editor/features/find/register'
import 'monaco-editor/features/folding/register'
import 'monaco-editor/features/gotoError/register'
import 'monaco-editor/features/hover/register'
import 'monaco-editor/features/indentation/register'
import 'monaco-editor/features/linesOperations/register'
import 'monaco-editor/features/multicursor/register'
import 'monaco-editor/features/suggest/register'
import 'monaco-editor/features/wordHighlighter/register'
import 'monaco-editor/features/wordOperations/register'
import 'monaco-editor/languages/features/json/register'
import 'monaco-editor/languages/definitions/yaml/register'
import 'monaco-editor/languages/definitions/hcl/register'
import editorWorker from 'monaco-editor/editor/editor.worker?worker'
import jsonWorker from 'monaco-editor/language/json/json.worker?worker'

// Monaco from the bundle, not a CDN: the observatory works offline and
// under a strict CSP. Only the editor core, the editing features it uses,
// and three languages: YAML (the resource editor) and HCL (the assurance
// rules' Code tab), tokenized in the page, and JSON (the directory sync's
// Code tab, checked in its own worker).
self.MonacoEnvironment = { getWorker: (_, label) => (label === 'json' ? new jsonWorker() : new editorWorker()) }
loader.config({ monaco })
monaco.editor.defineTheme('solo-dark', {
  base: 'vs-dark', inherit: true, rules: [{ token: 'type', foreground: 'b082fb' }, { token: 'string', foreground: 'e6d6ff' }],
  colors: { 'editor.background': '#12012a', 'editor.lineHighlightBackground': '#1f0c40', 'editorGutter.background': '#12012a' },
})

export const monacoTheme = () => (document.documentElement.dataset.theme === 'light' ? 'vs' : 'solo-dark')
