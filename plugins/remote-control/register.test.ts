import type { On } from 'claude-code'
import { describe, expect, mock, test } from 'claude-code/testing'
import type { Engine, MockClock } from 'claude-code/testing'

import { meetsFloor, redact } from './hooks/register'

const HOME = '/Users/tester'
const CONFIG_HOME = `${HOME}/.claude`
const PID = 4242
const SOCKET = `/private/tmp/user/501/cc-socks/${PID}.sock`
const RECORD = `${CONFIG_HOME}/sessions/${PID}.json`
const DIR = `${CONFIG_HOME}/agentctl/remote-control/${PID}`
const SEEDED = 'session_01SEEDEDbridgeSecretXyz'
const NEXT_BRIDGE = 'session_01NEXTbridgeSecretAbc'
const START = 1_760_000_000_000
const SERVICE = 'Claude Code-credentials'
const ID_A = 'a'.repeat(32)
const ID_B = 'b'.repeat(32)
const ID_C = '0123456789abcdef0123456789abcdef'

type FileRec = { kind: 'file' | 'dir'; text: string; uid: number; mode: string; real?: string }
type RecordFields = { pid: number; sessionId: string; status: string; version: string; messagingSocketPath: string; bridgeSessionId?: string }
type WorldOptions = {
  version?: string
  bridge?: string
  status?: string
  env?: Record<string, string>
}

function ran(exitCode: number, stdout: string) {
  return { exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
}

/**
 * Everything beneath the mod: a file system in memory, the two host
 * commands, the command list, the engine's version and authorization, and
 * what the mod wrote, ran, logged and showed.
 */
class World {
  readonly files = new Map<string, FileRec>()
  readonly argv: string[][] = []
  readonly runs: string[] = []
  readonly toasts: string[] = []
  readonly logs: string[] = []
  readonly writes: { path: string; text: string }[] = []
  readonly failedWrites: { path: string; text: string }[] = []
  readonly registered: string[] = []
  readonly lists: string[] = []
  failResponseWrites = 0
  isListed = true
  isAuthorized = true
  isRunRefused = false
  statOverride: ((path: string) => string | undefined) | undefined
  onRemoteControl: (() => void) | undefined
  record: RecordFields
  readonly clock: MockClock
  private readonly version: string

  constructor(on: On, options: WorldOptions = {}) {
    this.version = options.version ?? '2.1.296'
    this.record = { pid: PID, sessionId: 'sess-1', status: options.status ?? 'idle', version: this.version, messagingSocketPath: SOCKET }
    if (options.bridge !== undefined) this.record.bridgeSessionId = options.bridge
    this.dir(CONFIG_HOME)
    this.dir(`${CONFIG_HOME}/sessions`)
    this.dir(`${CONFIG_HOME}/agentctl`)
    this.dir(`${CONFIG_HOME}/agentctl/remote-control`)
    this.dir(DIR)
    this.writeRecord()

    this.clock = mock.clock(on, { now: START })
    mock.env(on, { HOME, CLAUDE_CODE_MESSAGING_SOCKET: SOCKET, ...(options.env ?? {}) })

    on('fs.exists', (_$, e) => ({ value: this.files.has(e.path) }))
    on('fs.stat', (_$, e) => {
      const rec = this.files.get(e.path)
      if (rec === undefined) return { deny: `ENOENT: ${e.path}` }
      const stat = { kind: rec.kind, size: rec.text.length, mtimeMs: 0, isLink: false }
      return { value: e.resolve ? { ...stat, realPath: rec.real ?? e.path } : stat }
    })
    on('fs.read', (_$, e) => {
      const rec = this.files.get(e.path)
      return rec?.kind === 'file' ? { value: rec.text } : { deny: `ENOENT: ${e.path}` }
    })
    on('fs.list', (_$, e) => {
      this.lists.push(e.path)
      const entries = [...this.files.entries()]
        .filter(([path]) => path.startsWith(`${e.path}/`) && !path.slice(e.path.length + 1).includes('/'))
        .map(([path, rec]) => ({ name: path.slice(e.path.length + 1), kind: rec.kind, size: rec.text.length, mtimeMs: 0, isLink: false }))
      return { value: entries }
    })
    on('fs.write', (_$, e) => {
      if (e.path.endsWith('.response.json') && this.failResponseWrites > 0) {
        this.failResponseWrites -= 1
        this.failedWrites.push({ path: e.path, text: e.text })
        return { deny: 'EIO: simulated write failure' }
      }
      this.files.set(e.path, { kind: 'file', text: e.text, uid: 501, mode: '644' })
      this.writes.push({ path: e.path, text: e.text })
      return { value: undefined }
    })
    on('process.run', (_$, e) => {
      const argv = [...e.argv]
      this.argv.push(argv)
      if (argv[0] === '/usr/bin/id' && argv[1] === '-u') return { value: ran(0, '501\n') }
      if (argv[0] === '/usr/bin/stat' && argv[1] === '-f' && argv[2] === '%u %Lp %HT') {
        const path = argv[3] ?? ''
        const override = this.statOverride?.(path)
        if (override !== undefined) return { value: ran(0, override) }
        const rec = this.files.get(path)
        if (rec === undefined) return { value: ran(1, '') }
        return { value: ran(0, `${rec.uid} ${rec.mode} ${rec.kind === 'dir' ? 'Directory' : 'Regular File'}\n`) }
      }
      return { value: ran(127, '') }
    })
    on('command.register', (_$, e) => {
      this.registered.push(e.name)
      return { value: { command: e.name } }
    })
    on('command.list', () => ({
      value: [
        { name: 'status', description: '', source: 'builtin' as const },
        ...(this.isListed ? [{ name: 'remote-control', description: '', source: 'builtin' as const }] : []),
      ],
    }))
    on('command.run', (_$, e) => {
      if (e.command !== 'remote-control') return { text: '' }
      // A throw leaves the call unanswered, so the caller's promise rejects as
      // Claude Code's own refusal of an unknown command does.
      if (this.isRunRefused) throw new Error('remote-control refused')
      this.runs.push(e.command)
      this.onRemoteControl?.()
      // The real command can print the bridge URL; the mod must never repeat it.
      return { text: `Remote Control active: https://claude.ai/code/${SEEDED}` }
    })
    on('session.version', () => ({ value: { version: this.version, base: this.version } }))
    on('session.authorize', () => ({ value: this.isAuthorized ? { handle: 'opaque', kind: 'bearer' as const } : null }))
    on('session.surfaces', () => ({ value: ['terminal' as const] }))
    on('ui.toast', (_$, e) => {
      this.toasts.push(e.text)
      return { value: undefined }
    })
    on('ui.log', (_$, e) => {
      this.logs.push(e.text)
      return { value: undefined }
    })
    on('session.start', (_$, e) => ({ cwd: e.cwd }))
    on('session.end', (_$, e) => ({ sessionId: e.sessionId }))
    on('turn.start', (_$, e) => ({ turnId: e.turnId }))
    on('turn.complete', (_$, e) => ({ text: e.answer }))
  }

  dir(path: string): void {
    this.files.set(path, { kind: 'dir', text: '', uid: 501, mode: '700' })
  }

  writeRecord(): void {
    this.files.set(RECORD, { kind: 'file', text: JSON.stringify(this.record), uid: 501, mode: '600' })
  }

  setRecord(patch: Partial<RecordFields>): void {
    this.record = { ...this.record, ...patch }
    if (patch.bridgeSessionId === '') delete this.record.bridgeSessionId
    this.writeRecord()
  }

  /** Places a request as agentctl writes it: mode 0600, owned by the session's uid. */
  request(id: string, body: Record<string, unknown> = {}, file: Partial<FileRec> & { name?: string } = {}): void {
    const { name, ...rec } = file
    const text = JSON.stringify({ v: 1, id, action: 'reconnect', issuedAt: this.clock.now(), expiresAt: this.clock.now() + 75_000, subject: { service: SERVICE }, ...body })
    this.files.set(`${DIR}/${name ?? `${id}.request.json`}`, { kind: 'file', text, uid: 501, mode: '600', ...rec })
  }

  ack(id: string): Record<string, unknown> | undefined {
    const rec = this.files.get(`${DIR}/${id}.ack.json`)
    return rec === undefined ? undefined : JSON.parse(rec.text)
  }

  response(id: string): Record<string, unknown> | undefined {
    const rec = this.files.get(`${DIR}/${id}.response.json`)
    return rec === undefined ? undefined : JSON.parse(rec.text)
  }

  writesTo(suffix: string): { path: string; text: string }[] {
    return this.writes.filter((w) => w.path.endsWith(suffix))
  }

  /** Everything the mod produced that a person or agentctl could read. */
  outputs(): string {
    return [...this.writes.map((w) => w.text), ...this.toasts, ...this.logs].join('\n')
  }
}

async function start($: Engine): Promise<void> {
  await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: true })
}

// A hot reload re-fires session.start in a fresh module environment; the
// mod rebuilds every module variable there, so raising it again stands for
// one while $.state carries over.
const reload = start

async function command($: Engine, args = ''): Promise<string> {
  const typed = { command: 'agentctl-rc', args, origin: { kind: 'composer' as const }, presentation: { isFullscreen: false, columns: 120 } }
  return (await $.command.run(typed)).text ?? ''
}

async function turnStart($: Engine, turnId = 'turn-1'): Promise<void> {
  await $.turn.start({ text: 'work', turnId })
}

async function turnComplete($: Engine, turnId = 'turn-1'): Promise<void> {
  await $.turn.complete({ answer: 'done', durationMs: 10, isAborted: false, turnId, reason: 'answer' })
}

describe('admission', () => {
  test('ignores a file whose name is not a 32-hex request name', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, {}, { name: 'deadbeef.request.json' })
    w.request(ID_B, {}, { name: `${ID_B.toUpperCase()}.request.json` })
    await start($)
    await w.clock.advance(1_000)
    expect(w.writes).toEqual([])
    expect(w.argv.filter((a) => (a[3] ?? '').startsWith(DIR))).toEqual([])
  })

  test('ignores a request whose real path leaves the transport directory', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, {}, { real: '/Users/tester/elsewhere/planted.request.json' })
    await start($)
    await w.clock.advance(1_000)
    expect(w.ack(ID_A)).toBeUndefined()
    expect(w.writes).toEqual([])
  })

  test('rejects a request owned by a foreign uid', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, {}, { uid: 502 })
    await start($)
    await w.clock.advance(500)
    expect(w.ack(ID_A)).toEqual({ v: 1, id: ID_A, action: '', state: 'rejected', reason: 'metadata', answeredAt: START + 500 })
    expect(w.response(ID_A)).toBeUndefined()
    expect(w.runs).toEqual([])
  })

  test('rejects a request whose mode is not 600', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, {}, { mode: '644' })
    await start($)
    await w.clock.advance(500)
    expect(w.ack(ID_A)?.state).toBe('rejected')
    expect(w.ack(ID_A)?.reason).toBe('metadata')
    expect(w.runs).toEqual([])
  })

  test('rejects a request already expired at admission', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, { expiresAt: START + 100 })
    await start($)
    await w.clock.advance(500)
    expect(w.ack(ID_A)).toEqual({ v: 1, id: ID_A, action: 'reconnect', state: 'rejected', reason: 'expired', answeredAt: START + 500 })
    expect(w.response(ID_A)).toBeUndefined()
    expect(w.runs).toEqual([])
  })

  test('rejects a second file that reuses an admitted id, and answers the first once', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, { action: 'status', expiresAt: START + 3_000 })
    await start($)
    await w.clock.advance(500)
    expect(w.response(ID_A)?.result).toBe('ok')
    w.request(ID_B, { id: ID_A, action: 'status', expiresAt: START + 3_000 })
    await w.clock.advance(1_000)
    expect(w.ack(ID_B)?.reason).toBe('duplicate')
    expect(w.writesTo(`${ID_A}.response.json`).length).toBe(1)
    expect(w.writesTo(`${ID_A}.ack.json`).length).toBe(1)
  })

  test('acknowledges once and answers status from the seeded registry record', async ($, on) => {
    const w = new World(on, { bridge: SEEDED })
    w.request(ID_C, { action: 'status', expiresAt: START + 3_000 })
    await start($)
    await w.clock.advance(1_500)
    expect(w.ack(ID_C)).toEqual({ v: 1, id: ID_C, action: 'status', state: 'accepted', answeredAt: START + 500 })
    expect(w.response(ID_C)).toEqual({
      v: 1,
      id: ID_C,
      action: 'status',
      result: 'ok',
      answeredAt: START + 500,
      bridge: { present: true, generation: 1 },
      surfaces: ['terminal'],
      version: '2.1.296',
      remoteControlListed: true,
      provenance: {
        home: HOME,
        configDir: { set: false, value: '' },
        secureStorageDir: { set: false, value: '' },
        oauthTokenSet: false,
        apiKeySet: false,
        baseUrlSet: false,
        authorized: true,
      },
    })
    expect(w.writes.length).toBe(2)
    expect(w.outputs()).not.toContain(SEEDED)
  })

  test('answers status with remoteControlListed false when remote-control is not listed', async ($, on) => {
    const w = new World(on)
    w.isListed = false
    w.request(ID_A, { action: 'status', expiresAt: START + 3_000 })
    await start($)
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'ok', remoteControlListed: false })
    expect(await command($)).toContain('remote-control command: not listed')
    expect(w.runs).toEqual([])
  })

  test('reports provenance overrides as presence and spellings only', async ($, on) => {
    const w = new World(on, {
      env: { CLAUDE_CONFIG_DIR: '', CLAUDE_SECURESTORAGE_CONFIG_DIR: '/Users/tester/vault', CLAUDE_CODE_OAUTH_TOKEN: 'sk-ant-oat-secret', ANTHROPIC_BASE_URL: 'http://127.0.0.1:18764' },
    })
    w.isAuthorized = false
    w.request(ID_A, { action: 'status', expiresAt: START + 3_000 })
    await start($)
    await w.clock.advance(500)
    expect(w.response(ID_A)?.provenance).toEqual({
      home: HOME,
      configDir: { set: true, value: '' },
      secureStorageDir: { set: true, value: '/Users/tester/vault' },
      oauthTokenSet: true,
      apiKeySet: false,
      baseUrlSet: true,
      authorized: false,
    })
    expect(w.outputs()).not.toContain('sk-ant-oat-secret')
    expect(w.outputs()).not.toContain('127.0.0.1:18764')
  })
})

describe('reconnect rules', () => {
  test('answers already_connected at admission without running', async ($, on) => {
    const w = new World(on, { bridge: SEEDED })
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.ack(ID_A)?.state).toBe('accepted')
    expect(w.response(ID_A)?.result).toBe('already_connected')
    expect(w.runs).toEqual([])
  })

  test('answers already_connected when a bridge appears while a turn runs, without running', async ($, on) => {
    const w = new World(on)
    w.request(ID_A)
    await start($)
    await turnStart($)
    await w.clock.advance(1_000)
    expect(w.ack(ID_A)?.state).toBe('accepted')
    expect(w.response(ID_A)).toBeUndefined()
    w.setRecord({ bridgeSessionId: SEEDED })
    await turnComplete($)
    await w.clock.advance(500)
    expect(w.response(ID_A)?.result).toBe('already_connected')
    expect(w.runs).toEqual([])
  })

  test('defers a reconnect while a turn runs and runs it once the turn completes', async ($, on) => {
    const w = new World(on)
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    w.request(ID_A)
    await start($)
    await turnStart($)
    await w.clock.advance(2_000)
    expect(w.runs).toEqual([])
    await turnComplete($)
    await w.clock.advance(500)
    expect(w.runs).toEqual(['remote-control'])
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'reconnected', bridge: { present: true, generation: 1 } })
    await w.clock.advance(5_000)
    expect(w.runs).toEqual(['remote-control'])
    expect(w.writesTo('.response.json').length).toBe(1)
  })

  test('treats a busy registry status as a running turn', async ($, on) => {
    const w = new World(on, { status: 'busy' })
    w.request(ID_A)
    await start($)
    await w.clock.advance(1_500)
    expect(w.runs).toEqual([])
    w.setRecord({ status: 'idle' })
    await w.clock.advance(500)
    expect(w.runs).toEqual(['remote-control'])
  })

  test('expires a reconnect that is still waiting at its deadline, without running', async ($, on) => {
    const w = new World(on)
    w.request(ID_A, { expiresAt: START + 2_200 })
    await start($)
    await turnStart($)
    await w.clock.advance(2_000)
    expect(w.response(ID_A)).toBeUndefined()
    await w.clock.advance(500)
    expect(w.response(ID_A)?.result).toBe('expired')
    await turnComplete($)
    await w.clock.advance(1_000)
    expect(w.runs).toEqual([])
  })

  test('answers unavailable when remote-control is not listed', async ($, on) => {
    const w = new World(on)
    w.isListed = false
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'unavailable', reason: 'command_not_listed' })
    expect(w.runs).toEqual([])
  })

  test('answers unavailable without first-party authorization', async ($, on) => {
    const w = new World(on)
    w.isAuthorized = false
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'unavailable', reason: 'not_authorized' })
    expect(w.runs).toEqual([])
  })

  test('answers unavailable when Claude Code refuses the run', async ($, on) => {
    const w = new World(on)
    w.isRunRefused = true
    w.request(ID_A)
    await start($)
    await w.clock.advance(1_000)
    expect(w.response(ID_A)).toMatchObject({ result: 'unavailable', reason: 'run_rejected' })
    await w.clock.advance(2_000)
    expect(w.writesTo('.response.json').length).toBe(1)
  })

  test('answers not_confirmed when the registry record disappears during a run', async ($, on) => {
    const w = new World(on)
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.runs).toEqual(['remote-control'])
    w.files.delete(RECORD)
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'not_confirmed', reason: 'record_changed' })
  })

  test('answers not_confirmed when the record names another session during a run', async ($, on) => {
    const w = new World(on)
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    w.setRecord({ sessionId: 'sess-2', bridgeSessionId: NEXT_BRIDGE })
    await w.clock.advance(500)
    expect(w.response(ID_A)).toMatchObject({ result: 'not_confirmed', reason: 'record_changed' })
  })
})

describe('reload', () => {
  test('resumes a request waiting for idle after a reload', async ($, on) => {
    const w = new World(on, { status: 'busy' })
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    w.request(ID_A)
    await start($)
    await w.clock.advance(1_000)
    expect(w.ack(ID_A)?.state).toBe('accepted')
    expect(w.runs).toEqual([])
    await reload($)
    w.setRecord({ status: 'idle' })
    await w.clock.advance(1_000)
    expect(w.runs).toEqual(['remote-control'])
    expect(w.response(ID_A)?.result).toBe('reconnected')
    expect(w.writesTo('.ack.json').length).toBe(1)
  })

  test('observes a run issued before a reload for the rest of its window without running again', async ($, on) => {
    const w = new World(on)
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.runs).toEqual(['remote-control'])
    await w.clock.advance(20_000)
    await reload($)
    await w.clock.advance(9_500)
    expect(w.response(ID_A)).toBeUndefined()
    await w.clock.advance(1_000)
    expect(w.response(ID_A)).toMatchObject({ result: 'not_confirmed', reason: 'window_elapsed' })
    expect(w.runs).toEqual(['remote-control'])
    await w.clock.advance(2_000)
    expect(w.writesTo('.response.json').length).toBe(1)
  })

  test('delivers the stored response once, byte for byte, when a reload interrupts its delivery', async ($, on) => {
    const w = new World(on, { bridge: SEEDED })
    w.failResponseWrites = 1
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    expect(w.failedWrites.length).toBe(1)
    expect(w.response(ID_A)).toBeUndefined()
    await reload($)
    await w.clock.advance(1_500)
    const delivered = w.writesTo(`${ID_A}.response.json`)
    expect(delivered.length).toBe(1)
    expect(delivered[0]?.text).toBe(w.failedWrites[0]?.text)
    expect(w.runs).toEqual([])
  })

  test('runs /usr/bin/id once across a reload and no host command but id and stat', async ($, on) => {
    const w = new World(on)
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    w.request(ID_A)
    await start($)
    await w.clock.advance(1_000)
    await reload($)
    w.request(ID_B, { action: 'status', expiresAt: w.clock.now() + 3_000 })
    await w.clock.advance(500)
    expect(w.argv.filter((a) => a[0] === '/usr/bin/id')).toEqual([['/usr/bin/id', '-u']])
    for (const argv of w.argv) {
      expect(['/usr/bin/id', '/usr/bin/stat']).toContain(argv[0])
    }
    expect(w.argv.filter((a) => a[0] === '/usr/bin/stat').map((a) => a[3])).toEqual([
      RECORD,
      `${DIR}/${ID_A}.request.json`,
      RECORD,
      `${DIR}/${ID_B}.request.json`,
    ])
  })
})

describe('lifecycle', () => {
  test('stops polling at session end and cancels what is still pending', async ($, on) => {
    const w = new World(on, { status: 'busy' })
    w.request(ID_A)
    await start($)
    await w.clock.advance(500)
    await $.session.end({ reason: 'prompt_input_exit', sessionId: 'sess-1', resume: { id: 'sess-1' } })
    expect(w.response(ID_A)).toMatchObject({ result: 'cancelled', reason: 'session_ended' })
    w.request(ID_B, { action: 'status', expiresAt: START + 60_000 })
    const listed = w.lists.length
    await w.clock.advance(5_000)
    expect(w.lists.length).toBe(listed)
    expect(w.ack(ID_B)).toBeUndefined()
  })

  test('keeps polling after /clear, which ends the conversation but not the process', async ($, on) => {
    const w = new World(on)
    await start($)
    await $.session.end({ reason: 'clear', sessionId: 'sess-1', resume: { id: 'sess-1' } })
    w.request(ID_A, { action: 'status', expiresAt: START + 3_000 })
    await w.clock.advance(500)
    expect(w.response(ID_A)?.result).toBe('ok')
  })

  test('stays inactive below Claude Code 2.1.287 and registers only the status command', async ($, on) => {
    const w = new World(on, { version: '2.1.286' })
    w.request(ID_A)
    await start($)
    await w.clock.advance(2_000)
    expect(w.registered).toEqual(['agentctl-rc'])
    expect(w.argv).toEqual([])
    expect(w.ack(ID_A)).toBeUndefined()
    expect(w.logs.filter((l) => l.includes('inactive')).length).toBe(1)
    expect(await command($)).toContain('inactive: Claude Code 2.1.286 is older than 2.1.287')
    expect(await command($, 'reconnect')).toContain('inactive')
  })

  test('stays inactive when the stat probe does not answer the macOS shape', async ($, on) => {
    const w = new World(on)
    // GNU stat reads -f as "file system" and prints another shape entirely.
    w.statOverride = (path) => (path === RECORD ? '  File: "/Users/tester/.claude/sessions/4242.json"\n    ID: 0 Namelen: 255 Type: apfs\n' : undefined)
    w.request(ID_A)
    await start($)
    await w.clock.advance(2_000)
    expect(w.ack(ID_A)).toBeUndefined()
    expect(await command($)).toContain('inactive: the host stat probe did not answer the macOS shape')
  })

  test('stays inactive when the registry record names another process', async ($, on) => {
    const w = new World(on)
    w.setRecord({ messagingSocketPath: '/private/tmp/user/501/cc-socks/9999.sock' })
    await start($)
    expect(await command($)).toContain('inactive: the session registry record does not match this process')
  })
})

describe('/agentctl-rc', () => {
  test('prints presence, generation, surfaces, version and provenance, never the bridge id', async ($, on) => {
    const w = new World(on, { bridge: SEEDED, env: { CLAUDE_CONFIG_DIR: '/Users/tester/.claude' } })
    await start($)
    const text = await command($)
    expect(text).toContain('Remote Control bridge: present, generation 1')
    expect(text).toContain('Surfaces: terminal')
    expect(text).toContain('Claude Code: 2.1.296')
    expect(text).toContain('remote-control command: listed')
    expect(text).toContain('CLAUDE_CONFIG_DIR                "/Users/tester/.claude"')
    expect(text).toContain('CLAUDE_SECURESTORAGE_CONFIG_DIR  unset')
    expect(text).toContain('first-party authorization        yes')
    expect(text).not.toContain(SEEDED)
    expect(text).not.toContain('https://')
  })

  test('reconnect while connected answers requested, then toasts already connected without running', async ($, on) => {
    const w = new World(on, { bridge: SEEDED })
    await start($)
    expect(await command($, 'reconnect')).toBe('requested')
    await w.clock.advance(500)
    expect(w.toasts).toEqual(['Remote Control is already connected; nothing was run.'])
    expect(w.runs).toEqual([])
  })

  test('reconnect where remote-control is not listed toasts unavailable', async ($, on) => {
    const w = new World(on)
    w.isListed = false
    await start($)
    expect(await command($, 'reconnect')).toBe('requested')
    await w.clock.advance(500)
    expect(w.toasts).toEqual(['Remote Control is unavailable in this session (command_not_listed); nothing was run.'])
    expect(w.runs).toEqual([])
  })

  test('reconnect during a turn waits for it, then runs and toasts reconnected', async ($, on) => {
    const w = new World(on)
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    await start($)
    await turnStart($)
    expect(await command($, 'reconnect')).toBe('requested')
    await w.clock.advance(1_500)
    expect(w.runs).toEqual([])
    expect(w.toasts).toEqual([])
    await turnComplete($)
    await w.clock.advance(1_000)
    expect(w.runs).toEqual(['remote-control'])
    expect(w.toasts).toEqual(['Remote Control reconnected.'])
  })

  test('reconnect when idle runs once and toasts reconnected', async ($, on) => {
    const w = new World(on)
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    await start($)
    expect(await command($, 'reconnect')).toBe('requested')
    expect(await command($, 'reconnect')).toBe('A reconnect is already requested.')
    await w.clock.advance(1_000)
    expect(w.runs).toEqual(['remote-control'])
    expect(w.toasts).toEqual(['Remote Control reconnected.'])
    expect(await command($)).toContain('present, generation 1')
    expect(w.writes).toEqual([])
  })
})

describe('redaction', () => {
  test('never writes, shows or logs a seeded bridge id or URL', async ($, on) => {
    const w = new World(on, { bridge: SEEDED })
    w.request(ID_A, { action: 'status', expiresAt: START + 3_000 })
    w.request(ID_B)
    await start($)
    await w.clock.advance(500)
    w.setRecord({ bridgeSessionId: '' })
    w.onRemoteControl = () => w.setRecord({ bridgeSessionId: NEXT_BRIDGE })
    w.request(ID_C)
    await w.clock.advance(1_000)
    expect(w.response(ID_C)?.result).toBe('reconnected')
    const everything = [w.outputs(), await command($)].join('\n')
    expect(everything).not.toContain(SEEDED)
    expect(everything).not.toContain(NEXT_BRIDGE)
    expect(everything).not.toContain('https://')
  })

  test('redact replaces Remote Control URLs and bridge ids', () => {
    expect(redact(`Open https://claude.ai/code/${SEEDED} or ${SEEDED}`)).toBe('Open [URL redacted] or [bridge id redacted]')
    expect(redact('already connected')).toBe('already connected')
  })

  test('meetsFloor compares the three release components', () => {
    expect(meetsFloor('2.1.287')).toBe(true)
    expect(meetsFloor('2.1.296-dev')).toBe(true)
    expect(meetsFloor('2.2.0')).toBe(true)
    expect(meetsFloor('3.0.0')).toBe(true)
    expect(meetsFloor('2.1.286')).toBe(false)
    expect(meetsFloor('2.0.999')).toBe(false)
    expect(meetsFloor(undefined)).toBe(false)
    expect(meetsFloor('nightly')).toBe(false)
  })
})
