// A small 5-field cron reader: enough to describe a CronJob schedule and say
// when it fires next. All times are UTC, as the CronJob's are by default.

type Cron = {
  minute: Set<number>; hour: Set<number>; dom: Set<number>; month: Set<number>; dow: Set<number>
  domStar: boolean; dowStar: boolean; fields: string[]
}

const MACROS: Record<string, string> = {
  '@yearly': '0 0 1 1 *', '@annually': '0 0 1 1 *', '@monthly': '0 0 1 * *', '@weekly': '0 0 * * 0',
  '@daily': '0 0 * * *', '@midnight': '0 0 * * *', '@hourly': '0 * * * *',
}
const BOUNDS: [number, number][] = [[0, 59], [0, 23], [1, 31], [1, 12], [0, 7]]
const FIELD = ['minute', 'hour', 'day of month', 'month', 'day of week']
export const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

function field(s: string, i: number): Set<number> {
  const [lo, hi] = BOUNDS[i]
  const out = new Set<number>()
  for (const part of s.split(',')) {
    const m = /^(\*|\d+(?:-\d+)?)(?:\/(\d+))?$/.exec(part)
    if (!m) throw new Error(`${FIELD[i]}: "${part}" is not a number, range, list or step`)
    const step = m[2] === undefined ? 1 : Number(m[2])
    if (step < 1) throw new Error(`${FIELD[i]}: step must be at least 1`)
    let a = lo, b = hi
    if (m[1] !== '*') {
      const [x, y] = m[1].split('-').map(Number)
      // "5/15" means from 5 to the end, every 15
      a = x; b = y ?? (m[2] !== undefined ? hi : x)
    }
    if (a < lo || b > hi || a > b) throw new Error(`${FIELD[i]}: "${part}" is outside ${lo}-${hi}`)
    for (let v = a; v <= b; v += step) out.add(i === 4 && v === 7 ? 0 : v)
  }
  return out
}

function parse(expr: string): Cron {
  const s = MACROS[expr.trim()] ?? expr.trim()
  const f = s.split(/\s+/)
  if (f.length !== 5 || !f[0]) throw new Error('expected 5 fields: minute hour day-of-month month day-of-week')
  const [minute, hour, dom, month, dow] = f.map((x, i) => field(x, i))
  return { minute, hour, dom, month, dow, domStar: f[2].startsWith('*'), dowStar: f[4].startsWith('*'), fields: f }
}

export function cronError(expr: string): string | undefined {
  try { parse(expr); return undefined } catch (e) { return (e as Error).message }
}

// Standard cron: when both day fields are restricted, either one matching is enough.
function dayMatches(c: Cron, t: Date) {
  const dom = c.dom.has(t.getUTCDate()), dow = c.dow.has(t.getUTCDay())
  return c.domStar || c.dowStar ? dom && dow : dom || dow
}

export function nextRuns(expr: string, from: Date, n: number): Date[] {
  let c: Cron
  try { c = parse(expr) } catch { return [] }
  const t = new Date(from.getTime())
  t.setUTCSeconds(0, 0)
  t.setUTCMinutes(t.getUTCMinutes() + 1)
  const out: Date[] = []
  // skip whole months, days and hours that can't match; the guard ends
  // schedules that never fire (the 30th of February)
  for (let guard = 0; out.length < n && guard < 200_000; guard++) {
    if (!c.month.has(t.getUTCMonth() + 1)) { t.setUTCMonth(t.getUTCMonth() + 1, 1); t.setUTCHours(0, 0, 0, 0); continue }
    if (!dayMatches(c, t)) { t.setUTCDate(t.getUTCDate() + 1); t.setUTCHours(0, 0, 0, 0); continue }
    if (!c.hour.has(t.getUTCHours())) { t.setUTCHours(t.getUTCHours() + 1, 0, 0, 0); continue }
    if (!c.minute.has(t.getUTCMinutes())) { t.setUTCMinutes(t.getUTCMinutes() + 1, 0, 0); continue }
    out.push(new Date(t))
    t.setUTCMinutes(t.getUTCMinutes() + 1)
  }
  return out
}

const p2 = (n: number) => String(n).padStart(2, '0')
const sorted = (s: Set<number>) => [...s].sort((a, b) => a - b)
const list = (s: Set<number>, name: (n: number) => string = String) => {
  const v = sorted(s)
  return v.length > 6 ? `${v.slice(0, 5).map(name).join(', ')} and ${v.length - 5} more` : v.map(name).join(', ')
}
const NUM = /^\d+$/
const STEP = /^\*\/(\d+)$/

export function describeCron(expr: string): string {
  let c: Cron
  try { c = parse(expr) } catch { return '' }
  const [mi, ho] = c.fields
  let time: string
  if (NUM.test(mi) && NUM.test(ho)) time = `at ${p2(+ho)}:${p2(+mi)} UTC`
  else if (NUM.test(mi) && ho === '*') time = mi === '0' ? 'every hour, on the hour' : `every hour at minute ${mi}`
  else if (NUM.test(mi) && STEP.test(ho)) time = `every ${STEP.exec(ho)![1]} hours${mi === '0' ? ', on the hour' : ` at minute ${mi}`} (UTC)`
  else if (mi === '*' && ho === '*') time = 'every minute'
  else if (STEP.test(mi) && ho === '*') time = `every ${STEP.exec(mi)![1]} minutes`
  else time = `at minute ${list(c.minute)} past hour ${list(c.hour)} (UTC)`
  const days: string[] = []
  if (!c.domStar) days.push(`on day ${list(c.dom)} of the month`)
  if (!c.dowStar) days.push(`on ${list(c.dow, d => DAYS[d])}`)
  let day = days.join(' or ')
  if (!day && NUM.test(ho)) day = 'every day'
  if (!c.fields[3].startsWith('*') || c.month.size < 12) day += `${day ? ' ' : ''}in ${list(c.month, m => MONTHS[m - 1])}`
  const s = `${time}${day ? `, ${day}` : ''}`
  return s[0].toUpperCase() + s.slice(1)
}

export type Preset =
  | { kind: 'daily'; h: number; m: number }
  | { kind: 'hours'; n: number }
  | { kind: 'weekly'; d: number; h: number; m: number }
  | { kind: 'custom' }

export function detectPreset(expr: string): Preset {
  const f = expr.trim().split(/\s+/)
  if (f.length !== 5 || cronError(expr)) return { kind: 'custom' }
  const [mi, ho, dom, mo, dow] = f
  if (dom !== '*' || mo !== '*') return { kind: 'custom' }
  if (NUM.test(mi) && NUM.test(ho) && dow === '*') return { kind: 'daily', h: +ho, m: +mi }
  if (NUM.test(mi) && NUM.test(ho) && NUM.test(dow)) return { kind: 'weekly', d: +dow % 7, h: +ho, m: +mi }
  if (mi === '0' && dow === '*') {
    const n = ho === '*' ? 1 : STEP.test(ho) ? Number(STEP.exec(ho)![1]) : 0
    if (n >= 1 && n <= 12) return { kind: 'hours', n }
  }
  return { kind: 'custom' }
}

export function presetCron(p: Exclude<Preset, { kind: 'custom' }>): string {
  switch (p.kind) {
    case 'daily': return `${p.m} ${p.h} * * *`
    case 'hours': return p.n === 1 ? '0 * * * *' : `0 */${p.n} * * *`
    case 'weekly': return `${p.m} ${p.h} * * ${p.d}`
  }
}

export const hhmm = (h: number, m: number) => `${p2(h)}:${p2(m)}`
