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
import 'monaco-editor/languages/definitions/yaml/register'
import editorWorker from 'monaco-editor/editor/editor.worker?worker'

// Monaco from the bundle, not a CDN: the observatory works offline and
// under a strict CSP. Only the editor core, the editing features it uses,
// and the languages tokenized in the page: YAML (the resource editor, the
// directory sync's and assurance rules' Code tabs), and the two below.
self.MonacoEnvironment = { getWorker: () => new editorWorker() }
loader.config({ monaco })
monaco.editor.defineTheme('solo-dark', {
  base: 'vs-dark', inherit: true, rules: [{ token: 'type', foreground: 'b082fb' }, { token: 'string', foreground: 'e6d6ff' }],
  colors: { 'editor.background': '#12012a', 'editor.lineHighlightBackground': '#1f0c40', 'editorGutter.background': '#12012a' },
})

export const monacoTheme = () => (document.documentElement.dataset.theme === 'light' ? 'vs' : 'solo-dark')

// The routing rules' language: a rule header (name -> IdPs), its
// description as comments, and its CEL.
monaco.languages.register({ id: 'fabric-routing' })
monaco.languages.setMonarchTokensProvider('fabric-routing', {
  tokenizer: {
    root: [
      [/#.*$/, 'comment'],
      [/^(rule)(\s+)([a-z0-9-]+)(\s*)(->)/, ['keyword', '', 'type', '', 'keyword']],
      [/"([^"\\]|\\.)*"/, 'string'],
      [/'([^'\\]|\\.)*'/, 'string'],
      [/\b(true|false|null|in|has|matches|contains|startsWith|endsWith|default)\b/, 'keyword'],
      [/\b(request|source|jwt|mcp)\b/, 'variable'],
      [/[&|!=<>?:]+/, 'operator'],
      [/\d+/, 'number'],
    ],
  },
})

// Rego, read-only in the assurance rules' Code tab: the gate's decision
// logic. Monaco has no Rego of its own.
monaco.languages.register({ id: 'rego' })
monaco.languages.setMonarchTokensProvider('rego', {
  keywords: ['package', 'import', 'default', 'if', 'else', 'not', 'some', 'every', 'in', 'contains', 'with', 'as', 'true', 'false', 'null'],
  tokenizer: {
    root: [
      [/#.*$/, 'comment'],
      [/`[^`]*`/, 'string'],
      [/"([^"\\]|\\.)*"/, 'string'],
      [/[A-Za-z_][A-Za-z0-9_]*/, { cases: { '@keywords': 'keyword', '@default': 'identifier' } }],
      [/\d+(\.\d+)?([eE][-+]?\d+)?/, 'number'],
      [/:=|==|!=|<=|>=|[=<>|&+\-*/%]/, 'operator'],
    ],
  },
})
