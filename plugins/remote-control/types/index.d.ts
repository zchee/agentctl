// The session state agentctl-remote-control keeps in `$.state`, and the
// shapes of the files it exchanges with agentctl. Values in `$.state` are
// readable by every plugin in the session, so nothing here ever holds a
// bridge id, a URL or a credential.

/** What a request asks for. */
export type AgentctlRcAction = 'status' | 'reconnect'

/** Where a request came from: a request file agentctl wrote, or `/agentctl-rc reconnect`. */
export type AgentctlRcOrigin = 'file' | 'command'

/**
 * Where a request stands. `accepted` and `waiting_idle` still go through the
 * rule list; `running` means `remote-control` was issued and is only
 * observed; `final` holds a stored answer not yet delivered; `published`
 * is done.
 */
export type AgentctlRcPhase = 'accepted' | 'waiting_idle' | 'running' | 'final' | 'published'

/** The final answer of a request. `ok` answers `status`; the rest answer `reconnect`. */
export type AgentctlRcResult = 'ok' | 'reconnected' | 'already_connected' | 'unavailable' | 'not_confirmed' | 'expired' | 'cancelled'

/** Why a request file was rejected at admission. */
export type AgentctlRcRejectReason = 'name' | 'expired' | 'duplicate' | 'metadata' | 'action' | 'subject' | 'version' | 'busy'

/** An environment variable as the session sees it: whether it is set, and its spelling when it is. */
export type AgentctlRcEnvValue = { set: boolean; value: string }

/**
 * The environment the session's keychain service name is derived from.
 * Directory spellings and presence flags only, never a secret.
 */
export type AgentctlRcProvenance = {
  home: string
  configDir: AgentctlRcEnvValue
  secureStorageDir: AgentctlRcEnvValue
  oauthTokenSet: boolean
  apiKeySet: boolean
  baseUrlSet: boolean
  authorized: boolean
}

/**
 * The acknowledgement file `<id>.ack.json`, written once per admitted file.
 * A rejection carries an empty `action` when the body was not read or its
 * version is unknown.
 */
export type AgentctlRcAck = {
  v: 1
  id: string
  action: string
  state: 'accepted' | 'rejected'
  reason?: AgentctlRcRejectReason
  answeredAt: number
}

/** The response file `<id>.response.json`, written once per accepted request when final. */
export type AgentctlRcResponse = {
  v: 1
  id: string
  action: AgentctlRcAction
  result: AgentctlRcResult
  answeredAt: number
  bridge: { present: boolean; generation: number }
  surfaces: string[]
  version: string
  /** Whether `remote-control` is in the session's command list when the answer is written. */
  remoteControlListed: boolean
  provenance: AgentctlRcProvenance
  reason?: string
}

/** A delivery that must happen exactly once: the exact text, and whether it went out. */
export type AgentctlRcDelivery = { text: string; published: boolean }

/** One request in the table, keyed by its 32-hex id. */
export type AgentctlRcEntry = {
  phase: AgentctlRcPhase
  action: AgentctlRcAction
  origin: AgentctlRcOrigin
  admittedAt: number
  /**
   * When the request expires. For a rejection, how long the refusal is
   * remembered. No entry leaves the table before this time.
   */
  expiresAt: number
  /** When `remote-control` was issued. */
  runStartedAt?: number
  /** When an issued run stops being watched for a new bridge. */
  observeUntil?: number
  /** The session id the registry record showed when the run was issued. */
  runSessionId?: string
  /** The acknowledgement, for a request that came from a file. */
  ack?: AgentctlRcDelivery
  /** The final response; its text never changes once stored. */
  terminal?: { response: string; published: boolean }
}

declare module 'claude-code' {
  interface PluginState {
    'agentctl-remote-control': {
      /** Every request this session admitted: at most 1 024 entries, 64 of them not yet answered. */
      requests: Record<string, AgentctlRcEntry>
      /** How many distinct bridges this session has been seen to hold. */
      generation: number
      /** The uid `/usr/bin/id -u` answered at session start. */
      uid: number
    }
  }
}
