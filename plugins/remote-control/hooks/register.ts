import type { EngineInterface, Register } from 'claude-code'

import type {
  AgentctlRcAck,
  AgentctlRcAction,
  AgentctlRcEntry,
  AgentctlRcProvenance,
  AgentctlRcRejectReason,
  AgentctlRcResponse,
  AgentctlRcResult,
} from '../types'

// Claude Code 2.1.287 is the first release with the hooks this mod stands on.
const FLOOR = [2, 1, 287] as const
const POLL_MS = 500
// How long an issued `remote-control` is watched for a new bridge id.
const OBSERVE_MS = 30_000
// The lifetime of a reconnect asked for with `/agentctl-rc reconnect`, the
// same as agentctl gives its own reconnect requests.
const COMMAND_LIFETIME_MS = 75_000
// The longest lifetime a request may ask for, ten times agentctl's longest.
// A rejection is remembered this long, so a request file still lying there
// afterwards is refused again, as expired when its body is readable, and is
// never admitted.
const MAX_LIFETIME_MS = 600_000
// Bounds on the table: entries not yet answered, and entries of any phase.
const MAX_PENDING = 64
const MAX_ENTRIES = 1024
// agentctl reads registry records with the same bound.
const MAX_RECORD_BYTES = 64 * 1024
const MAX_REQUEST_BYTES = 64 * 1024
const REQUEST_NAME = /^([0-9a-f]{32})\.request\.json$/
const ID_COMMAND = '/usr/bin/id'
const STAT_COMMAND = '/usr/bin/stat'
// The darwin `stat -f '%u %Lp %HT'` answer: owner uid, permission bits in
// octal, file type in words. A non-darwin `stat` cannot print this shape.
const STAT_SHAPE = /^(\d+) ([0-7]{1,4}) ([A-Za-z ]+)$/

type Timer = ReturnType<EngineInterface['clock']['every']>
type Table = Record<string, AgentctlRcEntry>

/** What the mod established about its own process at session start. */
type Context = {
  pid: number
  configHome: string
  recordPath: string
  transportDir: string
  uid: number
  versionBase: string
}

/**
 * The registry record as the mod uses it. The bridge id itself never leaves
 * `readRecord`: only a digest, compared to tell one bridge from the next.
 */
type Record_ = {
  pid: number | undefined
  sessionId: string | undefined
  status: string | undefined
  messagingSocketPath: string | undefined
  bridgeMark: string | undefined
}

type Bridge = { present: boolean; generation: number }

/** What one poll reads once and every entry of that poll shares. */
type Inputs = {
  record: Record_ | undefined
  bridge: Bridge
  isListed: () => Promise<boolean>
  isAuthorized: () => Promise<boolean>
  surfaces: () => Promise<string[]>
  provenance: () => Promise<AgentctlRcProvenance>
}

// Module variables are rebuilt by every session.start, which is also what a
// hot reload re-fires; everything a request needs to survive a reload lives
// in $.state instead.
let ticker: Timer | undefined
let context: Context | undefined
let refusal: string | undefined
let turns = new Set<string>()
let isPolling = false
let bridgeMark: string | undefined
let sawAbsent = false
let queue: Promise<unknown> = Promise.resolve()
// Ids already answered `busy`. They are not stored in the table, which may
// be full, so this set is what keeps that answer to one write per id.
let answeredBusy = new Set<string>()

/** Replaces Remote Control URLs and bridge ids in text bound for a log or an answer. */
export function redact(text: string): string {
  return text
    .replace(/https?:\/\/\S+/g, '[URL redacted]')
    .replace(/\b(?:session|cse|bridge)_[A-Za-z0-9_-]+/g, '[bridge id redacted]')
}

/** Reports whether a release base such as `2.1.296` or `2.1.296-dev` is at or above the floor. */
export function meetsFloor(base: string | undefined): boolean {
  const match = /^(\d+)\.(\d+)\.(\d+)/.exec(base ?? '')
  if (match === null) return false
  const parts = [Number(match[1]), Number(match[2]), Number(match[3])]
  for (let i = 0; i < FLOOR.length; i++) {
    const have = parts[i] ?? 0
    const want = FLOOR[i] ?? 0
    if (have !== want) return have > want
  }
  return true
}

// Joins a name onto a directory without cleaning the directory's spelling,
// exactly as agentctl builds the same paths, so both sides name one file.
function joinPath(dir: string, name: string): string {
  if (dir === '') return name
  return dir.endsWith('/') ? `${dir}${name}` : `${dir}/${name}`
}

// The configuration home as Claude Code and agentctl derive it: a non-empty
// CLAUDE_CONFIG_DIR as spelled, else HOME's .claude, NFC-normalized.
function configHomeOf(home: string | undefined, configDir: string | undefined): string | undefined {
  if (configDir !== undefined && configDir !== '') return configDir.normalize('NFC')
  if (home === undefined || home === '') return undefined
  return joinPath(home, '.claude').normalize('NFC')
}

function isNonEmpty(value: string | undefined): boolean {
  return value !== undefined && value !== ''
}

function log($: EngineInterface, text: string): void {
  $.ui.log(redact(text), { to: 'debug' })
}

// Runs table mutations one at a time, so the poll, the command and a
// rejected run never interleave a read and a write of $.state.
function serial<T>(work: () => Promise<T>): Promise<T> {
  const run = queue.then(work, work)
  queue = run.catch(() => undefined)
  return run
}

async function sha256(text: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('')
}

function newId(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('')
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function stringOf(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}

async function readRecord($: EngineInterface, path: string): Promise<Record_ | undefined> {
  const stat = await $.fs.stat(path).catch(() => undefined)
  if (stat === undefined || stat.kind !== 'file' || stat.isLink || stat.size > MAX_RECORD_BYTES) return undefined
  const text = await $.fs.read(path).catch(() => undefined)
  if (typeof text !== 'string') return undefined
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    // The parser's message can quote the record, bridge id included, so it is dropped.
    return undefined
  }
  if (!isObject(raw)) return undefined
  const bridge = stringOf(raw.bridgeSessionId)
  return {
    pid: typeof raw.pid === 'number' ? raw.pid : undefined,
    sessionId: stringOf(raw.sessionId),
    status: stringOf(raw.status),
    messagingSocketPath: stringOf(raw.messagingSocketPath),
    bridgeMark: bridge === undefined || bridge === '' ? undefined : await sha256(bridge),
  }
}

// Counts distinct bridges: a new digest, or a bridge seen after an absence,
// is a new generation. After a reload the first bridge seen is taken as the
// one already counted unless the mod saw it absent first.
async function observeBridge($: EngineInterface, record: Record_ | undefined): Promise<Bridge> {
  const held = await $.state.get({ plugin: 'agentctl-remote-control', key: 'generation' })
  let generation = held.value ?? 0
  const mark = record?.bridgeMark
  if (mark === undefined) {
    if (record !== undefined) sawAbsent = true
    return { present: false, generation }
  }
  if (mark !== bridgeMark) {
    const isNew = bridgeMark !== undefined || generation === 0 || sawAbsent
    bridgeMark = mark
    sawAbsent = false
    if (isNew) {
      generation += 1
      await $.state.set({ plugin: 'agentctl-remote-control', key: 'generation' }, generation)
    }
  }
  return { present: true, generation }
}

async function provenanceOf($: EngineInterface): Promise<AgentctlRcProvenance> {
  const configDir = await $.env.get('CLAUDE_CONFIG_DIR')
  const secureStorageDir = await $.env.get('CLAUDE_SECURESTORAGE_CONFIG_DIR')
  return {
    home: (await $.env.get('HOME')) ?? '',
    configDir: { set: configDir !== undefined, value: configDir ?? '' },
    secureStorageDir: { set: secureStorageDir !== undefined, value: secureStorageDir ?? '' },
    oauthTokenSet: isNonEmpty(await $.env.get('CLAUDE_CODE_OAUTH_TOKEN')),
    apiKeySet: isNonEmpty(await $.env.get('ANTHROPIC_API_KEY')),
    baseUrlSet: isNonEmpty(await $.env.get('ANTHROPIC_BASE_URL')),
    authorized: (await $.session.authorize()) !== null,
  }
}

function once<T>(work: () => Promise<T>): () => Promise<T> {
  let held: Promise<T> | undefined
  return () => (held ??= work())
}

async function inputsOf($: EngineInterface, ctx: Context): Promise<Inputs> {
  const record = await readRecord($, ctx.recordPath)
  const bridge = await observeBridge($, record)
  return {
    record,
    bridge,
    isListed: once(async () => (await $.command.list()).some((c) => c.name === 'remote-control')),
    isAuthorized: once(async () => (await $.session.authorize()) !== null),
    surfaces: once(async () => [...(await $.session.surfaces())]),
    provenance: once(() => provenanceOf($)),
  }
}

/** The owner uid, mode and type `/usr/bin/stat` reports for a path, or undefined when it answers anything else. */
async function hostStat($: EngineInterface, path: string): Promise<{ uid: number; mode: string; type: string } | undefined> {
  const ran = await $.process.run([STAT_COMMAND, '-f', '%u %Lp %HT', path], { timeoutMs: 5_000 }).catch(() => undefined)
  if (ran === undefined || ran.exitCode !== 0) return undefined
  const match = STAT_SHAPE.exec(ran.stdout.replace(/\n$/, ''))
  if (match === null) return undefined
  return { uid: Number(match[1]), mode: match[2] ?? '', type: match[3] ?? '' }
}

async function ownUid($: EngineInterface): Promise<number | undefined> {
  const held = await $.state.get({ plugin: 'agentctl-remote-control', key: 'uid' })
  if (typeof held.value === 'number') return held.value
  const ran = await $.process.run([ID_COMMAND, '-u'], { timeoutMs: 5_000 }).catch(() => undefined)
  if (ran === undefined || ran.exitCode !== 0 || !/^\d+\n?$/.test(ran.stdout)) return undefined
  const uid = Number(ran.stdout.trim())
  await $.state.set({ plugin: 'agentctl-remote-control', key: 'uid' }, uid)
  return uid
}

// Establishes the process's own identity and the platform, or says why the
// mod must stay inactive in this session.
async function establish($: EngineInterface): Promise<Context | string> {
  const version = await $.session.version()
  if (!meetsFloor(version.base)) return `Claude Code ${version.base ?? version.version} is older than 2.1.287`
  const socket = await $.env.get('CLAUDE_CODE_MESSAGING_SOCKET')
  const pidMatch = /\/(\d+)\.sock$/.exec(socket ?? '')
  if (socket === undefined || pidMatch === null) return 'the session has no messaging socket naming its process'
  const pid = Number(pidMatch[1])
  const configHome = configHomeOf(await $.env.get('HOME'), await $.env.get('CLAUDE_CONFIG_DIR'))
  if (configHome === undefined) return 'neither CLAUDE_CONFIG_DIR nor HOME is set'
  const recordPath = joinPath(joinPath(configHome, 'sessions'), `${pid}.json`)
  const record = await readRecord($, recordPath)
  if (record === undefined || record.pid !== pid || record.messagingSocketPath !== socket) {
    return 'the session registry record does not match this process'
  }
  const uid = await ownUid($)
  if (uid === undefined) return '/usr/bin/id -u did not answer a uid'
  const probe = await hostStat($, recordPath)
  if (probe === undefined || probe.uid !== uid || probe.type !== 'Regular File') {
    return 'the host stat probe did not answer the macOS shape for the registry record'
  }
  const transportDir = joinPath(joinPath(joinPath(configHome, 'agentctl'), 'remote-control'), String(pid))
  return { pid, configHome, recordPath, transportDir, uid, versionBase: version.base ?? '' }
}

async function loadTable($: EngineInterface): Promise<Table> {
  const held = await $.state.get({ plugin: 'agentctl-remote-control', key: 'requests' })
  return { ...(held.value ?? {}) }
}

async function save($: EngineInterface, table: Table): Promise<void> {
  await $.state.set({ plugin: 'agentctl-remote-control', key: 'requests' }, table)
}

// Drops published entries whose expiry has passed. An entry whose expiry has
// not passed is kept whatever its phase, because its request file may still
// lie in the directory and would otherwise be admitted, and run, a second
// time; once it has passed, such a file can only be refused again. An
// entry not yet published is kept until its answer is delivered.
function evict(table: Table, now: number): boolean {
  let changed = false
  for (const [id, entry] of Object.entries(table)) {
    if (now < entry.expiresAt || entry.phase !== 'published') continue
    delete table[id]
    changed = true
  }
  return changed
}

function pendingCount(table: Table): number {
  return Object.values(table).filter((entry) => entry.phase !== 'published').length
}

function isFull(table: Table): boolean {
  return pendingCount(table) >= MAX_PENDING || Object.keys(table).length >= MAX_ENTRIES
}

function isBusy(record: Record_ | undefined): boolean {
  return turns.size > 0 || (record?.status !== undefined && record.status !== 'idle')
}

async function responseText(inputs: Inputs, ctx: Context, id: string, action: AgentctlRcAction, result: AgentctlRcResult, now: number, reason?: string): Promise<string> {
  const response: AgentctlRcResponse = {
    v: 1,
    id,
    action,
    result,
    answeredAt: now,
    bridge: { present: inputs.bridge.present, generation: inputs.bridge.generation },
    surfaces: await inputs.surfaces(),
    version: ctx.versionBase,
    remoteControlListed: await inputs.isListed(),
    provenance: await inputs.provenance(),
  }
  if (reason !== undefined) response.reason = reason
  return JSON.stringify(response)
}

function toastText(result: AgentctlRcResult, reason: string | undefined): string {
  switch (result) {
    case 'reconnected':
      return 'Remote Control reconnected.'
    case 'already_connected':
      return 'Remote Control is already connected; nothing was run.'
    case 'unavailable':
      return `Remote Control is unavailable in this session (${reason ?? 'unknown'}); nothing was run.`
    case 'not_confirmed':
      return `Remote Control was started, but no new bridge was seen (${reason ?? 'unknown'}).`
    case 'expired':
      return 'The reconnect expired before the session was idle; nothing was run.'
    case 'cancelled':
      return 'The reconnect was cancelled; nothing was run.'
    case 'ok':
      return 'Remote Control status answered.'
  }
}

// Writes into the transport directory only when it still exists, because
// $.fs.write would create it and the directory is agentctl's to create.
async function writeTransport($: EngineInterface, ctx: Context, name: string, text: string): Promise<boolean> {
  if (!(await $.fs.exists(ctx.transportDir))) return false
  await $.fs.write(joinPath(ctx.transportDir, name), text)
  return true
}

// A delivery counts as published only once the file was written; while the
// directory is missing it stays pending and the same text is tried again.
async function deliverAck($: EngineInterface, ctx: Context, table: Table, id: string, entry: AgentctlRcEntry): Promise<void> {
  if (entry.ack === undefined || entry.ack.published) return
  if (!(await writeTransport($, ctx, `${id}.ack.json`, entry.ack.text))) return
  entry.ack.published = true
  await save($, table)
}

async function deliverFinal($: EngineInterface, ctx: Context, table: Table, id: string, entry: AgentctlRcEntry): Promise<void> {
  const terminal = entry.terminal
  if (terminal === undefined) {
    // A rejection has only its acknowledgement to deliver.
    if (entry.ack !== undefined && !entry.ack.published) return
    entry.phase = 'published'
    await save($, table)
    return
  }
  if (!terminal.published) {
    if (entry.origin === 'file') {
      if (!(await writeTransport($, ctx, `${id}.response.json`, terminal.response))) return
    } else {
      const answer = JSON.parse(terminal.response) as AgentctlRcResponse
      $.ui.toast(toastText(answer.result, answer.reason))
    }
  }
  terminal.published = true
  entry.phase = 'published'
  await save($, table)
}

// Stores the final answer before delivering it, so a reload in between
// delivers the identical text instead of computing a new one.
async function finish($: EngineInterface, ctx: Context, table: Table, inputs: Inputs, id: string, entry: AgentctlRcEntry, result: AgentctlRcResult, now: number, reason?: string): Promise<void> {
  entry.phase = 'final'
  entry.terminal = { response: await responseText(inputs, ctx, id, entry.action, result, now, reason), published: false }
  await save($, table)
  log($, `request ${id}: ${result}${reason === undefined ? '' : ` (${reason})`}`)
  await deliverFinal($, ctx, table, id, entry)
}

async function onRunRejected($: EngineInterface, id: string): Promise<void> {
  const ctx = context
  if (ctx === undefined) return
  const table = await loadTable($)
  const entry = table[id]
  if (entry?.phase !== 'running') return
  await finish($, ctx, table, await inputsOf($, ctx), id, entry, 'unavailable', await $.clock.now(), 'run_rejected')
}

// The rule list for a reconnect that has not been run yet, in the order the
// transport contract fixes; `remote-control` is issued only from an idle
// session whose record, read this tick, shows no bridge.
async function applyRules($: EngineInterface, ctx: Context, table: Table, inputs: Inputs, id: string, entry: AgentctlRcEntry, now: number): Promise<void> {
  if (now >= entry.expiresAt) return finish($, ctx, table, inputs, id, entry, 'expired', now)
  if (!(await inputs.isListed())) return finish($, ctx, table, inputs, id, entry, 'unavailable', now, 'command_not_listed')
  if (!(await inputs.isAuthorized())) return finish($, ctx, table, inputs, id, entry, 'unavailable', now, 'not_authorized')
  if (inputs.bridge.present) return finish($, ctx, table, inputs, id, entry, 'already_connected', now)
  const record = inputs.record
  if (record === undefined || isBusy(record)) {
    // An unreadable record is treated like a running turn: nothing is
    // issued without seeing the bridge absent, and expiry ends the wait.
    if (entry.phase !== 'waiting_idle') {
      entry.phase = 'waiting_idle'
      await save($, table)
    }
    return
  }
  entry.phase = 'running'
  entry.runStartedAt = now
  entry.observeUntil = now + OBSERVE_MS
  if (record.sessionId !== undefined) entry.runSessionId = record.sessionId
  await save($, table)
  log($, `request ${id}: running remote-control`)
  // Not awaited: the command's own text can carry the bridge URL and is
  // never read; the registry record is what confirms the bridge.
  void $.command.run({ command: 'remote-control' }).catch(() => serial(() => onRunRejected($, id)))
}

// The one rule for a run already issued: it is observed, never issued again.
async function observeRun($: EngineInterface, ctx: Context, table: Table, inputs: Inputs, id: string, entry: AgentctlRcEntry, now: number): Promise<void> {
  const record = inputs.record
  if (record === undefined || record.pid !== ctx.pid || record.sessionId !== entry.runSessionId) {
    return finish($, ctx, table, inputs, id, entry, 'not_confirmed', now, 'record_changed')
  }
  if (inputs.bridge.present) return finish($, ctx, table, inputs, id, entry, 'reconnected', now)
  if (now >= (entry.observeUntil ?? 0)) return finish($, ctx, table, inputs, id, entry, 'not_confirmed', now, 'window_elapsed')
}

async function step($: EngineInterface, ctx: Context, table: Table, inputs: Inputs, id: string, entry: AgentctlRcEntry, now: number): Promise<void> {
  await deliverAck($, ctx, table, id, entry)
  switch (entry.phase) {
    case 'published':
      return
    case 'final':
      return deliverFinal($, ctx, table, id, entry)
    case 'running':
      return observeRun($, ctx, table, inputs, id, entry, now)
    case 'accepted':
    case 'waiting_idle':
      if (entry.action === 'status') return finish($, ctx, table, inputs, id, entry, 'ok', now)
      return applyRules($, ctx, table, inputs, id, entry, now)
  }
}

// The transport directory as it really lies, or undefined when it is
// missing, not a directory, or resolves anywhere but where agentctl makes it.
async function transportReal($: EngineInterface, ctx: Context): Promise<string | undefined> {
  if (!(await $.fs.exists(ctx.transportDir))) return undefined
  const dir = await $.fs.stat(ctx.transportDir, { resolve: true }).catch(() => undefined)
  const home = await $.fs.stat(ctx.configHome, { resolve: true }).catch(() => undefined)
  if (dir?.realPath === undefined || home?.realPath === undefined || dir.kind !== 'dir' || dir.isLink) return undefined
  const expected = joinPath(joinPath(joinPath(home.realPath, 'agentctl'), 'remote-control'), String(ctx.pid))
  return dir.realPath === expected ? dir.realPath : undefined
}

// The acknowledgement of a refused file. `action` is empty when the body was
// not read or cannot be trusted, and the request's own action otherwise.
function rejectedAck(id: string, action: string, reason: AgentctlRcRejectReason, now: number): string {
  const ack: AgentctlRcAck = { v: 1, id, action, state: 'rejected', reason, answeredAt: now }
  return JSON.stringify(ack)
}

function rejection(id: string, action: string, reason: AgentctlRcRejectReason, now: number): AgentctlRcEntry {
  return {
    phase: 'final',
    action: 'status',
    origin: 'file',
    admittedAt: now,
    expiresAt: now + MAX_LIFETIME_MS,
    ack: { text: rejectedAck(id, action, reason, now), published: false },
  }
}

// Validates the body and metadata of one request file whose name and real
// path already passed, in the contract's order, and answers the entry to store.
async function validate($: EngineInterface, ctx: Context, table: Table, id: string, path: string, size: number, now: number): Promise<AgentctlRcEntry> {
  const meta = await hostStat($, path)
  if (meta === undefined || meta.uid !== ctx.uid || meta.mode !== '600' || meta.type !== 'Regular File' || size > MAX_REQUEST_BYTES) {
    return rejection(id, '', 'metadata', now)
  }
  const text = await $.fs.read(path).catch(() => undefined)
  let body: unknown
  try {
    body = typeof text === 'string' ? JSON.parse(text) : undefined
  } catch {
    body = undefined
  }
  // A body under an unknown version cannot be trusted to name its action.
  if (!isObject(body) || body.v !== 1) return rejection(id, '', 'version', now)
  const rawAction = stringOf(body.action) ?? ''
  if (rawAction !== 'status' && rawAction !== 'reconnect') return rejection(id, rawAction, 'action', now)
  const action: AgentctlRcAction = rawAction
  const expiresAt = body.expiresAt
  // A lifetime beyond the cap would keep its entry, and the table slot,
  // longer than the table's eviction rule allows for.
  if (typeof expiresAt !== 'number' || !Number.isFinite(expiresAt) || expiresAt <= now || expiresAt - now > MAX_LIFETIME_MS) {
    return rejection(id, action, 'expired', now)
  }
  if (body.id !== id) {
    const other = stringOf(body.id)
    return rejection(id, action, other !== undefined && other in table ? 'duplicate' : 'name', now)
  }
  const subject = body.subject
  if (!isObject(subject) || typeof subject.service !== 'string' || subject.service === '') return rejection(id, action, 'subject', now)

  const ack: AgentctlRcAck = { v: 1, id, action, state: 'accepted', answeredAt: now }
  return { phase: 'accepted', action, origin: 'file', admittedAt: now, expiresAt, ack: { text: JSON.stringify(ack), published: false } }
}

// Admits the request files not yet in the table. A file whose name or real
// path fails is ignored without an answer; past that, a full table answers
// `busy` once per id without reading the file, and admits nothing.
async function admit($: EngineInterface, ctx: Context, table: Table, inputs: Inputs, now: number): Promise<void> {
  const realDir = await transportReal($, ctx)
  if (realDir === undefined) return
  const ids = new Map<string, string>()
  for (const entry of await $.fs.list(ctx.transportDir)) {
    const match = entry.isLink ? null : REQUEST_NAME.exec(entry.name)
    if (match !== null) ids.set(entry.name, match[1] ?? '')
  }
  const listed = new Set(ids.values())
  for (const id of answeredBusy) if (!listed.has(id)) answeredBusy.delete(id)
  for (const name of [...ids.keys()].sort()) {
    const id = ids.get(name) ?? ''
    if (id in table || answeredBusy.has(id)) continue
    try {
      const path = joinPath(ctx.transportDir, name)
      const stat = await $.fs.stat(path, { resolve: true }).catch(() => undefined)
      if (stat?.realPath !== joinPath(realDir, name)) continue
      if (isFull(table)) {
        if (await writeTransport($, ctx, `${id}.ack.json`, rejectedAck(id, '', 'busy', now))) answeredBusy.add(id)
        log($, `request ${id}: rejected, the table is full`)
        continue
      }
      const entry = await validate($, ctx, table, id, path, stat.size, now)
      table[id] = entry
      await save($, table)
      log($, `request ${id}: ${entry.phase === 'accepted' ? `accepted ${entry.action}` : 'rejected'}`)
      await step($, ctx, table, inputs, id, entry, now)
    } catch {
      log($, `request file ${name}: admission failed`)
    }
  }
}

async function poll($: EngineInterface, ctx: Context): Promise<void> {
  const now = await $.clock.now()
  const inputs = await inputsOf($, ctx)
  const table = await loadTable($)
  if (evict(table, now)) await save($, table)
  for (const [id, entry] of Object.entries(table)) {
    if (entry.phase === 'published') continue
    try {
      await step($, ctx, table, inputs, id, entry, now)
    } catch {
      log($, `request ${id}: delivery failed; retrying on the next poll`)
    }
  }
  await admit($, ctx, table, inputs, now)
}

function tick($: EngineInterface): void {
  const ctx = context
  if (ctx === undefined || isPolling) return
  isPolling = true
  serial(() => poll($, ctx))
    .catch((error: unknown) => log($, `poll failed: ${String(error)}`))
    .finally(() => {
      isPolling = false
    })
}

async function statusText($: EngineInterface): Promise<string> {
  const lines: string[] = []
  const version = await $.session.version()
  const ctx = context
  if (ctx === undefined) {
    lines.push(`agentctl-remote-control is inactive: ${refusal ?? 'not started'}.`)
  } else {
    const bridge = await observeBridge($, await readRecord($, ctx.recordPath))
    lines.push(`Remote Control bridge: ${bridge.present ? 'present' : 'absent'}, generation ${bridge.generation}`)
  }
  const provenance = await provenanceOf($)
  const setOrNot = (value: boolean): string => (value ? 'set' : 'unset')
  // The directory spellings name the account layout of the machine and are
  // kept out of the transcript; only the response file carries them.
  lines.push(
    `Surfaces: ${(await $.session.surfaces()).join(', ') || 'none'}`,
    `Claude Code: ${version.base ?? version.version}`,
    `remote-control command: ${(await $.command.list()).some((c) => c.name === 'remote-control') ? 'listed' : 'not listed'}`,
    'Provenance:',
    `  HOME                             ${setOrNot((await $.env.get('HOME')) !== undefined)}`,
    `  CLAUDE_CONFIG_DIR                ${setOrNot(provenance.configDir.set)}`,
    `  CLAUDE_SECURESTORAGE_CONFIG_DIR  ${setOrNot(provenance.secureStorageDir.set)}`,
    `  CLAUDE_CODE_OAUTH_TOKEN          ${setOrNot(provenance.oauthTokenSet)}`,
    `  ANTHROPIC_API_KEY                ${setOrNot(provenance.apiKeySet)}`,
    `  ANTHROPIC_BASE_URL               ${setOrNot(provenance.baseUrlSet)}`,
    `  first-party authorization        ${provenance.authorized ? 'yes' : 'no'}`,
  )
  return redact(lines.join('\n'))
}

// Enqueues a reconnect through the same table a request file goes through;
// the poll applies the rules, because $.command.run is refused inside a hook
// the turn is waiting on.
async function requestReconnect($: EngineInterface): Promise<string> {
  if (context === undefined) return redact(`agentctl-remote-control is inactive: ${refusal ?? 'not started'}.`)
  return serial(async () => {
    const table = await loadTable($)
    if (Object.values(table).some((e) => e.origin === 'command' && e.phase !== 'published')) return 'A reconnect is already requested.'
    if (isFull(table)) return 'Too many requests are pending; try again shortly.'
    const now = await $.clock.now()
    table[newId()] = { phase: 'accepted', action: 'reconnect', origin: 'command', admittedAt: now, expiresAt: now + COMMAND_LIFETIME_MS }
    await save($, table)
    return 'requested'
  })
}

// Marks what is still pending as ended when the session ends for good.
async function endPending($: EngineInterface, ctx: Context): Promise<void> {
  const now = await $.clock.now()
  const table = await loadTable($)
  const inputs = await inputsOf($, ctx)
  for (const [id, entry] of Object.entries(table)) {
    if (entry.phase === 'accepted' || entry.phase === 'waiting_idle') {
      await finish($, ctx, table, inputs, id, entry, 'cancelled', now, 'session_ended').catch(() => undefined)
    } else if (entry.phase === 'running') {
      await finish($, ctx, table, inputs, id, entry, 'not_confirmed', now, 'session_ended').catch(() => undefined)
    }
  }
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    ticker?.cancel()
    ticker = undefined
    context = undefined
    refusal = undefined
    turns = new Set<string>()
    isPolling = false
    bridgeMark = undefined
    sawAbsent = false
    queue = Promise.resolve()
    answeredBusy = new Set<string>()

    await $.command.register({
      name: 'agentctl-rc',
      description: 'Show this session\'s Remote Control bridge status, or "reconnect" it.',
      argumentHint: '[reconnect]',
      immediate: true,
    })
    const established = await establish($).catch((error: unknown) => `start-up failed: ${String(error)}`)
    if (typeof established === 'string') {
      // Stored redacted: a host error can quote a URL or a bridge id, and
      // the refusal is repeated in command answers.
      refusal = redact(established)
      log($, `inactive: ${refusal}`)
      return next(e)
    }
    context = established
    ticker = $.clock.every(POLL_MS, () => tick($))
    return next(e)
  })

  on('session.end', async ($, e, next) => {
    // A /clear ends the conversation but not the process: its registry
    // record and transport directory stay, and no session.start follows, so
    // the poll keeps running for the process's next conversation.
    if (e.reason !== 'clear') {
      ticker?.cancel()
      ticker = undefined
      const ctx = context
      context = undefined
      if (ctx !== undefined) await serial(() => endPending($, ctx)).catch(() => undefined)
    }
    return next(e)
  })

  on('turn.start', async ($, e, next) => {
    turns.add(e.turnId)
    return next(e)
  })

  on('turn.complete', async ($, e, next) => {
    turns.delete(e.turnId)
    return next(e)
  })

  on('command.run', { command: 'agentctl-rc' }, async ($, e) => {
    const args = e.args.trim()
    if (args === '') return { text: await statusText($) }
    if (args === 'reconnect') return { text: await requestReconnect($) }
    return { text: 'Usage: /agentctl-rc [reconnect]' }
  }).catch(() => ({ text: '/agentctl-rc failed; the debug log has the reason.' }))
}
