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

package testutil

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func init() { registerScriptCmd("rcmod", rcmodCmd) }

// rcmodScale is the default number of milliseconds that stand for one
// second of the follow-up's long waits in the scripts: the drop wait
// becomes 4.5 s and the reconnect lifetime 7.5 s.
const rcmodScale = "100"

// rcmodSession plays one running Claude Code session with the agentctl
// Remote Control mod: a live process, its registry record, and a
// goroutine that answers request files the way the scripted behaviour
// says.
type rcmodSession struct {
	name     string
	home     string
	pid      int
	record   string
	behavior map[string]string
	child    *exec.Cmd

	mu         sync.Mutex
	statuses   int
	reconnects int
	generation int
	violations []string
}

// rcmodWorld is one script's sessions, keyed by the fixture's home and
// the session's name, because every arm of a script builds a new home.
type rcmodWorld struct {
	mu          sync.Mutex
	sessions    map[string]*rcmodSession
	cancel      context.CancelFunc
	ctx         context.Context
	wg          sync.WaitGroup
	interrupted *exec.Cmd
}

var (
	rcmodWorldsMu sync.Mutex
	rcmodWorlds   = map[*testscript.TestScript]*rcmodWorld{}
)

func rcmodWorldOf(ts *testscript.TestScript) *rcmodWorld {
	rcmodWorldsMu.Lock()
	defer rcmodWorldsMu.Unlock()
	world, ok := rcmodWorlds[ts]
	if ok {
		return world
	}
	ctx, cancel := context.WithCancel(context.Background())
	world = &rcmodWorld{sessions: map[string]*rcmodSession{}, cancel: cancel, ctx: ctx}
	rcmodWorlds[ts] = world
	ts.Defer(func() {
		cancel()
		world.wg.Wait()
		for _, session := range world.sessions {
			_ = session.child.Process.Kill()
			_ = session.child.Wait()
		}
		rcmodWorldsMu.Lock()
		delete(rcmodWorlds, ts)
		rcmodWorldsMu.Unlock()
	})
	return world
}

var rcmodRequestName = regexp.MustCompile(`^([0-9a-f]{32})\.request\.json$`)

func rcmodCmd(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) == 0 {
		ts.Fatalf("usage: rcmod <seed|check|counts|interrupt|exited> ...")
	}
	world := rcmodWorldOf(ts)
	switch args[0] {
	case "seed":
		if len(args) < 2 {
			ts.Fatalf("usage: rcmod seed <name> [key=value...]")
		}
		world.seed(ts, args[1], args[2:])
	case "check":
		if len(args) != 4 {
			ts.Fatalf("usage: rcmod check <name> <status requests> <reconnect requests>")
		}
		world.check(ts, args[1], args[2], args[3])
	case "counts":
		rcmodCounts(ts, args[1:])
	case "interrupt":
		world.interrupt(ts)
	case "exited":
		if len(args) != 2 || world.interrupted == nil || world.interrupted.ProcessState == nil {
			ts.Fatalf("usage: rcmod exited <code>, after rcmod interrupt and wait")
		}
		if got := strconv.Itoa(world.interrupted.ProcessState.ExitCode()); got != args[1] {
			ts.Fatalf("the interrupted swap exited %s, want %s", got, args[1])
		}
	default:
		ts.Fatalf("unknown rcmod action %q", args[0])
	}
}

// seed starts a session. Keys: version (registry version, "none" for no
// member), status (ok, silent, truncated, reject:<reason>), provenance
// (live, explicit, isolated, gateway, token), drop (milliseconds after a
// status answer before the bridge vanishes, or never, replace, vanish),
// reconnect (reconnected, unavailable, expired, silent, unknown, wrongid,
// early, truncated, reject:<reason>), delay (milliseconds before a
// reconnect answer) and listed (false when `remote-control` is not among
// the session's commands).
func (world *rcmodWorld) seed(ts *testscript.TestScript, name string, args []string) {
	behavior := map[string]string{"version": "2.1.296", "status": "ok", "provenance": "live", "drop": "200", "reconnect": "reconnected", "delay": "0", "listed": "true"}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if _, known := behavior[key]; !ok || !known {
			ts.Fatalf("rcmod seed: unknown setting %q", arg)
		}
		behavior[key] = value
	}
	if ts.Getenv("AGENTCTL_REMOTE_CONTROL_TIME_SCALE") == "" {
		ts.Setenv("AGENTCTL_REMOTE_CONTROL_TIME_SCALE", rcmodScale)
	}
	child := exec.Command("/bin/sleep", "600")
	ts.Check(child.Start())
	home := ts.Getenv("HOME")
	session := &rcmodSession{name: name, home: home, pid: child.Process.Pid, behavior: behavior, child: child, generation: 1}
	session.record = filepath.Join(home, ".claude", "sessions", strconv.Itoa(session.pid)+".json")
	ts.Check(os.MkdirAll(filepath.Dir(session.record), 0o700))
	ts.Check(session.writeRecord("rcmod-bridge-" + name + "-1"))
	world.mu.Lock()
	if _, dup := world.sessions[home+"\x00"+name]; dup {
		world.mu.Unlock()
		_ = child.Process.Kill()
		ts.Fatalf("rcmod session %q seeded twice", name)
	}
	world.sessions[home+"\x00"+name] = session
	world.mu.Unlock()
	world.wg.Go(func() { session.serve(world.ctx, &world.wg) })
}

func (s *rcmodSession) writeRecord(bridge string) error {
	document := map[string]any{"pid": s.pid, "sessionId": "rcmod-local-" + s.name, "cwd": "rcmod-private-cwd", "kind": "interactive", "name": "rcmod-" + s.name, "status": "idle", "messagingSocketPath": filepath.Join(os.TempDir(), strconv.Itoa(s.pid)+".sock")}
	if version := s.behavior["version"]; version != "none" {
		document["version"] = version
	}
	if bridge != "" {
		document["bridgeSessionId"] = bridge
	}
	body, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return rcmodWrite(s.record, string(body))
}

// rcmodWrite writes through a rename, as Claude Code and the mod do, so a
// reader never sees half a file unless a behaviour asks for it.
func rcmodWrite(path, body string) error {
	if err := os.WriteFile(path+".rcmod", []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".rcmod", path)
}

func (s *rcmodSession) violation(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
}

func (s *rcmodSession) serve(ctx context.Context, wg *sync.WaitGroup) {
	dir := filepath.Join(s.home, ".claude", "agentctl", "remote-control", strconv.Itoa(s.pid))
	seen := map[string]bool{}
	for ctx.Err() == nil {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			match := rcmodRequestName.FindStringSubmatch(entry.Name())
			if match == nil || seen[match[1]] {
				continue
			}
			seen[match[1]] = true
			path := filepath.Join(dir, entry.Name())
			info, err := os.Lstat(path)
			if err != nil {
				continue
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(stat.Uid) != os.Getuid() {
				s.violation("request %s is not a 0600 regular file this user owns: %v", entry.Name(), info.Mode())
			}
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var request struct {
				V         int    `json:"v"`
				ID        string `json:"id"`
				Action    string `json:"action"`
				IssuedAt  int64  `json:"issuedAt"`
				ExpiresAt int64  `json:"expiresAt"`
				Subject   struct {
					Service string `json:"service"`
				} `json:"subject"`
			}
			if err := json.Unmarshal(body, &request, json.RejectUnknownMembers(true)); err != nil || request.V != 1 || request.ID != match[1] || request.Subject.Service == "" || request.ExpiresAt <= request.IssuedAt {
				s.violation("request %s is malformed: %v %s", entry.Name(), err, body)
				continue
			}
			if ctx.Err() == nil {
				wg.Go(func() { s.answer(ctx, dir, request.ID, request.Action) })
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *rcmodSession) answer(ctx context.Context, dir, id, action string) {
	ack := func(state, reason string) string {
		member := ""
		if reason != "" {
			member = fmt.Sprintf(",%q:%q", "reason", reason)
		}
		return fmt.Sprintf(`{"v":1,"id":%q,"action":%q,"state":%q%s,"answeredAt":%d}`, id, action, state, member, time.Now().UnixMilli())
	}
	response := func(responseID, result string) string {
		s.mu.Lock()
		generation := s.generation
		s.mu.Unlock()
		return fmt.Sprintf(`{"v":1,"id":%q,"action":%q,"result":%q,"answeredAt":%d,"bridge":{"present":%t,"generation":%d},"surfaces":["terminal"],"version":"2.1.296","remoteControlListed":%t,"provenance":%s}`, responseID, action, result, time.Now().UnixMilli(), result == "ok" || result == "reconnected", generation, s.behavior["listed"] != "false", s.provenance())
	}
	write := func(suffix, body string) {
		if err := rcmodWrite(filepath.Join(dir, id+suffix), body); err != nil && ctx.Err() == nil {
			s.violation("answer %s%s could not be written: %v", id, suffix, err)
		}
	}
	pause := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	switch action {
	case "status":
		s.mu.Lock()
		s.statuses++
		s.mu.Unlock()
		behavior := s.behavior["status"]
		switch {
		case behavior == "silent":
			return
		case strings.HasPrefix(behavior, "reject:"):
			write(".ack.json", ack("rejected", strings.TrimPrefix(behavior, "reject:")))
			return
		case behavior == "truncated":
			full := ack("accepted", "")
			write(".ack.json", full[:len(full)/2])
			if !pause(100 * time.Millisecond) {
				return
			}
			write(".ack.json", full)
		default:
			write(".ack.json", ack("accepted", ""))
		}
		write(".response.json", response(id, "ok"))
		s.scheduleDrop(ctx, pause)
	case "reconnect":
		s.mu.Lock()
		s.reconnects++
		s.mu.Unlock()
		if delay, _ := strconv.Atoi(s.behavior["delay"]); delay > 0 && !pause(time.Duration(delay)*time.Millisecond) {
			return
		}
		behavior := s.behavior["reconnect"]
		switch {
		case behavior == "silent":
			return
		case strings.HasPrefix(behavior, "reject:"):
			write(".ack.json", ack("rejected", strings.TrimPrefix(behavior, "reject:")))
			return
		case behavior == "unknown":
			write(".ack.json", ack("accepted", ""))
			write(".response.json", response(id, "probably"))
			return
		case behavior == "unavailable", behavior == "expired":
			write(".ack.json", ack("accepted", ""))
			write(".response.json", response(id, behavior))
			return
		case behavior == "truncated":
			full := ack("accepted", "")
			write(".ack.json", full[:len(full)/2])
			if !pause(100 * time.Millisecond) {
				return
			}
			write(".ack.json", full)
		case behavior == "wrongid":
			write(".ack.json", ack("accepted", ""))
			write(".response.json", response(strings.Repeat("f", 32), "reconnected"))
			if !pause(400 * time.Millisecond) {
				return
			}
		case behavior == "early":
		default:
			write(".ack.json", ack("accepted", ""))
		}
		s.mu.Lock()
		s.generation++
		generation := s.generation
		s.mu.Unlock()
		if err := s.writeRecord(fmt.Sprintf("rcmod-bridge-%s-%d", s.name, generation)); err != nil {
			s.violation("the registry record could not be rewritten: %v", err)
		}
		write(".response.json", response(id, "reconnected"))
		if behavior == "early" && pause(150*time.Millisecond) {
			write(".ack.json", ack("accepted", ""))
		}
	default:
		s.violation("unknown action %q", action)
	}
}

// scheduleDrop plays Claude Code dropping the bridge after the credential
// changed. It runs after each status answer, because the swap follows the
// status round.
func (s *rcmodSession) scheduleDrop(ctx context.Context, pause func(time.Duration) bool) {
	switch drop := s.behavior["drop"]; drop {
	case "never":
	case "replace":
		if pause(200 * time.Millisecond) {
			s.mu.Lock()
			s.generation++
			generation := s.generation
			s.mu.Unlock()
			if err := s.writeRecord(fmt.Sprintf("rcmod-bridge-%s-%d", s.name, generation)); err != nil {
				s.violation("the registry record could not be rewritten: %v", err)
			}
		}
	case "vanish":
		if pause(200*time.Millisecond) && ctx.Err() == nil {
			if err := os.Remove(s.record); err != nil {
				s.violation("the registry record could not be removed: %v", err)
			}
		}
	default:
		delay, err := strconv.Atoi(drop)
		if err != nil {
			s.violation("drop %q is not a number of milliseconds", drop)
			return
		}
		if pause(time.Duration(delay) * time.Millisecond) {
			if err := s.writeRecord(""); err != nil {
				s.violation("the registry record could not be rewritten: %v", err)
			}
		}
	}
}

func (s *rcmodSession) provenance() string {
	unset := map[string]any{"set": false, "value": ""}
	document := map[string]any{"home": s.home, "configDir": unset, "secureStorageDir": unset, "oauthTokenSet": false, "apiKeySet": false, "baseUrlSet": false, "authorized": true}
	switch s.behavior["provenance"] {
	case "live":
	case "explicit":
		document["configDir"] = map[string]any{"set": true, "value": filepath.Join(s.home, ".claude")}
	case "isolated":
		document["secureStorageDir"] = map[string]any{"set": true, "value": filepath.Join(s.home, "isolated")}
	case "gateway":
		document["authorized"] = false
		document["baseUrlSet"] = true
	case "token":
		document["oauthTokenSet"] = true
	default:
		s.violation("unknown provenance %q", s.behavior["provenance"])
	}
	body, err := json.Marshal(document)
	if err != nil {
		s.violation("the provenance could not be encoded: %v", err)
		return "{}"
	}
	return string(body)
}

// check asserts how many requests of each action a session received, and
// that every request it saw kept the transport contract.
func (world *rcmodWorld) check(ts *testscript.TestScript, name, statuses, reconnects string) {
	world.mu.Lock()
	session, ok := world.sessions[ts.Getenv("HOME")+"\x00"+name]
	world.mu.Unlock()
	if !ok {
		ts.Fatalf("no rcmod session %q", name)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.violations) != 0 {
		ts.Fatalf("rcmod session %q saw contract violations:\n%s", name, strings.Join(session.violations, "\n"))
	}
	if got := fmt.Sprintf("%d %d", session.statuses, session.reconnects); got != statuses+" "+reconnects {
		ts.Fatalf("rcmod session %q received %s status/reconnect requests, want %s %s", name, got, statuses, reconnects)
	}
}

// interrupt waits for a reconnect request to reach any session, then
// sends SIGINT to the one swap running in the background.
func (world *rcmodWorld) interrupt(ts *testscript.TestScript) {
	home := ts.Getenv("HOME")
	if !WaitUntil(60*time.Second, func() bool {
		world.mu.Lock()
		defer world.mu.Unlock()
		for _, session := range world.sessions {
			if session.home != home {
				continue
			}
			session.mu.Lock()
			reconnects := session.reconnects
			session.mu.Unlock()
			if reconnects != 0 {
				return true
			}
		}
		return false
	}) {
		ts.Fatalf("no reconnect request arrived to interrupt")
	}
	background := ts.BackgroundCmds()
	if len(background) != 1 {
		ts.Fatalf("observed %d background commands; want 1", len(background))
	}
	world.interrupted = background[0]
	ts.Check(world.interrupted.Process.Signal(syscall.SIGINT))
}

// rcmodCounts compares the outcome document's remote_control object in the
// last command's stdout with the named counts; every count not named must
// be zero, and the object must carry all twelve.
func rcmodCounts(ts *testscript.TestScript, args []string) {
	want := map[string]int{}
	for _, name := range []string{"eligible", "provenance_skipped", "unreachable", "unavailable", "version_rejected", "metadata_rejected", "dropped", "not_dropped", "reconnected", "not_confirmed", "already_connected", "restored"} {
		want[name] = 0
	}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		count, err := strconv.Atoi(value)
		if _, known := want[key]; !ok || !known || err != nil {
			ts.Fatalf("usage: rcmod counts [count=n...]; bad %q", arg)
		}
		want[key] = count
	}
	decoder := jsontext.NewDecoder(strings.NewReader(ts.ReadFile("stdout")))
	var got map[string]int
	found := false
	for {
		value, err := decoder.ReadValue()
		if err == io.EOF {
			break
		}
		ts.Check(err)
		var document struct {
			Kind          string         `json:"kind"`
			RemoteControl map[string]int `json:"remote_control"`
		}
		ts.Check(json.Unmarshal(value, &document))
		if document.Kind == "outcome" {
			got, found = document.RemoteControl, true
		}
	}
	if !found || got == nil {
		ts.Fatalf("stdout has no outcome document with a remote_control object")
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		ts.Fatalf("remote_control counts mismatch (-want +got):\n%s", diff)
	}
}
