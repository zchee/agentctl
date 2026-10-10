// Copyright 2026 The agentctl Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

// The agentctl half of `--restart-remote-control`.
//
// agentctl and the Remote Control mod inside each running Claude Code
// session talk through files under that session's own configuration home:
// agentctl writes a request, the mod writes an acknowledgement and then a
// response. Before the swap, a status request asks each bridged session
// which keychain item it reads, so only the sessions this swap concerns
// are followed up. After the swap, agentctl waits for each such session's
// old bridge to disappear — Claude Code drops it by itself once the
// credential changes — and only then asks the mod to start a new one.
// Nothing here changes the swap's own outcome or exit status.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

const (
	// useRCContractVersion is the transport contract both halves speak.
	useRCContractVersion = 1
	// useRCSweepAge is how old a file agentctl left behind must be before
	// a later run removes it.
	useRCSweepAge = 24 * time.Hour
	// useRCPreflightBudget bounds the whole status round, inside the swap
	// deadline and before the keychain read.
	useRCPreflightBudget = 5 * time.Second
	// useRCStatusLifetime is a status request's lifetime, and how long
	// any request waits for its acknowledgement.
	useRCStatusLifetime = 3 * time.Second
	// useRCReconnectLifetime is a reconnect request's lifetime: long
	// enough for a busy session to finish its turn.
	useRCReconnectLifetime = 75 * time.Second
	// useRCFollowUpBudget bounds the whole follow-up after the swap.
	useRCFollowUpBudget = 90 * time.Second
	// useRCPoll is how often an answer or the registry is re-read.
	useRCPoll = 250 * time.Millisecond
)

const (
	useRCActionStatus    = "status"
	useRCActionReconnect = "reconnect"
)

// useRCTiming holds the follow-up's waits, so a test build can shorten the
// long ones without changing what they mean.
type useRCTiming struct {
	preflight, status, drop, reconnect, followUp, poll time.Duration
}

func useRCDefaultTiming() useRCTiming {
	return useRCTiming{
		preflight: useRCPreflightBudget,
		status:    useRCStatusLifetime,
		drop:      claude.RemoteControlDropWaitSeconds * time.Second,
		reconnect: useRCReconnectLifetime,
		followUp:  useRCFollowUpBudget,
		poll:      useRCPoll,
	}
}

var (
	errUseRCSymlink = errors.New("the Remote Control transport directory is a symbolic link")
	errUseRCNotDir  = errors.New("the Remote Control transport path is not a directory")
	errUseRCOwner   = errors.New("the Remote Control transport directory belongs to another user")
	errUseRCMode    = errors.New("the Remote Control transport directory is open to other users")
)

// useRCTransportFile matches every file name agentctl itself writes or
// waits for, so the sweep never touches anything else.
var useRCTransportFile = regexp.MustCompile(`^[0-9a-f]{32}\.(request\.json(\.tmp)?|ack\.json|response\.json)$`)

// useRCTransport is the request/response channel under one configuration
// home: <configHome>/agentctl/remote-control/<pid>/.
type useRCTransport struct {
	root   string
	uid    int
	timing useRCTiming
}

func newUseRCTransport(env *claude.EnvView, timing useRCTiming) useRCTransport {
	home := filepath.Dir(claude.SessionsDir(env))
	return useRCTransport{root: filepath.Join(home, "agentctl", "remote-control"), uid: os.Getuid(), timing: timing}
}

// open creates, or checks, the chain of directories down to one session's
// directory, and sweeps that directory of agentctl's own stale files.
func (t useRCTransport) open(pid uint32) (string, error) {
	dir := filepath.Join(t.root, strconv.FormatUint(uint64(pid), 10))
	for _, path := range []string{filepath.Dir(t.root), t.root, dir} {
		if err := t.ensureDir(path); err != nil {
			return "", err
		}
	}
	t.sweep(dir, time.Now())
	return dir, nil
}

// ensureDir makes dir with mode 0700 when it is missing, then accepts it
// only as a real directory owned by this user and closed to everyone else:
// a request planted by another local user could otherwise start a bridge.
func (t useRCTransport) ensureDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return errUseRCSymlink
	}
	if !info.IsDir() {
		return errUseRCNotDir
	}
	if !t.ownedByMe(info) {
		return errUseRCOwner
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errUseRCMode
	}
	return nil
}

func (t useRCTransport) ownedByMe(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == t.uid
}

// sweep removes agentctl's own request and answer files older than a day:
// a run that was killed, or an answer that arrived after agentctl gave up,
// leaves them behind, and the mod has no way to remove a file.
func (t useRCTransport) sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !useRCTransportFile.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !t.ownedByMe(info) || now.Sub(info.ModTime()) < useRCSweepAge {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("a stale Remote Control transport file could not be removed", slog.Any("error", err))
		}
	}
}

type useRCSubject struct {
	Service string `json:"service"`
}

type useRCRequest struct {
	V         int          `json:"v"`
	ID        string       `json:"id"`
	Action    string       `json:"action"`
	IssuedAt  int64        `json:"issuedAt"`
	ExpiresAt int64        `json:"expiresAt"`
	Subject   useRCSubject `json:"subject"`
}

// send writes one request. It is written under a temporary name with mode
// 0600 and renamed into place, so the mod never reads a partial request.
func (t useRCTransport) send(dir, action, service string, lifetime time.Duration) (string, time.Time, error) {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	issued := time.Now()
	body, err := json.Marshal(useRCRequest{V: useRCContractVersion, ID: id, Action: action, IssuedAt: issued.UnixMilli(), ExpiresAt: issued.Add(lifetime).UnixMilli(), Subject: useRCSubject{Service: service}})
	if err != nil {
		return "", issued, err
	}
	temporary := filepath.Join(dir, id+".request.json.tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", issued, err
	}
	_, err = file.Write(body)
	if err == nil {
		// The creation mode passes through the umask; the mod accepts
		// exactly 0600, so the mode is set explicitly.
		err = file.Chmod(0o600)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, filepath.Join(dir, id+".request.json"))
	}
	if err != nil {
		_ = os.Remove(temporary)
		return "", issued, err
	}
	return id, issued, nil
}

// clean removes the request and any answer to it. A late answer written
// after this is left for the sweep.
func (useRCTransport) clean(dir, id string) {
	for _, suffix := range []string{".request.json.tmp", ".request.json", ".ack.json", ".response.json"} {
		if err := os.Remove(filepath.Join(dir, id+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("a Remote Control transport file could not be removed", slog.Any("error", err))
		}
	}
}

// read returns a file the mod wrote, accepting only a regular file this
// user owns, through the same bounded reader the registry uses. Anything
// else reads as "not yet".
func (t useRCTransport) read(path string) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !t.ownedByMe(info) {
		return nil, false
	}
	read, err := secret.ReadFile(path, useRegistryRecordLimit)
	if err != nil || !read.Present {
		return nil, false
	}
	return read.Bytes, true
}

type useRCAck struct {
	V          *int     `json:"v"`
	ID         *string  `json:"id"`
	Action     *string  `json:"action"`
	State      *string  `json:"state"`
	Reason     *string  `json:"reason"`
	AnsweredAt *float64 `json:"answeredAt"`
}

type useRCBridge struct {
	Present    *bool    `json:"present"`
	Generation *float64 `json:"generation"`
}

type useRCResponse struct {
	V          *int                            `json:"v"`
	ID         *string                         `json:"id"`
	Action     *string                         `json:"action"`
	Result     *string                         `json:"result"`
	AnsweredAt *float64                        `json:"answeredAt"`
	Bridge     *useRCBridge                    `json:"bridge"`
	Surfaces   []string                        `json:"surfaces"`
	Version    *string                         `json:"version"`
	Listed     *bool                           `json:"remoteControlListed"`
	Provenance *claude.RemoteControlProvenance `json:"provenance"`
	Reason     *string                         `json:"reason"`
}

// matches reports whether the envelope belongs to this request.
func useRCMatches(v *int, id, action *string, wantID, wantAction string) bool {
	return v != nil && *v == useRCContractVersion && id != nil && *id == wantID && action != nil && *action == wantAction
}

func (t useRCTransport) readAck(dir, id, action string) (useRCAck, bool) {
	body, ok := t.read(filepath.Join(dir, id+".ack.json"))
	if !ok {
		return useRCAck{}, false
	}
	var ack useRCAck
	if json.Unmarshal(body, &ack) != nil || !useRCMatches(ack.V, ack.ID, ack.Action, id, action) || ack.State == nil || ack.AnsweredAt == nil {
		return useRCAck{}, false
	}
	if *ack.State != "accepted" && *ack.State != "rejected" {
		return useRCAck{}, false
	}
	return ack, true
}

// useRCResults are the result words each action may end in.
var useRCResults = map[string][]string{
	useRCActionStatus:    {"ok"},
	useRCActionReconnect: {claude.RemoteControlReconnected, claude.RemoteControlAlreadyConnected, claude.RemoteControlUnavailable, claude.RemoteControlNotConfirmed, claude.RemoteControlExpired, claude.RemoteControlCancelled},
}

func (t useRCTransport) readResponse(dir, id, action string) (useRCResponse, bool) {
	body, ok := t.read(filepath.Join(dir, id+".response.json"))
	if !ok {
		return useRCResponse{}, false
	}
	var response useRCResponse
	if json.Unmarshal(body, &response) != nil || !useRCMatches(response.V, response.ID, response.Action, id, action) || response.Result == nil || response.AnsweredAt == nil || response.Bridge == nil || response.Bridge.Present == nil {
		return useRCResponse{}, false
	}
	known := false
	for _, result := range useRCResults[action] {
		known = known || result == *response.Result
	}
	if !known || action == useRCActionStatus && (response.Provenance == nil || response.Version == nil || response.Listed == nil) {
		return useRCResponse{}, false
	}
	return response, true
}

type useRCAnswerKind uint8

const (
	useRCAnswered useRCAnswerKind = iota + 1
	useRCRejected
	useRCSilent
	useRCNoResponse
	useRCInterrupted
)

type useRCAnswer struct {
	kind     useRCAnswerKind
	reason   string
	response useRCResponse
}

// wait polls for the answer to one request. A response is taken even when
// its acknowledgement never appeared; a malformed, truncated or foreign
// file reads as "not yet", so only the deadline turns silence into failure.
func (t useRCTransport) wait(ctx context.Context, dir, id, action string, issued time.Time, lifetime time.Duration) useRCAnswer {
	ackDeadline := issued.Add(min(t.timing.status, lifetime))
	deadline := issued.Add(lifetime)
	ticker := time.NewTicker(t.timing.poll)
	defer ticker.Stop()
	acked := false
	for {
		if response, ok := t.readResponse(dir, id, action); ok {
			return useRCAnswer{kind: useRCAnswered, response: response}
		}
		if !acked {
			if ack, ok := t.readAck(dir, id, action); ok {
				if *ack.State == "rejected" {
					reason := ""
					if ack.Reason != nil {
						reason = *ack.Reason
					}
					return useRCAnswer{kind: useRCRejected, reason: reason}
				}
				acked = true
			}
		}
		now := time.Now()
		switch {
		case !acked && now.After(ackDeadline):
			return useRCAnswer{kind: useRCSilent}
		case now.After(deadline):
			return useRCAnswer{kind: useRCNoResponse}
		}
		select {
		case <-ctx.Done():
			return useRCAnswer{kind: useRCInterrupted}
		case <-ticker.C:
		}
	}
}

// exchange sends one request, waits for its answer and removes the files.
func (t useRCTransport) exchange(ctx context.Context, dir, action, service string, lifetime time.Duration) useRCAnswer {
	id, issued, err := t.send(dir, action, service, lifetime)
	if err != nil {
		slog.DebugContext(ctx, "a Remote Control request could not be written", slog.String("action", action), slog.Any("error", err))
		return useRCAnswer{kind: useRCSilent}
	}
	defer t.clean(dir, id)
	return t.wait(ctx, dir, id, action, issued, lifetime)
}

// useRCClass is what the preflight learned about one bridged session.
type useRCClass uint8

const (
	useRCEligible useRCClass = iota + 1
	useRCProvenanceSkipped
	useRCUnreachable
	useRCVersionRejected
	useRCMetadataRejected
	useRCUnavailable
)

// useRCRejection maps a rejected acknowledgement's reason to its class.
func useRCRejection(reason string) useRCClass {
	switch reason {
	case "metadata":
		return useRCMetadataRejected
	case "version":
		return useRCVersionRejected
	default:
		return useRCUnreachable
	}
}

func (c useRCClass) count(counts *claude.RemoteControlCounts) {
	switch c {
	case useRCEligible:
		counts.Eligible++
	case useRCProvenanceSkipped:
		counts.ProvenanceSkipped++
	case useRCUnreachable:
		counts.Unreachable++
	case useRCVersionRejected:
		counts.VersionRejected++
	case useRCMetadataRejected:
		counts.MetadataRejected++
	case useRCUnavailable:
		counts.Unavailable++
	}
}

// useRCSession is one eligible session, as the preflight saw it.
type useRCSession struct {
	path   string
	dir    string
	record claude.RegistryRecord
}

// useRemoteControl is the preflight's result, carried to the follow-up.
type useRemoteControl struct {
	transport useRCTransport
	service   string
	counts    claude.RemoteControlCounts
	eligible  []useRCSession
}

// useRemoteControlSessions spells "N Claude Code session(s)".
func useRemoteControlSessions(n int) string {
	if n == 1 {
		return "1 Claude Code session"
	}
	return fmt.Sprintf("%d Claude Code sessions", n)
}

// useUnreadableRegistryNote is the note for a registry agentctl could not
// list. It states only what is observable: it never says what starting
// Remote Control again does to a conversation claude.ai keeps.
func useUnreadableRegistryNote(kind string) string {
	return fmt.Sprintf("agentctl could not read Claude Code's session registry (%s), so it cannot say whether a running session has Remote Control on. In a session that does and reads this store, Claude Code stops Remote Control after the swap (now, or on its next account check), and `/remote-control` there starts it again", kind)
}

// remoteControlPreflight classes every bridged session under one short
// deadline, before any keychain read. A session that does not answer, or a
// registry that cannot be read, refuses the swap: then nothing can say
// whether a session would start Remote Control again.
func (swap useLiveSwap) remoteControlPreflight(ctx context.Context, hints *useSessionHints, service string) (*useRemoteControl, *useReport) {
	started := time.Now()
	timing := useRemoteControlTiming()
	rc := &useRemoteControl{transport: newUseRCTransport(swap.env, timing), service: service}
	if hints.unreadable != "" {
		report := useRefused(claude.SwapRefusal{Kind: claude.SwapRemoteControlUnreachable}, service, "agentctl could not read Claude Code's session registry, so it cannot tell which running sessions to ask to start Remote Control again; nothing was written. Run this again without `--restart-remote-control`, and start Remote Control by hand where it stops")
		return rc, report
	}
	ctx, cancel := context.WithTimeout(ctx, timing.preflight)
	defer cancel()
	classes := make([]useRCClass, len(hints.sessions))
	dirs := make([]string, len(hints.sessions))
	var wg sync.WaitGroup
	for i, session := range hints.sessions {
		wg.Go(func() { classes[i], dirs[i] = rc.classify(ctx, session) })
	}
	wg.Wait()
	var names []string
	for i, class := range classes {
		class.count(&rc.counts)
		if class == useRCEligible {
			rc.eligible = append(rc.eligible, useRCSession{path: hints.sessions[i].path, dir: dirs[i], record: hints.sessions[i].record})
			continue
		}
		names = append(names, hints.sessions[i].name)
	}
	hints.names, hints.handled = names, len(rc.eligible)
	slog.DebugContext(ctx, "remote control preflight", slog.String("stage", "preflight"), slog.Any("counts", rc.counts), slog.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	if n := rc.counts.Unreachable; n != 0 {
		them := "them"
		if n == 1 {
			them = "it"
		}
		return rc, useRefused(claude.SwapRefusal{Kind: claude.SwapRemoteControlUnreachable}, service, fmt.Sprintf("%s with Remote Control on did not answer agentctl's request (the agentctl Remote Control mod is not installed there, or did not answer within %d s), so nothing was written. Install the mod in %s, or run this again without `--restart-remote-control` and start Remote Control by hand where it stops", useRemoteControlSessions(n), int(useRCPreflightBudget/time.Second), them))
	}
	return rc, nil
}

// classify asks one session for its status. The registry's version is
// checked first, so a session too old for the mod gets no request at all.
func (rc *useRemoteControl) classify(ctx context.Context, session useBridgedSession) (useRCClass, string) {
	version, ok := claude.ParseVersion(session.record.Version)
	if !session.decoded || !ok || version.Less(claude.RemoteControlMinVersion) {
		return useRCVersionRejected, ""
	}
	dir, err := rc.transport.open(session.pid)
	if err != nil {
		slog.DebugContext(ctx, "the Remote Control transport directory was refused", slog.Any("error", err))
		return useRCUnreachable, ""
	}
	answer := rc.transport.exchange(ctx, dir, useRCActionStatus, rc.service, rc.transport.timing.status)
	switch answer.kind {
	case useRCAnswered:
		if !*answer.response.Listed {
			// The command is not offered there, so no request could start it.
			return useRCUnavailable, dir
		}
		if answer.response.Provenance.Concerns(rc.service) {
			return useRCEligible, dir
		}
		return useRCProvenanceSkipped, dir
	case useRCRejected:
		return useRCRejection(answer.reason), dir
	default:
		return useRCUnreachable, dir
	}
}

// useRCOutcome is what the follow-up observed for one eligible session.
type useRCOutcome struct {
	dropped    bool
	notDropped bool
	restored   bool
	class      useRCClass
	// result is the reconnect result word, or empty when there is none.
	result string
	// counted reports whether result is to be counted.
	counted bool
}

// followUpRemoteControl runs after the swap released every lock and before
// the report is printed. It never changes the swap's outcome or exit
// status; it adds the counts, and warnings derived from them.
func (p SessionProcess) followUpRemoteControl(ctx context.Context, opts cli.ClaudeUseOptions, report *useReport) {
	if !opts.RestartRemoteControl {
		return
	}
	rc := report.rc
	if rc == nil {
		report.remoteControl = &claude.RemoteControlCounts{}
		return
	}
	started := time.Now()
	configApplied := report.config != nil && report.config.NotUpdated() == nil
	action := claude.RemoteControlActionFor(report.outcome.Kind, configApplied)
	counts := rc.counts
	followCtx, cancel := context.WithTimeout(ctx, rc.transport.timing.followUp)
	defer cancel()
	outcomes := make([]useRCOutcome, len(rc.eligible))
	var wg sync.WaitGroup
	for i, session := range rc.eligible {
		switch action {
		case claude.RemoteControlReconnect:
			wg.Go(func() { outcomes[i] = rc.reconnect(followCtx, session) })
		case claude.RemoteControlKeep:
			wg.Go(func() { outcomes[i] = rc.keep(session) })
		}
	}
	wg.Wait()
	var results []string
	for _, outcome := range outcomes {
		if outcome.dropped {
			counts.Dropped++
		}
		if outcome.notDropped {
			counts.NotDropped++
		}
		if outcome.restored {
			counts.Restored++
		}
		outcome.class.count(&counts)
		if outcome.counted {
			counts.CountResult(outcome.result)
			results = append(results, outcome.result)
		}
	}
	report.remoteControl = &counts
	slog.DebugContext(ctx, "remote control follow-up", slog.String("stage", "follow-up"), slog.Any("counts", counts), slog.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	if ctx.Err() != nil && action == claude.RemoteControlReconnect {
		note := fmt.Sprintf("the Remote Control follow-up was interrupted: %d reconnected, %d already connected, %d unavailable, %d not confirmed, %d not dropped", counts.Reconnected, counts.AlreadyConnected, counts.Unavailable, counts.NotConfirmed, counts.NotDropped)
		if err := tell(p.Err, "note: "+note); err != nil {
			slog.ErrorContext(ctx, "the Remote Control note could not be written", slog.Any("error", err))
		}
	}
	for _, warning := range claude.RemoteControlWarnings(counts, results, action) {
		report.warnings = append(report.warnings, warning)
		if err := tell(p.Err, "warning: "+warning); err != nil {
			slog.ErrorContext(ctx, "the Remote Control warning could not be written", slog.Any("error", err))
		}
	}
}

// current re-reads the session's registry record. It reports false when
// the record is gone, or now names another process or session: the
// session agentctl asked about is not the one there.
func (rc *useRemoteControl) current(session useRCSession) (claude.RegistryRecord, bool, bool) {
	read, err := secret.ReadFile(session.path, useRegistryRecordLimit)
	if err != nil || !read.Present {
		return claude.RegistryRecord{}, false, true
	}
	record, err := claude.DecodeRegistryRecord(read.Bytes)
	if err != nil {
		// A record caught mid-write reads again on the next poll.
		return claude.RegistryRecord{}, false, false
	}
	if record.PID != session.record.PID || record.SessionID != session.record.SessionID {
		return claude.RegistryRecord{}, false, true
	}
	return record, true, false
}

// keep counts a session whose bridge is still there after a pass that
// changed no credential.
func (rc *useRemoteControl) keep(session useRCSession) useRCOutcome {
	record, ok, _ := rc.current(session)
	return useRCOutcome{restored: ok && record.Bridge.Present()}
}

// reconnect waits for the old bridge to disappear, then asks the mod for a
// new one. A connected bridge is never asked to reconnect: the mod would
// have to open Claude Code's interactive dialog to act on it.
func (rc *useRemoteControl) reconnect(ctx context.Context, session useRCSession) useRCOutcome {
	notConfirmed := useRCOutcome{counted: true}
	deadline := time.Now().Add(rc.transport.timing.drop)
	ticker := time.NewTicker(rc.transport.timing.poll)
	defer ticker.Stop()
	for {
		record, ok, gone := rc.current(session)
		if gone {
			return notConfirmed
		}
		if ok {
			if !record.Bridge.Present() {
				break
			}
			if session.record.Bridge.Replaced(record.Bridge) {
				return useRCOutcome{result: claude.RemoteControlAlreadyConnected, counted: true}
			}
		}
		if time.Now().After(deadline) {
			return useRCOutcome{notDropped: true}
		}
		select {
		case <-ctx.Done():
			return notConfirmed
		case <-ticker.C:
		}
	}
	answer := rc.transport.exchange(ctx, session.dir, useRCActionReconnect, rc.service, rc.transport.timing.reconnect)
	outcome := useRCOutcome{dropped: true}
	switch answer.kind {
	case useRCAnswered:
		outcome.result, outcome.counted = *answer.response.Result, true
	case useRCRejected:
		outcome.class = useRCRejection(answer.reason)
	default:
		outcome.counted = true
	}
	return outcome
}

// useRemoteControlPlatformRefusal is the refusal for a platform without a
// live swap, decided before anything is read.
func useRemoteControlPlatformRefusal() *useReport {
	report := useRefused(claude.SwapRefusal{Kind: claude.SwapRemoteControlUnsupportedPlatform}, "", "restarting Remote Control after a live swap is supported only on macOS, where the live swap itself runs; nothing was read or written")
	report.remoteControl = &claude.RemoteControlCounts{}
	return report
}
