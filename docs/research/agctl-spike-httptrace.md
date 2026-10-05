# Spike: Codex refresh-POST outcome classification with net/http + httptrace

- Written: 2026-10-05 18:18:40 JST (from `date`)
- Verdict: **GO**
- Evidence: real-socket test suite in `internal/provider/codex/refreshclass_test.go`
  run verbose with `go test -race -count=1 -timeout=30s -v ./internal/provider/codex/...`
  → `ok 1.285s`, "Fault evidence completed: 2026-10-05 18:17:42 JST" (from `date` in the
  same command). Full repository gates (gofmt/gofumpt/modernize/goimports-rereviser,
  `go build`, `go vet`, `golangci-lint run` 0 issues, `go test -race ./...` plain and
  `-tags agentctl_testing`) all passed, "Gates completed: 2026-10-05 18:13:09 JST".
  Toolchain: `go1.27.1 darwin/arm64`.
- Final verification after draining request bodies in response test handlers:
  nine package race-suite repetitions passed (2026-10-05 18:25:24 JST from the
  gate command). `go build ./...` and `go vet ./...` passed again; the subsequent
  repository-wide lint stopped only at another package's unformatted
  `internal/secret/secret_budget_test.go:30`. Owned-package lint reported zero
  issues, and both repository-wide race suites with `-timeout=60s` passed again;
  `go mod tidy -diff` was empty. That command finished at
  2026-10-05 18:26:28 JST (from its `date` output).
- Reference: the frozen Rust implementation at
  `/Users/zchee/rust/src/github.com/zchee/agctl` (commit
  `dbf6aeab84dfbe2465de496316d7bf6780940a60`), files
  `src/provider/codex/oauth.rs` (timeouts lines 96–115, `classify_response`
  350–378, `classify_error` 398–429), `src/provider/codex/oauth_tests.rs`, and
  `src/provider/codex/refresh.rs` (durable marker before the POST, lines ~391,
  703–730, 752–880). The HTTP client there is `ureq 3.4.1`.

## 1. Verdict and scope

GO. Every outcome class the reference derives from its HTTP client is reproducible
with `net/http` + `net/http/httptrace` on real sockets, with the same conservative
boundaries: only proof that the request never left the machine (`not sent`) permits
automatic resend; a received HTTP response is an envelope for the caller's
status/body policy, never permission to resend; everything else is `unknown`
(transport or TLS) and never resends.

Scope boundary: this spike implements the **transport envelope** only. The semantic
settlement of a received response (usable 2xx → applied, 401/grant errors →
permanent, 429 + `Retry-After` → rate-limited unknown, 5xx → server-error unknown,
truncated 2xx → ambiguous) is the caller's policy, exactly as `classify_response`
is separate from `classify_error` in the reference. `Outcome` retains the status,
cloned headers, complete body (or the body error), and the trace snapshot so that
policy can be written later without re-touching the transport.

Resend caveat carried over from the reference: a later attempt proven `not sent`
does not erase a durable unknown marker left by an earlier attempt.
`AllowsAutomaticResend` documents this; it judges one attempt only.

## 2. Fault → observed Go error chain → class → reference class

The socket-fault chains below come from the verbose run dated above (`%T` chains
and `errors.Is` probes logged by the tests). DNS and dial-timeout rows are
explicitly synthetic classifier tests, not observed network faults. Write-stall
tests assert timeout identity and trace milestones rather than logging a complete
chain. "class" is the Go `Outcome`; "reference" is the
`classify_error`/`classify_response` arm in `oauth.rs`.

| Fault (test) | Observed Go error chain | `errors.Is` / detection | Go class | Reference class |
|---|---|---|---|---|
| connection refused | `*url.Error → *net.OpError → *os.SyscallError → syscall.Errno` | `errors.Is(err, syscall.ECONNREFUSED)` | not sent | pre-send (connect) |
| host not found / resolve timeout | `*net.DNSError` (synthesized + unit-tested) | `errors.AsType[*net.DNSError]`, `IsNotFound`/`IsTimeout` | not sent | pre-send (resolve) |
| dial timeout (synthetic classifier test) | `*url.Error → *net.OpError{Op:"dial"} → *poll.DeadlineExceededError` | `Op=="dial" && Timeout()` | not sent | pre-send (connect) |
| TLS handshake timeout | `*url.Error → http.tlsHandshakeTimeoutError` | `trace.TLSStarted && !TLSOK && isTimeout(trace.TLSErr)` | not sent | pre-send (connect bound covers TLS) |
| no response headers (server silent) | `*url.Error → *http.timeoutError` | `errors.Is(err, context.DeadlineExceeded)` and `url.Error.Timeout()` | unknown/transport | sent, outcome unknown |
| FIN after complete request | `*url.Error → *errors.errorString` | `errors.Is(err, io.EOF)` | unknown/transport | sent, outcome unknown |
| RST after complete request | `*url.Error → *net.OpError → *os.SyscallError → syscall.Errno` | `errors.Is(err, syscall.ECONNRESET)` | unknown/transport | sent, outcome unknown |
| RST mid response body | `*net.OpError → *os.SyscallError → syscall.Errno` (BodyErr) | ECONNRESET | response (200 kept, body discarded) | received, body ambiguous |
| FIN mid response body | `*errors.errorString` (BodyErr) | `errors.Is(err, io.ErrUnexpectedEOF)` | response (200 kept) | received, body ambiguous |
| response body stall | `*net.OpError → *poll.DeadlineExceededError` (BodyErr) | `net.Error.Timeout()`; NOT `context.DeadlineExceeded` | response (200 kept) | received, body ambiguous |
| untrusted certificate | `*url.Error → *tls.CertificateVerificationError → x509.UnknownAuthorityError` | `errors.AsType[*tls.CertificateVerificationError]` | unknown/TLS | unknown (TLS), per reference policy |
| fatal handshake alert | `*url.Error → *tls.permanentError → *net.OpError → tls.alert` | `*net.OpError` with `Op=="remote error"` while `TLSStarted` | unknown/TLS | unknown (TLS) |
| corrupt record version after handshake | `*url.Error → tls.RecordHeaderError` | `errors.AsType[tls.RecordHeaderError]` | unknown/TLS | unknown (TLS) |
| corrupt ciphertext after handshake | `*url.Error → *tls.permanentError → *tls.permanentError → *net.OpError → tls.alert` | `Op=="local error"` while `TLSStarted` | unknown/TLS | unknown (TLS) |
| request header write stall | `*url.Error` wrapping transport-broken → timeout `*net.OpError` | nested `errors.AsType[*net.OpError]` timeout; `WroteHeaders==false` | unknown/transport | sent, outcome unknown |
| request body write stall | timeout asserted; complete chain not logged | timeout with `WroteHeaders==true` | unknown/transport | sent, outcome unknown |
| cancelled before send | `ctx.Err()` checked before `Do` | — | not sent | pre-send |
| 200/401/204/429+`Retry-After`/503/307 | none | status + cloned headers + complete body retained | response | received → later policy |

Deliberate conservatisms, identical to the reference:

- An unreachable-host dial error (`EHOSTUNREACH`) stays **unknown**, not
  not-sent: the reference's pre-send allowlist does not include it.
- Certificate failure and handshake alerts stay **unknown/TLS** even when the
  failed handshake establishes that no HTTP request was sent; the reference
  classifies all TLS failures except the connect-phase timeout as unknown, and
  this implementation preserves that policy. Absence of `WroteHeaders` alone
  would not establish no send.
- A trace snapshot with a write milestone (`WroteHeaders`/`WroteRequest`/
  `GotFirstResponseByte`) defeats every pre-send proof, including a refused-looking
  errno: milestones outrank error shape.
- `Classify(nil, …)` and any unmatched error return the zero `OutcomeUnknown`:
  the vocabulary fails closed.

## 3. Exact deadline mapping

The reference (`oauth.rs:96–115`) defines six per-phase bounds and **no** overall
request deadline; the six sum to 19s. The Go mapping, verified by
`TestDefaultPhaseTimeouts` and the write/stall tests:

| Phase | Bound | Go mechanism |
|---|---:|---|
| name resolution | 2s | `context.WithTimeout` around `net.DefaultResolver.LookupIPAddr` inside the custom `DialContext` |
| TCP connect + TLS handshake | 3s | one absolute deadline: `net.Dialer{Deadline}` across all resolved addresses, then `conn.SetDeadline(deadline)` so the TLS handshake inherits the remainder; `Transport.TLSHandshakeTimeout` set to the same bound as a backstop; `GotConn` clears it |
| request line + headers | 2s | `GotConn` arms `SetWriteDeadline(now+bound)` |
| request body | 2s | the body reader's first `Read` re-arms `SetWriteDeadline(now+bound)` once (never per write) |
| complete response headers | 8s | `Transport.ResponseHeaderTimeout` (covers all headers after the request write, not first byte only) |
| complete response body | 2s | `SetReadDeadline(now+bound)` before `io.ReadAll(io.LimitReader(body, cap+1))` |

Supporting facts that make the mapping exact:

- `httptrace.WroteHeaders` fires before buffered headers necessarily reach the
  socket, and `net/http.newTransferWriter` flushes headers before reading a body
  it cannot identify as in-memory. Wrapping `*bytes.Reader` in an opaque reader
  (no `WriterTo`) forces the header flush first, which is what makes the separate
  header and body write budgets real (verified in `TestPostWriteDeadlines` with
  1 KiB socket buffers; each stall trips its own 150ms bound well inside a 3s
  outer context, with `WroteHeaders` false for the header stall and true for the
  body stall).
- Single-send is structural, not hoped for: `DisableKeepAlives: true` removes the
  reused-connection retry path (`net/http` retries only idempotent/replayable
  requests on reused connections), `GetBody` is nil, no idempotency header, and
  `Protocols.SetHTTP1(true)` pins HTTP/1.1. Raw-socket fault cases assert one
  accepted connection; HTTP response cases assert exactly one handler call per
  explicit send, and certificate failure asserts zero handler calls.
- `CheckRedirect` returns `http.ErrUseLastResponse`: a 307 comes back as a
  response envelope, never re-POSTed (asserted).
- Response body cap 256 KiB mirrors the reference; an over-cap or incomplete body
  is cleared (`clear` + nil) and only `BodyErr` survives, while the status and
  headers remain for policy (an unreadable 200 is distinguishable from a 401/429).
- `resp.Body == http.NoBody` (204, empty 401) must skip the read-deadline arm:
  the transport has already closed the connection, and `SetReadDeadline` would
  fail with "use of closed network connection".

## 4. Pitfalls found (and encoded in code/tests)

1. `(*url.Error).Timeout()` can be false while a deeper `*net.OpError` is a
   timeout: the HTTP/1.x "transport connection broken" wrapper is a plain
   `fmt.wrapError` that does not implement `net.Error`, and
   `errors.AsType[net.Error]` stops at the outer `url.Error`. `isTimeout` probes
   `*url.Error`, then `*net.OpError`, then `context.DeadlineExceeded`/`net.Error`.
2. The body-stall read error is `*poll.DeadlineExceededError`:
   `errors.Is(err, context.DeadlineExceeded)` is **false**; only
   `net.Error.Timeout()` is true. Timeout detection cannot rely on `context`
   sentinels alone.
3. The TLS alert seam is private: `*tls.permanentError`, `tls.alert`, and
   `http.tlsHandshakeTimeoutError` are unexported. Post-handshake alerts are
   recognized through `*net.OpError` with the currently observed operation names
   `Op == "local error" | "remote error"` plus the `TLSHandshakeStart/Done` trace
   flags. This seam is version-sensitive: **re-validate against the fault matrix
   on every Go toolchain bump** (the matrix runs in CI, so a change fails loudly).
4. Trace callbacks are concurrent and can arrive late; absence of a write callback
   is never treated as proof of no send. Pre-send proofs come only from error
   identity (DNS, ECONNREFUSED, dial/TLS-handshake timeout) with no write
   milestone observed.
5. `MaxIdleConns: 0` means unlimited, not zero; `DisableKeepAlives` is what
   prevents reuse.

## 5. Distinguishability versus ureq 3.4.1

Go exposes more trace milestones than the reference uses:

- **Surplus**: `httptrace` gives per-request milestones (`ConnectStart`,
  `TLSHandshakeStart/Done`, `WroteHeaders`, `WroteRequest`,
  `GotFirstResponseByte`). These locate transport progress and handshake failure,
  but do not prove how many request bytes reached the server. The implementation
  records them in `Outcome.Trace` and deliberately does **not** upgrade TLS
  failures to not-sent, keeping class parity with the reference policy.
- **Parity**: every class the reference distinguishes (pre-send resolve/connect,
  TLS unknown, transport unknown, received response) maps one-to-one; no
  reference class collapsed, hence GO rather than any weaker classification.
- **Deficit (documented, not papered over)**: an HTTP/2 stream fault
  (RST_STREAM after request receipt) is untestable here because this client
  cannot negotiate HTTP/2 at all — `Protocols` is HTTP/1.1-only and
  `TestPostHTTP1ToHTTP2Server` proves an HTTP/2-enabled TLS server still serves
  the request as HTTP/1.1. The reference client is HTTP/1.1-only in the same way,
  so the fault is unreachable in both implementations rather than untested.

## 6. API for the implementation that follows

Package `internal/provider/codex` (`refreshclass.go`, ~380 lines; `doc.go`):

```go
const (
        TimeoutResolve      = 2 * time.Second
        TimeoutConnect      = 3 * time.Second // TCP + TLS together
        TimeoutSendRequest  = 2 * time.Second
        TimeoutSendBody     = 2 * time.Second
        TimeoutRecvResponse = 8 * time.Second
        TimeoutRecvBody     = 2 * time.Second
)

type PhaseTimeouts struct{ Resolve, Connect, SendRequest, SendBody, RecvResponse, RecvBody time.Duration }
func DefaultPhaseTimeouts() PhaseTimeouts

type OutcomeKind uint8 // OutcomeUnknown (zero, fail closed), OutcomeNotSent, OutcomeResponse
type UnknownClass uint8 // UnknownTransport (zero), UnknownTLS

type Observed struct { // trace snapshot; absence of a flag is not proof
        ConnectStarted, TLSStarted, TLSOK, WroteHeaders, WroteRequest, GotFirstResponseByte bool
        TLSErr, WroteRequestErr error
}

type Outcome struct {
        Kind OutcomeKind; Class UnknownClass; Reason string
        Status int; Header http.Header; Body []byte; BodyComplete bool; BodyErr error
        Err error; Trace Observed
}
func (o Outcome) AllowsAutomaticResend() bool // true only for OutcomeNotSent

func Classify(err error, trace Observed) Outcome
```

Private, so no command can reach the network without the durable-marker flow the
refresh implementation must add first (the marker is persisted **before** the
POST, per the reference and the inventory invariants):

```go
func newRefreshClient(bounds ...PhaseTimeouts) *refreshClient
func (c *refreshClient) post(ctx context.Context, endpoint, userAgent string, body []byte) Outcome
func dialRefresh(ctx context.Context, network, address string, phases PhaseTimeouts) (net.Conn, error)
```

Implementation obligations for the next step:

1. Persist the send marker durably, then call `post`; settle `OutcomeResponse`
   with the status/body policy before clearing the marker; leave the marker for
   every `OutcomeUnknown`.
2. Never resend automatically unless `AllowsAutomaticResend()` **and** no earlier
   attempt left an unsettled marker.
3. Keep the fault-matrix tests in CI; they are the tripwire for the private TLS
   error shapes on toolchain upgrades.
4. Do not log `Outcome.Body` or `Outcome.Header` (they can carry tokens); the
   package emits no logs itself.

Dependencies: standard library plus the already-present
`github.com/google/go-cmp v0.7.0` (tests only). No `x/net/http2`.
