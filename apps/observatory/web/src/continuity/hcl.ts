// A small subset of HCL (HashiCorp configuration language), for documents
// people edit by hand: blocks with labels, attributes, comments, and the
// values strings, numbers, true/false/null, lists and objects.
//
//   # a comment
//   rule "ledger" {
//     minimum = "AAL2"
//     clients = ["web", "cli"]
//     acr     = { "urn:gold" = "AAL2" }
//   }
//
// What it reads, it prints back the same way (attributes aligned on =, as
// terraform fmt does). Comments aren't kept: the document is printed from
// what it says.

export type Value = string | number | boolean | null | Value[] | HclObject
export type HclObject = { kind: 'object'; entries: Entry[] }
export type Entry = { key: string; value: Value; line: number; col: number }
export type Attr = { kind: 'attr'; key: string; value: Value; line: number; col: number }
export type Block = { kind: 'block'; type: string; labels: string[]; body: Item[]; line: number; col: number }
export type Item = Attr | Block

export class HclError extends Error {
  line: number
  col: number
  constructor(message: string, line: number, col: number) {
    super(message)
    this.line = line
    this.col = col
  }
}

type Tok = { t: 'ident' | 'string' | 'number' | 'punct' | 'nl' | 'eof'; v: string; line: number; col: number }

function lex(src: string): Tok[] {
  const out: Tok[] = []
  let i = 0, line = 1, col = 1
  const adv = (n = 1) => {
    for (let k = 0; k < n; k++) {
      if (src[i] === '\n') { line++; col = 1 } else col++
      i++
    }
  }
  while (i < src.length) {
    const c = src[i]
    if (c === '\n') { out.push({ t: 'nl', v: '\n', line, col }); adv(); continue }
    if (c === ' ' || c === '\t' || c === '\r') { adv(); continue }
    if (c === '#' || (c === '/' && src[i + 1] === '/')) { while (i < src.length && src[i] !== '\n') adv(); continue }
    if (c === '/' && src[i + 1] === '*') {
      const l = line, cl = col
      adv(2)
      while (i < src.length && !(src[i] === '*' && src[i + 1] === '/')) adv()
      if (i >= src.length) throw new HclError('a /* comment */ is never closed', l, cl)
      adv(2)
      continue
    }
    if ('{}[]=,:'.includes(c)) { out.push({ t: 'punct', v: c, line, col }); adv(); continue }
    if (c === '"') {
      const l = line, cl = col
      adv()
      let s = ''
      for (;;) {
        if (i >= src.length || src[i] === '\n') throw new HclError('a string is never closed: end it with "', l, cl)
        const d = src[i]
        if (d === '"') { adv(); break }
        if (d === '\\') {
          const e = src[i + 1]
          const map: Record<string, string> = { n: '\n', t: '\t', r: '\r', '"': '"', '\\': '\\' }
          if (e in map) { s += map[e]; adv(2); continue }
          if (e === 'u' && /^[0-9a-fA-F]{4}$/.test(src.slice(i + 2, i + 6))) { s += String.fromCharCode(parseInt(src.slice(i + 2, i + 6), 16)); adv(6); continue }
          throw new HclError(`unknown escape \\${e ?? ''} in a string`, line, col)
        }
        s += d
        adv()
      }
      out.push({ t: 'string', v: s, line: l, col: cl })
      continue
    }
    if (/[0-9-]/.test(c)) {
      const m = /^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?/.exec(src.slice(i))
      if (m) { out.push({ t: 'number', v: m[0], line, col }); adv(m[0].length); continue }
    }
    if (/[A-Za-z_]/.test(c)) {
      const m = /^[A-Za-z_][A-Za-z0-9_-]*/.exec(src.slice(i))!
      out.push({ t: 'ident', v: m[0], line, col })
      adv(m[0].length)
      continue
    }
    throw new HclError(`unexpected ${JSON.stringify(c)}`, line, col)
  }
  out.push({ t: 'eof', v: '', line, col })
  return out
}

const what = (t: Tok) => (t.t === 'eof' ? 'the end' : t.t === 'nl' ? 'a new line' : t.t === 'string' ? `"${t.v}"` : t.v)

export function parse(src: string): Item[] {
  const toks = lex(src)
  let p = 0
  const peek = () => toks[p]
  const next = () => toks[p++]
  const skipNl = () => { while (peek().t === 'nl') p++ }
  const expect = (v: string) => {
    const t = next()
    if (t.t !== 'punct' || t.v !== v) throw new HclError(`expected ${v}, found ${what(t)}`, t.line, t.col)
    return t
  }

  function value(): Value {
    skipNl()
    const t = next()
    switch (t.t) {
      case 'string': return t.v
      case 'number': return Number(t.v)
      case 'ident':
        if (t.v === 'true') return true
        if (t.v === 'false') return false
        if (t.v === 'null') return null
        throw new HclError(`${t.v} isn't a value: put text in quotes ("${t.v}")`, t.line, t.col)
      case 'punct':
        if (t.v === '[') {
          const out: Value[] = []
          for (;;) {
            skipNl()
            if (peek().t === 'punct' && peek().v === ']') { next(); return out }
            out.push(value())
            skipNl()
            const s = next()
            if (s.t === 'punct' && s.v === ']') return out
            if (s.t !== 'punct' || s.v !== ',') throw new HclError(`expected , or ] in a list, found ${what(s)}`, s.line, s.col)
          }
        }
        if (t.v === '{') {
          const obj: HclObject = { kind: 'object', entries: [] }
          for (;;) {
            skipNl()
            const k = next()
            if (k.t === 'punct' && k.v === '}') return obj
            if (k.t !== 'ident' && k.t !== 'string') throw new HclError(`expected a key or }, found ${what(k)}`, k.line, k.col)
            const eq = next()
            if (eq.t !== 'punct' || (eq.v !== '=' && eq.v !== ':')) throw new HclError(`expected = after ${k.v}, found ${what(eq)}`, eq.line, eq.col)
            if (obj.entries.some(e => e.key === k.v)) throw new HclError(`${k.v} is set twice`, k.line, k.col)
            obj.entries.push({ key: k.v, value: value(), line: k.line, col: k.col })
            const s = peek()
            if (s.t === 'punct' && s.v === ',') next()
            else if (s.t !== 'nl' && !(s.t === 'punct' && s.v === '}')) throw new HclError(`expected a new line, , or } after ${k.v}, found ${what(s)}`, s.line, s.col)
          }
        }
    }
    throw new HclError(`expected a value, found ${what(t)}`, t.line, t.col)
  }

  function body(top: boolean): Item[] {
    const items: Item[] = []
    for (;;) {
      skipNl()
      const t = peek()
      if (t.t === 'eof') {
        if (!top) throw new HclError('a block is never closed: end it with }', t.line, t.col)
        return items
      }
      if (t.t === 'punct' && t.v === '}') {
        if (top) throw new HclError('} without a block to close', t.line, t.col)
        next()
        return items
      }
      if (t.t !== 'ident') throw new HclError(`expected a name, found ${what(t)}`, t.line, t.col)
      next()
      const n = peek()
      if (n.t === 'punct' && n.v === '=') {
        next()
        if (items.some(i => i.kind === 'attr' && i.key === t.v)) throw new HclError(`${t.v} is set twice`, t.line, t.col)
        items.push({ kind: 'attr', key: t.v, value: value(), line: t.line, col: t.col })
        const e = peek()
        if (e.t !== 'nl' && e.t !== 'eof' && !(e.t === 'punct' && e.v === '}')) throw new HclError(`expected a new line after ${t.v}, found ${what(e)}`, e.line, e.col)
        continue
      }
      const labels: string[] = []
      while (peek().t === 'string' || peek().t === 'ident') labels.push(next().v)
      expect('{')
      items.push({ kind: 'block', type: t.v, labels, body: body(false), line: t.line, col: t.col })
    }
  }
  return body(true)
}

// printing

const IDENT = /^[A-Za-z_][A-Za-z0-9_-]*$/

export function str(s: string): string {
  return JSON.stringify(s)
}

function key(k: string): string {
  return IDENT.test(k) && !['true', 'false', 'null'].includes(k) ? k : str(k)
}

function inline(v: Value): string | undefined {
  if (v === null) return 'null'
  if (typeof v === 'string') return str(v)
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (Array.isArray(v)) {
    const parts = v.map(inline)
    if (parts.some(p => p === undefined)) return undefined
    const s = `[${parts.join(', ')}]`
    return s.length <= 72 ? s : undefined
  }
  if (!v.entries.length) return '{}'
  const parts = v.entries.map(e => { const x = inline(e.value); return x === undefined ? undefined : `${key(e.key)} = ${x}` })
  if (parts.some(p => p === undefined)) return undefined
  const s = `{ ${parts.join(', ')} }`
  return s.length <= 60 ? s : undefined
}

function printValue(v: Value, ind: string, force = false): string {
  const one = force ? undefined : inline(v)
  if (one !== undefined) return one
  if (Array.isArray(v)) return `[\n${v.map(x => `${ind}  ${printValue(x, ind + '  ')},`).join('\n')}\n${ind}]`
  const o = v as HclObject
  return `{\n${aligned(o.entries.map(e => [key(e.key), e.value] as [string, Value]), ind + '  ')}\n${ind}}`
}

function aligned(rows: [string, Value][], ind: string): string {
  const w = Math.max(0, ...rows.map(([k]) => k.length))
  return rows.map(([k, v]) => `${ind}${k.padEnd(w)} = ${printValue(v, ind, typeof v === 'object' && v !== null && !Array.isArray(v) && v.entries.length > 1)}`).join('\n')
}

// A document to print: attributes and blocks, with comments where wanted.
export type Doc = (
  | { kind: 'attr'; key: string; value: Value }
  | { kind: 'block'; type: string; labels: string[]; body: Doc; comment?: string }
  | { kind: 'comment'; text: string }
  | { kind: 'blank' }
)[]

export function print(doc: Doc, ind = ''): string {
  const out: string[] = []
  let run: [string, Value][] = []
  const flush = () => { if (run.length) out.push(aligned(run, ind)); run = [] }
  for (const d of doc) {
    if (d.kind === 'attr') { run.push([key(d.key), d.value]); continue }
    flush()
    if (d.kind === 'blank') out.push('')
    else if (d.kind === 'comment') out.push(...d.text.split('\n').map(l => `${ind}# ${l}`.trimEnd()))
    else {
      const head = `${ind}${d.type}${d.labels.map(l => ` ${str(l)}`).join('')} {${d.comment ? ` # ${d.comment}` : ''}`
      out.push(d.body.length ? `${head}\n${print(d.body, ind + '  ')}\n${ind}}` : d.comment ? `${head}\n${ind}}` : `${head}}`)
    }
  }
  flush()
  return out.join('\n')
}

export const obj = (entries: [string, Value][]): HclObject => ({ kind: 'object', entries: entries.map(([key, value]) => ({ key, value, line: 0, col: 0 })) })
