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

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude"
)

// testRCTiming keeps every wait short enough for a unit test while leaving
// the ack window longer than the poll.
func testRCTiming() useRCTiming {
	return useRCTiming{preflight: 2 * time.Second, status: 400 * time.Millisecond, drop: 400 * time.Millisecond, reconnect: 800 * time.Millisecond, followUp: 3 * time.Second, poll: 10 * time.Millisecond}
}

func testRCTransport(t *testing.T) (useRCTransport, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := claude.EnvWithHome(home)
	return newUseRCTransport(&env, testRCTiming()), home
}

func writeRCFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path+".partial", []byte(body), 0o600); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(path+".partial", path); err != nil {
		t.Error(err)
	}
}

var testRCRequestName = regexp.MustCompile(`^([0-9a-f]{32})\.request\.json$`)

// testRCResponder plays the mod: it answers every request file that
// appears in dir through handle, until the test ends.
func testRCResponder(t *testing.T, dir string, handle func(dir string, request useRCRequest)) {
	t.Helper()
	ctx := t.Context()
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		seen := map[string]bool{}
		for ctx.Err() == nil {
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				match := testRCRequestName.FindStringSubmatch(entry.Name())
				if match == nil || seen[match[1]] {
					continue
				}
				seen[match[1]] = true
				body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					continue
				}
				var request useRCRequest
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("the request is not valid JSON: %v", err)
					continue
				}
				handle(dir, request)
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
}

func rcAck(request useRCRequest, state, reason string) string {
	reasonMember := ""
	if reason != "" {
		reasonMember = fmt.Sprintf(`,"reason":%q`, reason)
	}
	return fmt.Sprintf(`{"v":1,"id":%q,"action":%q,"state":%q%s,"answeredAt":%d}`, request.ID, request.Action, state, reasonMember, time.Now().UnixMilli())
}

// rcAckAs writes an acknowledgement whose action member is chosen by the
// test, as the mod does when it rejects a request before reading it.
func rcAckAs(request useRCRequest, action, state, reason string) string {
	request.Action = action
	return rcAck(request, state, reason)
}

func rcStatus(request useRCRequest, provenance string) string {
	return fmt.Sprintf(`{"v":1,"id":%q,"action":"status","result":"ok","answeredAt":%d,"bridge":{"present":true,"generation":1},"surfaces":["terminal"],"version":"2.1.296","remoteControlListed":true,"provenance":%s}`, request.ID, time.Now().UnixMilli(), provenance)
}

func rcReconnect(request useRCRequest, result string) string {
	return fmt.Sprintf(`{"v":1,"id":%q,"action":"reconnect","result":%q,"answeredAt":%d,"bridge":{"present":true,"generation":2},"surfaces":["terminal"],"version":"2.1.296","remoteControlListed":true,"provenance":{"home":"/h","configDir":{"set":false,"value":""},"secureStorageDir":{"set":false,"value":""},"oauthTokenSet":false,"apiKeySet":false,"baseUrlSet":false,"authorized":true}}`, request.ID, result, time.Now().UnixMilli())
}

const rcLiveProvenance = `{"home":"/h","configDir":{"set":false,"value":""},"secureStorageDir":{"set":false,"value":""},"oauthTokenSet":false,"apiKeySet":false,"baseUrlSet":false,"authorized":true}`

func TestUseRCTransportDirectoryRefusals(t *testing.T) {
	tests := map[string]struct {
		prepare func(t *testing.T, transport *useRCTransport)
		want    error
	}{
		"success: a missing chain is created closed": {
			prepare: func(*testing.T, *useRCTransport) {},
		},
		"error: a symlinked session directory": {
			prepare: func(t *testing.T, transport *useRCTransport) {
				if err := os.MkdirAll(transport.root, 0o700); err != nil {
					t.Fatal(err)
				}
				target := t.TempDir()
				if err := os.Symlink(target, filepath.Join(transport.root, "4242")); err != nil {
					t.Fatal(err)
				}
			},
			want: errUseRCSymlink,
		},
		"error: a symlinked agentctl directory": {
			prepare: func(t *testing.T, transport *useRCTransport) {
				if err := os.Symlink(t.TempDir(), filepath.Dir(transport.root)); err != nil {
					t.Fatal(err)
				}
			},
			want: errUseRCSymlink,
		},
		"error: a directory open to other users": {
			prepare: func(t *testing.T, transport *useRCTransport) {
				if err := os.MkdirAll(transport.root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(transport.root, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: errUseRCMode,
		},
		"error: a file where a directory belongs": {
			prepare: func(t *testing.T, transport *useRCTransport) {
				if err := os.MkdirAll(transport.root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(transport.root, "4242"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: errUseRCNotDir,
		},
		"error: a directory another uid owns": {
			prepare: func(_ *testing.T, transport *useRCTransport) {
				transport.uid = os.Getuid() + 1
			},
			want: errUseRCOwner,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			transport, home := testRCTransport(t)
			tt.prepare(t, &transport)
			dir, err := transport.open(4242)
			if !errors.Is(err, tt.want) {
				t.Fatalf("open error=%v, want %v", err, tt.want)
			}
			if tt.want != nil {
				return
			}
			if want := filepath.Join(home, ".claude", "agentctl", "remote-control", "4242"); dir != want {
				t.Fatalf("dir=%q, want %q", dir, want)
			}
			for _, path := range []string{filepath.Dir(transport.root), transport.root, dir} {
				info, err := os.Lstat(path)
				if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
					t.Fatalf("%s: info=%v err=%v, want a 0700 directory", path, info, err)
				}
			}
		})
	}
}

func TestUseRCTransportForeignOwner(t *testing.T) {
	foreign := os.Getenv("AGENTCTL_TEST_FOREIGN_UID")
	if foreign == "" {
		t.Skip("set AGENTCTL_TEST_FOREIGN_UID to a second uid this process may chown to; without one only the uid comparison is covered")
	}
	uid, err := strconv.Atoi(foreign)
	if err != nil {
		t.Fatal(err)
	}
	transport, _ := testRCTransport(t)
	if err := os.MkdirAll(transport.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(transport.root, uid, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.open(4242); !errors.Is(err, errUseRCOwner) {
		t.Fatalf("open error=%v, want %v", err, errUseRCOwner)
	}
}

func TestUseRCSendWritesAClosedRequestAtomically(t *testing.T) {
	transport, _ := testRCTransport(t)
	dir, err := transport.open(7)
	if err != nil {
		t.Fatal(err)
	}
	id, issued, err := transport.send(dir, useRCActionReconnect, claude.LiveService, useRCReconnectLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("id %q is not 32 lowercase hex characters", id)
	}
	path := filepath.Join(dir, id+".request.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("request info=%v err=%v, want a 0600 regular file", info, err)
	}
	if _, err := os.Lstat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the temporary request was left behind: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got useRCRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := useRCRequest{V: 1, ID: id, Action: "reconnect", IssuedAt: issued.UnixMilli(), ExpiresAt: issued.Add(useRCReconnectLifetime).UnixMilli(), Subject: useRCSubject{Service: claude.LiveService}}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("request mismatch (-want +got):\n%s", diff)
	}
	other, _, err := transport.send(dir, useRCActionStatus, claude.LiveService, useRCStatusLifetime)
	if err != nil || other == id {
		t.Fatalf("a second request reused the id or failed: %v", err)
	}
	transport.clean(dir, id)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("clean left the request: %v", err)
	}
}

func TestUseRCWait(t *testing.T) {
	tests := map[string]struct {
		action     string
		handle     func(t *testing.T, dir string, request useRCRequest)
		cancel     bool
		wantKind   useRCAnswerKind
		wantReason string
		wantResult string
	}{
		"success: ack then response": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				time.Sleep(30 * time.Millisecond)
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(r, "reconnected"))
			},
			wantKind: useRCAnswered, wantResult: "reconnected",
		},
		"success: a response before its ack is taken": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(r, "already_connected"))
			},
			wantKind: useRCAnswered, wantResult: "already_connected",
		},
		"success: a truncated ack is retried until a valid one": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				ack := rcAck(r, "accepted", "")
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), ack[:len(ack)/2])
				time.Sleep(60 * time.Millisecond)
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), ack)
				time.Sleep(60 * time.Millisecond)
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(r, "unavailable"))
			},
			wantKind: useRCAnswered, wantResult: "unavailable",
		},
		"success: a status answer": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcStatus(r, rcLiveProvenance))
			},
			wantKind: useRCAnswered, wantResult: "ok",
		},
		"error: a rejected ack carries its reason": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "rejected", "metadata"))
			},
			wantKind: useRCRejected, wantReason: "metadata",
		},
		"error: a status rejection with an empty action carries its reason": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAckAs(r, "", "rejected", "metadata"))
			},
			wantKind: useRCRejected, wantReason: "metadata",
		},
		"error: a reconnect rejection with an empty action carries its reason": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAckAs(r, "", "rejected", "version"))
			},
			wantKind: useRCRejected, wantReason: "version",
		},
		"error: a rejection naming another action is ignored": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAckAs(r, useRCActionReconnect, "rejected", "metadata"))
			},
			wantKind: useRCSilent,
		},
		"error: a rejection with an empty action for another id is ignored": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				other := r
				other.ID = strings.Repeat("0", 32)
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAckAs(other, "", "rejected", "metadata"))
			},
			wantKind: useRCSilent,
		},
		"error: an acceptance with an empty action is ignored": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAckAs(r, "", "accepted", ""))
			},
			wantKind: useRCSilent,
		},
		"error: silence is no ack": {
			action:   useRCActionStatus,
			handle:   func(*testing.T, string, useRCRequest) {},
			wantKind: useRCSilent,
		},
		"error: an ack without a response runs to the deadline": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
			},
			wantKind: useRCNoResponse,
		},
		"error: a response for another request is ignored": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				other := r
				other.ID = strings.Repeat("0", 32)
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(other, "reconnected"))
			},
			wantKind: useRCNoResponse,
		},
		"error: a response for another action is ignored": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcStatus(r, rcLiveProvenance))
			},
			wantKind: useRCNoResponse,
		},
		"error: an unknown result is retried to the deadline": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(r, "probably"))
			},
			wantKind: useRCNoResponse,
		},
		"error: a status answer without remoteControlListed is retried": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), strings.Replace(rcStatus(r, rcLiveProvenance), `"remoteControlListed":true,`, "", 1))
			},
			wantKind: useRCNoResponse,
		},
		"error: a status answer without provenance is retried": {
			action: useRCActionStatus,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), strings.Replace(rcStatus(r, rcLiveProvenance), `"provenance":`, `"elsewhere":`, 1))
			},
			wantKind: useRCNoResponse,
		},
		"error: a symlinked response is never read": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				target := filepath.Join(t.TempDir(), "planted")
				writeRCFile(t, target, rcReconnect(r, "reconnected"))
				if err := os.Symlink(target, filepath.Join(dir, r.ID+".response.json")); err != nil {
					t.Error(err)
				}
			},
			wantKind: useRCNoResponse,
		},
		"error: cancellation ends the wait": {
			action: useRCActionReconnect,
			handle: func(t *testing.T, dir string, r useRCRequest) {
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
			},
			cancel:   true,
			wantKind: useRCInterrupted,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport, _ := testRCTransport(t)
			dir, err := transport.open(99)
			if err != nil {
				t.Fatal(err)
			}
			testRCResponder(t, dir, func(dir string, r useRCRequest) { tt.handle(t, dir, r) })
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
			}
			lifetime := transport.timing.reconnect
			if tt.action == useRCActionStatus {
				lifetime = transport.timing.status
			}
			answer := transport.exchange(ctx, dir, tt.action, claude.LiveService, lifetime)
			result := ""
			if answer.kind == useRCAnswered {
				result = answer.response.result
			}
			if answer.kind != tt.wantKind || answer.reason != tt.wantReason || result != tt.wantResult {
				t.Fatalf("answer kind=%d reason=%q result=%q, want kind=%d reason=%q result=%q", answer.kind, answer.reason, result, tt.wantKind, tt.wantReason, tt.wantResult)
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".request.json") {
					t.Fatalf("the request was not removed: %s", entry.Name())
				}
			}
		})
	}
}

func TestUseRCRejectionClasses(t *testing.T) {
	tests := map[string]struct {
		reason string
		want   useRCClass
	}{
		"success: metadata":                  {reason: "metadata", want: useRCMetadataRejected},
		"success: version":                   {reason: "version", want: useRCVersionRejected},
		"error: busy is unreachable":         {reason: "busy", want: useRCUnreachable},
		"error: name is unreachable":         {reason: "name", want: useRCUnreachable},
		"error: duplicate is unreachable":    {reason: "duplicate", want: useRCUnreachable},
		"error: no reason is unreachable":    {reason: "", want: useRCUnreachable},
		"error: an unknown word unreachable": {reason: "later", want: useRCUnreachable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := useRCRejection(tt.reason); got != tt.want {
				t.Fatalf("useRCRejection(%q) = %d, want %d", tt.reason, got, tt.want)
			}
		})
	}
}

// TestUseRCReadsTheModMetadataRejection crosses the two halves: the bytes the
// mod's own test pins for a request file with the wrong owner or mode (an
// empty action, because the mod rejects it before reading the body) must
// class the session as a metadata rejection, not as one that never answered.
func TestUseRCReadsTheModMetadataRejection(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "plugins", "remote-control", "testdata", "ack-metadata-rejected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID == "" {
		t.Fatalf("the fixture has no id: %v", err)
	}
	transport, _ := testRCTransport(t)
	dir, err := transport.open(99)
	if err != nil {
		t.Fatal(err)
	}
	writeRCFile(t, filepath.Join(dir, envelope.ID+".ack.json"), string(body))
	answer := transport.wait(t.Context(), dir, envelope.ID, useRCActionStatus, time.Now(), transport.timing.status)
	if answer.kind != useRCRejected {
		t.Fatalf("answer kind=%d, want the rejection", answer.kind)
	}
	if got := useRCRejection(answer.reason); got != useRCMetadataRejected {
		t.Fatalf("class=%d for reason %q, want metadata_rejected", got, answer.reason)
	}
}

// rcCompleteResponse is a response carrying every member the contract
// requires, with both directory variables set so their values are required.
func rcCompleteResponse(request useRCRequest, action, result string) map[string]any {
	set := func(value string) map[string]any { return map[string]any{"set": true, "value": value} }
	return map[string]any{
		"v": 1, "id": request.ID, "action": action, "result": result, "answeredAt": time.Now().UnixMilli(),
		"bridge":   map[string]any{"present": true, "generation": 2},
		"surfaces": []any{"terminal"}, "version": "2.1.296", "remoteControlListed": true,
		"provenance": map[string]any{"home": "/h", "configDir": set("/h/.claude"), "secureStorageDir": set("/h/secure"), "oauthTokenSet": false, "apiKeySet": false, "baseUrlSet": false, "authorized": true},
	}
}

// rcEdit applies change to the object at the dotted path's parent.
func rcEdit(document map[string]any, path string, change func(parent map[string]any, key string)) {
	keys := strings.Split(path, ".")
	parent := document
	for _, key := range keys[:len(keys)-1] {
		parent = parent[key].(map[string]any)
	}
	change(parent, keys[len(keys)-1])
}

func TestUseRCReadResponseRequiresEveryMember(t *testing.T) {
	type testCase struct {
		action string
		edit   func(document map[string]any)
		want   bool
		// wantProvenance is the converted provenance of a complete response.
		wantProvenance claude.RemoteControlProvenance
	}
	complete := claude.RemoteControlProvenance{Home: "/h", ConfigDir: claude.RemoteControlEnvValue{Set: true, Value: "/h/.claude"}, SecureStorageDir: claude.RemoteControlEnvValue{Set: true, Value: "/h/secure"}, Authorized: true}
	tests := map[string]testCase{}
	required := []string{
		"v", "id", "action", "result", "answeredAt", "bridge", "bridge.present", "bridge.generation", "surfaces", "version", "remoteControlListed",
		"provenance", "provenance.home", "provenance.configDir", "provenance.configDir.set", "provenance.configDir.value",
		"provenance.secureStorageDir", "provenance.secureStorageDir.set", "provenance.secureStorageDir.value",
		"provenance.oauthTokenSet", "provenance.apiKeySet", "provenance.baseUrlSet", "provenance.authorized",
	}
	// One case per required member and action, so a member the reader stops
	// checking fails by name.
	for _, action := range []string{useRCActionStatus, useRCActionReconnect} {
		tests["success: a complete "+action+" response"] = testCase{action: action, edit: func(map[string]any) {}, want: true, wantProvenance: complete}
		tests["success: an unset variable needs no value in a "+action+" response"] = testCase{
			action: action,
			edit: func(document map[string]any) {
				rcEdit(document, "provenance.configDir", func(parent map[string]any, key string) { parent[key] = map[string]any{"set": false} })
			},
			want:           true,
			wantProvenance: claude.RemoteControlProvenance{Home: "/h", SecureStorageDir: complete.SecureStorageDir, Authorized: true},
		}
		for _, path := range required {
			tests["error: a "+action+" response without "+path] = testCase{action: action, edit: func(document map[string]any) {
				rcEdit(document, path, func(parent map[string]any, key string) { delete(parent, key) })
			}}
			tests["error: a "+action+" response with a null "+path] = testCase{action: action, edit: func(document map[string]any) {
				rcEdit(document, path, func(parent map[string]any, key string) { parent[key] = nil })
			}}
		}
		tests["error: a "+action+" response whose provenance is only authorized"] = testCase{action: action, edit: func(document map[string]any) {
			document["provenance"] = map[string]any{"authorized": true}
		}}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport, _ := testRCTransport(t)
			dir, err := transport.open(99)
			if err != nil {
				t.Fatal(err)
			}
			result := "ok"
			lifetime := transport.timing.status
			if tt.action == useRCActionReconnect {
				result, lifetime = claude.RemoteControlReconnected, transport.timing.reconnect
			}
			testRCResponder(t, dir, func(dir string, r useRCRequest) {
				document := rcCompleteResponse(r, tt.action, result)
				tt.edit(document)
				body, err := json.Marshal(document)
				if err != nil {
					t.Error(err)
					return
				}
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), string(body))
			})
			answer := transport.exchange(t.Context(), dir, tt.action, claude.LiveService, lifetime)
			if !tt.want {
				// Without a complete response nothing is counted as an
				// answer: the status round classes the session unreachable,
				// and the follow-up counts it not confirmed.
				if answer.kind != useRCNoResponse || answer.response != (useRCResponse{}) {
					t.Fatalf("answer kind=%d response=%+v, want no response", answer.kind, answer.response)
				}
				return
			}
			want := useRCResponse{result: result, listed: true, provenance: tt.wantProvenance}
			if answer.kind != useRCAnswered {
				t.Fatalf("answer kind=%d, want an answer", answer.kind)
			}
			if diff := gocmp.Diff(want, answer.response, gocmp.AllowUnexported(useRCResponse{})); diff != "" {
				t.Fatalf("response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUseRCSweepRemovesOnlyOwnStaleFiles(t *testing.T) {
	transport, _ := testRCTransport(t)
	dir, err := transport.open(5)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	files := map[string]bool{
		strings.Repeat("a", 32) + ".request.json":     false,
		strings.Repeat("b", 32) + ".ack.json":         false,
		strings.Repeat("c", 32) + ".response.json":    false,
		strings.Repeat("d", 32) + ".request.json.tmp": false,
		"notes.txt":                           true,
		strings.Repeat("A", 32) + ".ack.json": true,
	}
	for name := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fresh := strings.Repeat("e", 32) + ".response.json"
	if err := os.WriteFile(filepath.Join(dir, fresh), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	files[fresh] = true
	transport.sweep(dir, time.Now())
	for name, kept := range files {
		_, err := os.Lstat(filepath.Join(dir, name))
		if exists := err == nil; exists != kept {
			t.Errorf("%s: exists=%v, want %v", name, exists, kept)
		}
	}
}

// testRCSession writes a registry record and returns a scanned session for
// it, as useScanSessions would.
func testRCSession(t *testing.T, home string, pid uint32, version, bridge string) useBridgedSession {
	t.Helper()
	path := filepath.Join(home, ".claude", "sessions", strconv.FormatUint(uint64(pid), 10)+".json")
	body := fmt.Sprintf(`{"pid":%d,"sessionId":"local-%d","version":%q,"status":"idle","bridgeSessionId":%q}`, pid, pid, version, bridge)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := claude.DecodeRegistryRecord([]byte(body))
	return useBridgedSession{path: path, name: fmt.Sprintf("session-%d", pid), pid: pid, record: record, decoded: err == nil}
}

func TestUseRemoteControlPreflight(t *testing.T) {
	isolated := `{"home":"/h","configDir":{"set":true,"value":"/elsewhere"},"secureStorageDir":{"set":false,"value":""},"oauthTokenSet":false,"apiKeySet":false,"baseUrlSet":false,"authorized":true}`
	gateway := strings.Replace(rcLiveProvenance, `"authorized":true`, `"authorized":false`, 1)
	type session struct {
		version string
		handle  func(t *testing.T, dir string, r useRCRequest)
	}
	answer := func(provenance string) func(t *testing.T, dir string, r useRCRequest) {
		return func(t *testing.T, dir string, r useRCRequest) {
			writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
			writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcStatus(r, provenance))
		}
	}
	unlisted := func(t *testing.T, dir string, r useRCRequest) {
		writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), strings.Replace(rcStatus(r, rcLiveProvenance), `"remoteControlListed":true`, `"remoteControlListed":false`, 1))
	}
	reject := func(reason string) func(t *testing.T, dir string, r useRCRequest) {
		return func(t *testing.T, dir string, r useRCRequest) {
			writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "rejected", reason))
		}
	}
	tests := map[string]struct {
		sessions   []session
		unreadable string
		want       claude.RemoteControlCounts
		refused    bool
		names      int
	}{
		"success: one eligible session": {
			sessions: []session{{version: "2.1.296", handle: answer(rcLiveProvenance)}},
			want:     claude.RemoteControlCounts{Eligible: 1},
		},
		"success: every class but unreachable": {
			sessions: []session{
				{version: "2.1.296", handle: answer(rcLiveProvenance)},
				{version: "2.1.296", handle: answer(isolated)},
				{version: "2.1.296", handle: answer(gateway)},
				{version: "2.1.286", handle: answer(rcLiveProvenance)},
				{version: "", handle: answer(rcLiveProvenance)},
				{version: "2.1.296", handle: reject("metadata")},
				{version: "2.1.296", handle: reject("version")},
				{version: "2.1.296", handle: unlisted},
			},
			want:  claude.RemoteControlCounts{Eligible: 1, ProvenanceSkipped: 2, VersionRejected: 3, MetadataRejected: 1, Unavailable: 1},
			names: 7,
		},
		"error: a silent session refuses the swap": {
			sessions: []session{{version: "2.1.296", handle: answer(rcLiveProvenance)}, {version: "2.1.296", handle: func(*testing.T, string, useRCRequest) {}}},
			want:     claude.RemoteControlCounts{Eligible: 1, Unreachable: 1},
			refused:  true,
			names:    1,
		},
		"error: a rejection for another reason is unreachable": {
			sessions: []session{{version: "2.1.296", handle: reject("duplicate")}},
			want:     claude.RemoteControlCounts{Unreachable: 1},
			refused:  true,
			names:    1,
		},
		"error: an unreadable registry refuses the swap": {
			unreadable: "permission denied",
			refused:    true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport, home := testRCTransport(t)
			env := claude.EnvWithHome(home)
			hints := useSessionHints{unreadable: tt.unreadable}
			for i, s := range tt.sessions {
				pid := uint32(1000 + i)
				hints.sessions = append(hints.sessions, testRCSession(t, home, pid, s.version, "bridge-"+strconv.Itoa(i)))
				hints.names = append(hints.names, hints.sessions[i].name)
				dir, err := transport.open(pid)
				if err != nil {
					t.Fatal(err)
				}
				testRCResponder(t, dir, func(dir string, r useRCRequest) {
					if r.Action != useRCActionStatus || r.Subject.Service != claude.LiveService || r.ExpiresAt-r.IssuedAt != useRCStatusLifetime.Milliseconds() {
						t.Errorf("unexpected status request %+v", r)
					}
					s.handle(t, dir, r)
				})
			}
			rc, refusal := (useLiveSwap{env: &env}).remoteControlPreflight(t.Context(), &hints, claude.LiveService)
			if (refusal != nil) != tt.refused {
				t.Fatalf("refusal=%+v, want refused=%v", refusal, tt.refused)
			}
			if refusal != nil && (refusal.outcome.Refusal.Reason() != "remote_control_unreachable" || refusal.outcome.ExitCode() != 30) {
				t.Fatalf("refusal %+v is not the unreachable one", refusal.outcome)
			}
			if diff := gocmp.Diff(tt.want, rc.counts); diff != "" {
				t.Fatalf("counts mismatch (-want +got):\n%s", diff)
			}
			if len(rc.eligible) != tt.want.Eligible || hints.handled != tt.want.Eligible || len(hints.names) != tt.names {
				t.Fatalf("eligible=%d handled=%d names=%d", len(rc.eligible), hints.handled, len(hints.names))
			}
		})
	}
}

func TestUseRemoteControlPreflightSendsNothingToAnOldSession(t *testing.T) {
	transport, home := testRCTransport(t)
	env := claude.EnvWithHome(home)
	hints := useSessionHints{sessions: []useBridgedSession{testRCSession(t, home, 77, "2.1.200", "bridge")}, names: []string{"old"}}
	rc, refusal := (useLiveSwap{env: &env}).remoteControlPreflight(t.Context(), &hints, claude.LiveService)
	if refusal != nil || rc.counts.VersionRejected != 1 {
		t.Fatalf("refusal=%+v counts=%+v", refusal, rc.counts)
	}
	if _, err := os.Lstat(filepath.Join(transport.root, "77")); !os.IsNotExist(err) {
		t.Fatalf("a transport directory was made for a session that gets no request: %v", err)
	}
}

// TestUseRemoteControlPreflightRefusesARecordNotNamedByItsPid covers the
// registry record that could steer a request at another process: its pid
// must be its own file name, or the session is unreachable and the swap is
// refused before any transport directory exists.
func TestUseRemoteControlPreflightRefusesARecordNotNamedByItsPid(t *testing.T) {
	tests := map[string]struct {
		file string
		body string
	}{
		"error: the record names another pid": {
			file: "999.json",
			body: `{"pid":4242,"sessionId":"local","version":"2.1.296","status":"idle","bridgeSessionId":"bridge"}`,
		},
		"error: the record names no pid": {
			file: "999.json",
			body: `{"sessionId":"local","version":"2.1.296","status":"idle","bridgeSessionId":"bridge"}`,
		},
		"error: the file name is not a pid": {
			file: "session.json",
			body: `{"pid":4242,"sessionId":"local","version":"2.1.296","status":"idle","bridgeSessionId":"bridge"}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport, home := testRCTransport(t)
			env := claude.EnvWithHome(home)
			path := filepath.Join(home, ".claude", "sessions", tt.file)
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			record, err := claude.DecodeRegistryRecord([]byte(tt.body))
			hints := useSessionHints{sessions: []useBridgedSession{{path: path, name: "steered", pid: 4242, record: record, decoded: err == nil}}, names: []string{"steered"}}
			rc, refusal := (useLiveSwap{env: &env}).remoteControlPreflight(t.Context(), &hints, claude.LiveService)
			if refusal == nil || refusal.outcome.Refusal.Reason() != "remote_control_unreachable" {
				t.Fatalf("refusal=%+v, want the unreachable refusal", refusal)
			}
			if diff := gocmp.Diff(claude.RemoteControlCounts{Unreachable: 1}, rc.counts); diff != "" {
				t.Fatalf("counts mismatch (-want +got):\n%s", diff)
			}
			if _, err := os.Lstat(transport.root); !os.IsNotExist(err) {
				t.Fatalf("a transport directory was made for a refused record: %v", err)
			}
		})
	}
}

// testRCRegistry rewrites one record during the follow-up, as Claude Code
// would when the bridge drops or comes back.
func testRCRegistry(t *testing.T, path string, pid uint32, session, bridge string) {
	t.Helper()
	writeRCFile(t, path, fmt.Sprintf(`{"pid":%d,"sessionId":%q,"version":"2.1.296","status":"idle","bridgeSessionId":%q}`, pid, session, bridge))
}

func TestUseRemoteControlFollowUp(t *testing.T) {
	type registry func(t *testing.T, path string, pid uint32)
	drop := func(after time.Duration) registry {
		return func(t *testing.T, path string, pid uint32) {
			time.Sleep(after)
			testRCRegistry(t, path, pid, fmt.Sprintf("local-%d", pid), "")
		}
	}
	tests := map[string]struct {
		outcome  claude.SwapOutcomeKind
		config   bool
		registry registry
		reply    string
		reject   string
		cancel   bool
		want     claude.RemoteControlCounts
		warnings []string
		note     bool
		// before applies the registry change before the follow-up starts.
		before      bool
		wantRequest bool
	}{
		"success: dropped then reconnected": {
			outcome: claude.SwapApplied, config: true, registry: drop(30 * time.Millisecond), reply: "reconnected",
			want:        claude.RemoteControlCounts{Eligible: 1, Dropped: 1, Reconnected: 1},
			wantRequest: true,
		},
		"error: dropped then expired": {
			outcome: claude.SwapApplied, config: true, registry: drop(0), reply: "expired",
			want:        claude.RemoteControlCounts{Eligible: 1, Dropped: 1, NotConfirmed: 1},
			warnings:    []string{"1 Claude Code session did not confirm that Remote Control started again (its request expired before the session was idle); run `/remote-control` there"},
			wantRequest: true,
		},
		"error: dropped then rejected for metadata": {
			outcome: claude.SwapApplied, config: true, registry: drop(0), reject: "metadata",
			want:        claude.RemoteControlCounts{Eligible: 1, Dropped: 1, MetadataRejected: 1},
			warnings:    []string{"1 Claude Code session with Remote Control on refused agentctl's request because the request file's owner or mode was not what the mod expects, so agentctl asked it nothing more"},
			wantRequest: true,
		},
		"error: dropped then silent": {
			outcome: claude.SwapApplied, config: true, registry: drop(0),
			want:        claude.RemoteControlCounts{Eligible: 1, Dropped: 1, NotConfirmed: 1},
			warnings:    []string{"1 Claude Code session did not confirm that Remote Control started again; run `/remote-control` there"},
			wantRequest: true,
		},
		"error: never dropped": {
			outcome: claude.SwapApplied, config: true, registry: func(*testing.T, string, uint32) {},
			want:     claude.RemoteControlCounts{Eligible: 1, NotDropped: 1},
			warnings: []string{"1 Claude Code session still had the earlier Remote Control bridge 45 s after the swap, so agentctl did not ask it to start it again; Claude Code stops it on its next account check, then run `/remote-control` there"},
		},
		"success: a bridge replaced without vanishing is already connected": {
			outcome: claude.SwapApplied, config: true,
			registry: func(t *testing.T, path string, pid uint32) {
				testRCRegistry(t, path, pid, fmt.Sprintf("local-%d", pid), "bridge-new")
			},
			want: claude.RemoteControlCounts{Eligible: 1, AlreadyConnected: 1},
		},
		"error: a record that names another session is not confirmed": {
			outcome: claude.SwapApplied, config: true,
			registry: func(t *testing.T, path string, pid uint32) {
				testRCRegistry(t, path, pid, "someone-else", "bridge-0")
			},
			want:     claude.RemoteControlCounts{Eligible: 1, NotConfirmed: 1},
			warnings: []string{"1 Claude Code session did not confirm that Remote Control started again; run `/remote-control` there"},
		},
		"error: a record that disappeared is not confirmed": {
			outcome: claude.SwapApplied, config: true,
			registry: func(t *testing.T, path string, _ uint32) {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
			},
			want:     claude.RemoteControlCounts{Eligible: 1, NotConfirmed: 1},
			warnings: []string{"1 Claude Code session did not confirm that Remote Control started again; run `/remote-control` there"},
		},
		"error: cancellation counts the pending session and notes it": {
			outcome: claude.SwapApplied, config: true, registry: func(*testing.T, string, uint32) {}, cancel: true,
			want:     claude.RemoteControlCounts{Eligible: 1, NotConfirmed: 1},
			warnings: []string{"1 Claude Code session did not confirm that Remote Control started again; run `/remote-control` there"},
			note:     true,
		},
		"error: config not updated sends nothing": {
			outcome: claude.SwapApplied, registry: drop(0),
			want:     claude.RemoteControlCounts{Eligible: 1},
			warnings: []string{"the swap applied but Claude Code's configuration was not updated, so agentctl asked no session to start Remote Control again: recover the configuration first, then run `/remote-control` in the 1 Claude Code session that had it on"},
		},
		"error: an unknown outcome sends nothing": {
			outcome: claude.SwapUnknown, config: true, registry: drop(0),
			want:     claude.RemoteControlCounts{Eligible: 1},
			warnings: []string{"the swap's outcome is unknown, so agentctl asked no session to start Remote Control again: run `agentctl claude status` first; if the credential changed, run `/remote-control` in the 1 Claude Code session that had it on once it stops there"},
		},
		"success: already active counts a kept bridge": {
			outcome: claude.SwapAlreadyActive, config: true, registry: func(*testing.T, string, uint32) {},
			want: claude.RemoteControlCounts{Eligible: 1, Restored: 1},
		},
		"error: a refused pass with a lost bridge warns": {
			outcome: claude.SwapRefused, registry: drop(0), before: true,
			want:     claude.RemoteControlCounts{Eligible: 1},
			warnings: []string{"1 Claude Code session that had Remote Control on no longer shows a bridge, although this pass changed no credential; run `/remote-control` there"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport, home := testRCTransport(t)
			const pid = 4321
			session := testRCSession(t, home, pid, "2.1.296", "bridge-0")
			dir, err := transport.open(pid)
			if err != nil {
				t.Fatal(err)
			}
			var requests sync.Map
			testRCResponder(t, dir, func(dir string, r useRCRequest) {
				requests.Store(r.ID, r.Action)
				if r.Action != useRCActionReconnect || r.ExpiresAt-r.IssuedAt != transport.timing.reconnect.Milliseconds() {
					t.Errorf("unexpected request %+v", r)
				}
				if tt.reject != "" {
					writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "rejected", tt.reject))
					return
				}
				if tt.reply == "" {
					return
				}
				writeRCFile(t, filepath.Join(dir, r.ID+".ack.json"), rcAck(r, "accepted", ""))
				writeRCFile(t, filepath.Join(dir, r.ID+".response.json"), rcReconnect(r, tt.reply))
			})
			if tt.before {
				tt.registry(t, session.path, pid)
			} else {
				var changes sync.WaitGroup
				t.Cleanup(changes.Wait)
				changes.Go(func() { tt.registry(t, session.path, pid) })
			}
			rc := &useRemoteControl{transport: transport, service: claude.LiveService, counts: claude.RemoteControlCounts{Eligible: 1}, eligible: []useRCSession{{path: session.path, dir: dir, record: session.record}}}
			report := &useReport{outcome: claude.SwapOutcome{Kind: tt.outcome}, rc: rc}
			if tt.config {
				report.config = &claude.ConfigReport{Outcome: "applied"}
			} else {
				report.config = new(claude.ConfigNotAttempted("absent"))
			}
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			var stderr bytes.Buffer
			(SessionProcess{Err: &stderr}).followUpRemoteControl(ctx, cli.ClaudeUseOptions{RestartRemoteControl: true}, report)
			if diff := gocmp.Diff(tt.want, *report.remoteControl); diff != "" {
				t.Fatalf("counts mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.warnings, report.warnings); diff != "" {
				t.Fatalf("warnings mismatch (-want +got):\n%s", diff)
			}
			var lines []string
			for _, warning := range tt.warnings {
				lines = append(lines, "warning: "+warning)
			}
			wantStderr := strings.Join(lines, "\n")
			if tt.note {
				wantStderr = "note: the Remote Control follow-up was interrupted: 0 reconnected, 0 already connected, 0 unavailable, 1 not confirmed, 0 not dropped" + strings.TrimSuffix("\n"+wantStderr, "\n")
			}
			if diff := gocmp.Diff(wantStderr, strings.TrimSuffix(stderr.String(), "\n")); diff != "" {
				t.Fatalf("stderr mismatch (-want +got):\n%s", diff)
			}
			sent := 0
			requests.Range(func(_, _ any) bool { sent++; return true })
			if (sent != 0) != tt.wantRequest || sent > 1 {
				t.Fatalf("requests sent=%d, want a request=%v", sent, tt.wantRequest)
			}
		})
	}
}

func TestUseRemoteControlFollowUpWithoutTheFlagOrPreflight(t *testing.T) {
	report := &useReport{outcome: claude.SwapOutcome{Kind: claude.SwapApplied}}
	(SessionProcess{}).followUpRemoteControl(t.Context(), cli.ClaudeUseOptions{}, report)
	if report.remoteControl != nil {
		t.Fatal("counts reported without the flag")
	}
	(SessionProcess{}).followUpRemoteControl(t.Context(), cli.ClaudeUseOptions{RestartRemoteControl: true}, report)
	if report.remoteControl == nil || *report.remoteControl != (claude.RemoteControlCounts{}) {
		t.Fatalf("a pass without a preflight must report zero counts, got %+v", report.remoteControl)
	}
}

func TestUseRemoteControlPlatformRefusalJSON(t *testing.T) {
	var stdout bytes.Buffer
	report := useRemoteControlPlatformRefusal()
	if err := (SessionProcess{Out: &stdout}).emitUse(report, true); err != nil {
		t.Fatal(err)
	}
	if report.outcome.ExitCode() != 30 {
		t.Fatalf("exit=%d, want 30", report.outcome.ExitCode())
	}
	var doc struct {
		Kind          string                      `json:"kind"`
		Outcome       string                      `json:"outcome"`
		Reason        string                      `json:"reason"`
		RemoteControl *claude.RemoteControlCounts `json:"remote_control"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := struct {
		Kind          string                      `json:"kind"`
		Outcome       string                      `json:"outcome"`
		Reason        string                      `json:"reason"`
		RemoteControl *claude.RemoteControlCounts `json:"remote_control"`
	}{Kind: "outcome", Outcome: "refused", Reason: "remote_control_unsupported_platform", RemoteControl: &claude.RemoteControlCounts{}}
	if diff := gocmp.Diff(want, doc); diff != "" {
		t.Fatalf("document mismatch (-want +got):\n%s", diff)
	}
	if strings.Count(stdout.String(), `": 0`) != 12 {
		t.Fatalf("the remote_control object must carry all twelve counts: %s", stdout.String())
	}
	var plain bytes.Buffer
	if err := (SessionProcess{Out: &plain}).emitUse(useRemoteControlPlatformRefusal(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain.String(), "refused: restarting Remote Control after a live swap is supported only on macOS") {
		t.Fatalf("plain output %q", plain.String())
	}
}

func TestUseOutcomeOmitsRemoteControlWithoutTheFlag(t *testing.T) {
	var stdout bytes.Buffer
	if err := (SessionProcess{Out: &stdout}).emitUse(useRefused(claude.SwapRefusal{Kind: claude.SwapNotOwned}, "", "n"), true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "remote_control") {
		t.Fatalf("remote_control present without the flag: %s", stdout.String())
	}
}

// syscall is referenced so a platform without Stat_t fails to build here
// rather than silently skipping the owner check.
var _ = syscall.Stat_t{}
