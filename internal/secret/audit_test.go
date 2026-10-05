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

package secret

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/config"
)

// newAuditStore is a store in a temporary directory.
func newAuditStore(t *testing.T) *config.Paths {
	t.Helper()
	return config.NewPaths(filepath.Join(t.TempDir(), "agctl"))
}

// newAuditStoreWithRoot is a store whose namespace root exists, ready for
// something to be planted at the log's name. EnsureDirs rather than a bare
// mkdir, because that is what the append itself runs first.
func newAuditStoreWithRoot(t *testing.T) *config.Paths {
	t.Helper()
	paths := newAuditStore(t)
	if err := paths.EnsureDirs(t.Context()); err != nil {
		t.Fatalf("the store directories should be creatable: %v", err)
	}
	return paths
}

func writeEvent(to string, from *string) *WriteEvent {
	return &WriteEvent{
		Target:      NamespaceTarget("0123abcd"),
		FromDigest8: from,
		ToDigest8:   to,
		Outcome:     WriteApplied,
		Direction:   DirectionForward,
	}
}

func sampleAt(t *testing.T, mtimeNS int64, ageMS uint64) LockSample {
	t.Helper()
	return LockSample{At: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), MtimeNS: mtimeNS, AgeMS: ageMS}
}

// breakRecord is a record with every member populated, so a shape
// assertion can see the whole key set at once: a lock that was removed and
// that a peer took back is the one combination carrying both an outcome
// and a reason.
func breakRecord(t *testing.T) *LockBreakRecord {
	t.Helper()
	sampleB := sampleAt(t, 1_700_000_000_000_000_000, 73_000)
	sampleC := sampleAt(t, 1_700_000_000_000_000_000, 73_010)
	return &LockBreakRecord{
		Path:                "/home/example/.claude/.oauth_refresh.lock",
		StoreDir:            "/home/example/.claude",
		Tree:                TreeLive,
		Service:             "Claude Code-credentials",
		Target:              TargetLive,
		SampleA:             sampleAt(t, 1_700_000_000_000_000_000, 61_000),
		SampleB:             &sampleB,
		SampleC:             &sampleC,
		IntervalWallMS:      12_000,
		IntervalMonotonicMS: 12_004,
		Evidence:            EvidenceNoStoppedClaude,
		Outcome:             OutcomeBroken,
		Reason:              ReasonRetaken,
	}
}

// topLevelKeys reads one line's member names in order.
func topLevelKeys(t *testing.T, line string) []string {
	t.Helper()
	decoder := jsontext.NewDecoder(bytes.NewReader([]byte(line)))
	var keys []string
	for {
		token, err := decoder.ReadToken()
		if err != nil {
			break
		}
		if token.Kind() != '"' || decoder.StackDepth() != 1 {
			continue
		}
		// After a member name is read, the enclosing object's item count
		// is odd; after its value, even.
		if kind, length := decoder.StackIndex(1); kind == '{' && length%2 == 1 {
			keys = append(keys, token.String())
		}
	}
	return keys
}

func lineValue(t *testing.T, line string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(line), &value); err != nil {
		t.Fatalf("the line parses: %v\n%s", err, line)
	}
	return value
}

func TestAuditAWriteLineFromBeforeTheDirectionMemberReadsAsForwardNamingNobody(t *testing.T) {
	line := `{"ts":"2026-09-10T00:00:00Z","monotonic_ms":0,"agctl_pid":1,"event":"write","target":"live","from_digest8":"deadbeef","to_digest8":"cafebabe","outcome":"applied"}`
	entry, err := decodeAuditLine([]byte(line))
	if err != nil {
		t.Fatalf("an older line still parses: %v", err)
	}
	event, ok := entry.Event.(*WriteEvent)
	if !ok {
		t.Fatalf("a write entry, got %T", entry.Event)
	}
	if event.Direction != DirectionForward {
		t.Errorf("direction = %q, want forward", event.Direction)
	}
	if event.IncomingIdentity != nil {
		t.Errorf("no identity on an old line: %+v", event.IncomingIdentity)
	}
}

func TestAuditALiveWriteRecordsTheAccountItInstalledByIDAlone(t *testing.T) {
	tests := map[string]struct {
		event        *WriteEvent
		wantIdentity bool
	}{
		"success: a forward write names the account": {
			event: &WriteEvent{
				Target:           TargetLive,
				FromDigest8:      ptr("deadbeef"),
				ToDigest8:        "cafebabe",
				Outcome:          WriteApplied,
				Direction:        DirectionForward,
				IncomingIdentity: &IncomingIdentity{AccountUUID: "acct-t"},
			},
			wantIdentity: true,
		},
		"success: an undo write may name nobody": {
			event: &WriteEvent{
				Target:      TargetLive,
				FromDigest8: ptr("cafebabe"),
				ToDigest8:   "deadbeef",
				Outcome:     WriteApplied,
				Direction:   DirectionUndo,
			},
			wantIdentity: false,
		},
		"success: an undo write names the account it put back": {
			event: &WriteEvent{
				Target:           TargetLive,
				FromDigest8:      ptr("cafebabe"),
				ToDigest8:        "deadbeef",
				Outcome:          WriteUnknown,
				Direction:        DirectionUndo,
				IncomingIdentity: &IncomingIdentity{AccountUUID: "acct-p", OrganizationUUID: ptr("org-p")},
			},
			wantIdentity: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entry := NewAuditEntry(tt.event)
			line, err := auditEntryLine(entry)
			if err != nil {
				t.Fatalf("the entry serialises: %v", err)
			}
			value := lineValue(t, line)
			if got := value["direction"]; got != string(tt.event.Direction) {
				t.Errorf("direction = %v: %s", got, line)
			}
			identity, present := value["incoming_identity"]
			if present != tt.wantIdentity {
				t.Fatalf("incoming_identity present = %v, want %v: %s", present, tt.wantIdentity, line)
			}
			if tt.wantIdentity {
				fields, ok := identity.(map[string]any)
				if !ok || len(fields) != 2 {
					t.Errorf("two ids and nothing else — no email, no name: %s", line)
				}
			}
			if strings.Contains(line, "@") {
				t.Errorf("no address anywhere on the line: %s", line)
			}

			back, err := decodeAuditLine([]byte(strings.TrimSuffix(line, "\n")))
			if err != nil {
				t.Fatalf("the line round-trips: %v", err)
			}
			if diff := gocmp.Diff(entry, back); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// And an undo line written before identities existed still parses,
	// naming nobody.
	older := `{"ts":"2026-09-11T00:00:00Z","monotonic_ms":0,"agctl_pid":1,"event":"write","target":"live","from_digest8":"cafebabe","to_digest8":"deadbeef","outcome":"applied","direction":"undo"}`
	entry, err := decodeAuditLine([]byte(older))
	if err != nil {
		t.Fatalf("an older undo line still parses: %v", err)
	}
	event := entry.Event.(*WriteEvent)
	if event.Direction != DirectionUndo || event.IncomingIdentity != nil {
		t.Errorf("direction %q, identity %+v", event.Direction, event.IncomingIdentity)
	}
}

func TestAuditAnEntryRoundTripsThroughTheLog(t *testing.T) {
	paths := newAuditStore(t)
	entry := NewAuditEntry(writeEvent("aabbccdd", ptr("11223344")))

	id, err := AuditAppend(t.Context(), paths, entry)
	if err != nil {
		t.Fatalf("the log should be appendable: %v", err)
	}
	if diff := gocmp.Diff(entry.ID(), id); diff != "" {
		t.Errorf("id mismatch (-want +got):\n%s", diff)
	}
	if id.PID != uint32(os.Getpid()) {
		t.Errorf("the id names this process: %d", id.PID)
	}

	read, err := TailAuditLog(paths, 10)
	if err != nil {
		t.Fatalf("the log should be readable: %v", err)
	}
	if diff := gocmp.Diff([]AuditEntry{entry}, read.Entries); diff != "" {
		t.Errorf("entries mismatch (-want +got):\n%s", diff)
	}
	if len(read.Unreadable) != 0 {
		t.Errorf("nothing was damaged: %+v", read.Unreadable)
	}
}

func TestAuditABreakRecordRoundTripsThroughTheLog(t *testing.T) {
	paths := newAuditStore(t)
	entry := NewAuditEntry(breakRecord(t))
	if _, err := AuditAppend(t.Context(), paths, entry); err != nil {
		t.Fatalf("the log should be appendable: %v", err)
	}
	read, err := TailAuditLog(paths, 1)
	if err != nil {
		t.Fatalf("readable: %v", err)
	}
	if diff := gocmp.Diff([]AuditEntry{entry}, read.Entries); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestAuditACleanBreakRecordsNoReason(t *testing.T) {
	// Every break reason is a reason not to have broken a lock, so a break
	// with nothing to explain writes no reason key rather than inventing a
	// word for success.
	record := breakRecord(t)
	record.Reason = ""
	entry := NewAuditEntry(record)
	line, err := auditEntryLine(entry)
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	value := lineValue(t, line)
	if _, present := value["reason"]; present {
		t.Errorf("no reason key on a clean break: %s", line)
	}
	if got := value["outcome"]; got != "broken" {
		t.Errorf("outcome = %v", got)
	}
	if len(value) != 16 {
		t.Errorf("one key fewer than the fully populated shape: %d keys", len(value))
	}

	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, entry); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	read, err := TailAuditLog(paths, 1)
	if err != nil {
		t.Fatalf("readable: %v", err)
	}
	if diff := gocmp.Diff([]AuditEntry{entry}, read.Entries); diff != "" {
		t.Errorf("and it round-trips (-want +got):\n%s", diff)
	}
}

func TestAuditTheLogIs0600WithOneLinePerEntry(t *testing.T) {
	paths := newAuditStore(t)
	for _, digest := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent(digest, nil))); err != nil {
			t.Fatalf("appendable: %v", err)
		}
	}

	shown := AuditLogPath(paths)
	info, err := os.Stat(shown)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the log holds a machine's swap history: mode = %04o", mode)
	}
	text := readText(t, shown)
	if got := strings.Count(text, "\n"); got != 3 {
		t.Errorf("three lines, got %d:\n%s", got, text)
	}
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("every entry is a whole line: %q", text)
	}
}

func TestAuditTailReturnsTheLastEntriesOldestFirst(t *testing.T) {
	paths := newAuditStore(t)
	for _, digest := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent(digest, nil))); err != nil {
			t.Fatalf("appendable: %v", err)
		}
	}

	lastTwo, err := TailAuditLog(paths, 2)
	if err != nil {
		t.Fatalf("readable: %v", err)
	}
	var digests []string
	for _, entry := range lastTwo.Entries {
		digests = append(digests, entry.Event.(*WriteEvent).ToDigest8)
	}
	if diff := gocmp.Diff([]string{"bbbbbbbb", "cccccccc"}, digests); diff != "" {
		t.Errorf("tail order (-want +got):\n%s", diff)
	}
	all, err := TailAuditLog(paths, 99)
	if err != nil || len(all.Entries) != 3 {
		t.Errorf("asking for more is not an error: %d, %v", len(all.Entries), err)
	}

	absent, err := TailAuditLog(newAuditStore(t), 5)
	if err != nil || len(absent.Entries) != 0 || len(absent.Unreadable) != 0 {
		t.Errorf("an absent log is no entries: %+v, %v", absent, err)
	}
}

func TestAuditTailNamesTheLineItCannotReadAndReturnsTheRest(t *testing.T) {
	// One damaged line must not take the report down: the entries that did
	// parse come back and the bad line is named by number.
	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aaaaaaaa", nil))); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	shown := AuditLogPath(paths)
	text := readText(t, shown) + `{"ts":"not a timestamp"}` + "\n"
	if err := os.WriteFile(shown, []byte(text), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	read, err := TailAuditLog(paths, 5)
	if err != nil {
		t.Fatalf("a corrupt line is skipped, not fatal: %v", err)
	}
	if len(read.Entries) != 1 || read.Entries[0].Event.(*WriteEvent).ToDigest8 != "aaaaaaaa" {
		t.Errorf("the good entry is still returned: %+v", read.Entries)
	}
	if len(read.Unreadable) != 1 || read.Unreadable[0].Line != 2 || read.Unreadable[0].Reason == "" {
		t.Errorf("the bad one is named by line number with a reason: %+v", read.Unreadable)
	}
}

func TestAuditATruncatedLastLineDoesNotHideTheEntriesBeforeIt(t *testing.T) {
	// The append is one write plus an fsync, so a process killed between
	// them leaves the final line half-written — by construction inside the
	// last n, so a tail that failed on it would fail on every read after
	// such a crash, exactly when doctor and undo are needed.
	paths := newAuditStore(t)
	for _, digest := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent(digest, nil))); err != nil {
			t.Fatalf("appendable: %v", err)
		}
	}
	shown := AuditLogPath(paths)
	lines := strings.Split(strings.TrimSuffix(readText(t, shown), "\n"), "\n")
	cut := lines[0] + "\n" + lines[1][:10]
	if err := os.WriteFile(shown, []byte(cut), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	read, err := TailAuditLog(paths, 5)
	if err != nil {
		t.Fatalf("a truncated tail is not fatal: %v", err)
	}
	if len(read.Entries) != 1 {
		t.Errorf("the earlier entry survives: %+v", read.Entries)
	}
	if len(read.Unreadable) != 1 || read.Unreadable[0].Line != 2 {
		t.Errorf("and the truncated one is named: %+v", read.Unreadable)
	}
}

func TestAuditAnEntryWithMembersThisBuildDoesNotKnowStillReads(t *testing.T) {
	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aaaaaaaa", nil))); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	shown := AuditLogPath(paths)
	widened := strings.Replace(strings.TrimSuffix(readText(t, shown), "\n"), "}", `,"something_new":true}`, 1)
	if err := os.WriteFile(shown, []byte(widened+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	read, err := TailAuditLog(paths, 1)
	if err != nil {
		t.Fatalf("an unknown member is not a failure: %v", err)
	}
	if len(read.Entries) != 1 || len(read.Unreadable) != 0 {
		t.Errorf("an unknown member is not damage either: %+v", read)
	}
}

func TestAuditNoLineCarriesTokenMaterial(t *testing.T) {
	// Asserted on the raw bytes rather than on the typed value, because
	// the file is the artefact the redaction rule is about. A digest is 64
	// hex digits and a hex-encoded blob is thousands, so nothing that big
	// may appear — while a nanosecond mtime, the longest legitimate run at
	// 19 digits, must still pass.
	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", ptr("11223344")))); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(breakRecord(t))); err != nil {
		t.Fatalf("appendable: %v", err)
	}

	text := readText(t, AuditLogPath(paths))
	for _, marker := range []string{"sk-ant-", "claudeAiOauth", "accessToken"} {
		if strings.Contains(text, marker) {
			t.Errorf("`%s` must not appear: %s", marker, text)
		}
	}
	longest := 0
	run := 0
	for _, b := range []byte(text) {
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	if longest > 24 {
		t.Errorf("a %d-digit hex run is too long to be a prefix: %s", longest, text)
	}
}

func TestAuditAppendRefusesADigestThatIsNotAPrefix(t *testing.T) {
	// The redaction guard: plant a token-shaped value and a whole digest
	// in the digest fields and both must fail the append, creating no log.
	paths := newAuditStore(t)
	whole := strings.Repeat("aabbccdd", 8)
	tests := map[string]*WriteEvent{
		"error: a whole digest as to_digest8":   writeEvent(whole, nil),
		"error: a whole digest as from_digest8": writeEvent("aabbccdd", ptr(whole)),
		"error: an uppercase prefix":            writeEvent("AABBCCDD", nil),
		"error: a short prefix":                 writeEvent("aabbccd", nil),
		"error: a planted token-shaped value":   writeEvent("sk-ant-oat01-whatever", nil),
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := AuditAppend(t.Context(), paths, NewAuditEntry(event))
			if err == nil {
				t.Fatalf("the entry must be refused")
			}
			if !strings.Contains(err.Error(), "digest prefixes only") {
				t.Errorf("the refusal names the rule: %v", err)
			}
		})
	}
	if _, err := os.Lstat(AuditLogPath(paths)); !os.IsNotExist(err) {
		t.Errorf("a refused entry creates no log, lstat err = %v", err)
	}
}

func TestAuditABreakRecordSerialisesWithTheFixedFieldNames(t *testing.T) {
	line, err := auditEntryLine(NewAuditEntry(breakRecord(t)))
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	want := []string{
		"ts", "monotonic_ms", "agctl_pid", "event",
		"path", "store_dir", "tree", "service", "target",
		"sample_a", "sample_b", "sample_c",
		"interval_wall_ms", "interval_monotonic_ms",
		"holder_evidence", "outcome", "reason",
	}
	if diff := gocmp.Diff(want, topLevelKeys(t, line)); diff != "" {
		t.Errorf("the readers depend on this shape (-want +got):\n%s", diff)
	}
	value := lineValue(t, line)
	for member, token := range map[string]string{"event": "lock_break", "tree": "live", "target": "live", "holder_evidence": "no_stopped_claude", "outcome": "broken", "reason": "retaken"} {
		if got := value[member]; got != token {
			t.Errorf("%s = %v, want %s", member, got, token)
		}
	}
	sample, ok := value["sample_a"].(map[string]any)
	if !ok || len(sample) != 3 {
		t.Fatalf("sample_a shape: %v", value["sample_a"])
	}
	for _, key := range []string{"at", "mtime_ns", "age_ms"} {
		if _, present := sample[key]; !present {
			t.Errorf("sample_a misses %s", key)
		}
	}
}

func TestAuditAWriteSerialisesWithTheDocumentedFieldNames(t *testing.T) {
	line, err := auditEntryLine(NewAuditEntry(writeEvent("aabbccdd", ptr("11223344"))))
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	want := []string{"ts", "monotonic_ms", "agctl_pid", "event", "target", "from_digest8", "to_digest8", "outcome", "direction"}
	if diff := gocmp.Diff(want, topLevelKeys(t, line)); diff != "" {
		t.Errorf("field names (-want +got):\n%s", diff)
	}
	value := lineValue(t, line)
	for member, token := range map[string]string{"event": "write", "target": "namespace:0123abcd", "outcome": "applied", "from_digest8": "11223344"} {
		if got := value[member]; got != token {
			t.Errorf("%s = %v, want %s", member, got, token)
		}
	}
}

func TestAuditTheBytesOnDiskAreTheEntryAndOneNewline(t *testing.T) {
	// Byte-exact emission, pinned with a fixed clock: reaching the log
	// through a walk must not change a byte of what lands in it.
	fixed := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	auditNow = func() time.Time { return fixed }
	t.Cleanup(func() { auditNow = time.Now })

	paths := newAuditStoreWithRoot(t)
	entry := NewAuditEntry(writeEvent("aabbccdd", ptr("11223344")))
	entry.MonotonicMS = 0
	entry.PID = 1
	line, err := auditEntryLine(entry)
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	want := `{"ts":"2026-09-10T00:00:00Z","monotonic_ms":0,"agctl_pid":1,"event":"write","target":"namespace:0123abcd","from_digest8":"11223344","to_digest8":"aabbccdd","outcome":"applied","direction":"forward"}` + "\n"
	if diff := gocmp.Diff(want, line); diff != "" {
		t.Fatalf("line bytes (-want +got):\n%s", diff)
	}

	shown := AuditLogPath(paths)
	file, err := OpenAuditLog(paths, shown)
	if err != nil {
		t.Fatalf("a healthy log opens: %v", err)
	}
	if _, err := AuditAppendThrough(file, shown, entry); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	_ = file.Close()
	if diff := gocmp.Diff(want, readText(t, shown)); diff != "" {
		t.Errorf("bytes on disk (-want +got):\n%s", diff)
	}
	if got := AuditLogStateOf(paths); got.Kind != AuditLogPresent {
		t.Errorf("state = %+v", got)
	}
}

func TestAuditAFirstWriteRecordsItselfAsOne(t *testing.T) {
	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil))); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	read, err := TailAuditLog(paths, 1)
	if err != nil {
		t.Fatalf("readable: %v", err)
	}
	if got := read.Entries[0].Event.(*WriteEvent).FromDigest8; got != nil {
		t.Errorf("no outgoing credential means the item was absent: %v", *got)
	}
}

func TestAuditTheVocabularySerialisesToTheDocumentedTokens(t *testing.T) {
	// The holder-evidence values are the whole vocabulary, and no value
	// names a pid or claims a store was identified — asserted on the
	// vocabulary itself so a later edit cannot reintroduce attribution.
	evidence := map[HolderEvidence]string{
		EvidenceStoppedClaudePresent: "stopped_claude_present",
		EvidenceNoStoppedClaude:      "no_stopped_claude",
		EvidenceUnreadable:           "unreadable",
		EvidenceNone:                 "none",
	}
	for value, token := range evidence {
		if string(value) != token {
			t.Errorf("evidence token %q", value)
		}
		if strings.ContainsAny(token, "0123456789") || strings.Contains(token, "pid") {
			t.Errorf("no holder-evidence value may carry a number or a pid: %q", token)
		}
	}
	for value, token := range map[Tree]string{TreeOwn: "agctl", TreeLive: "live"} {
		if string(value) != token {
			t.Errorf("tree token %q, want %q", value, token)
		}
	}
	for value, token := range map[BreakOutcome]string{OutcomeBroken: "broken", OutcomeAbandoned: "abandoned"} {
		if string(value) != token {
			t.Errorf("break outcome token %q, want %q", value, token)
		}
	}
	reasons := map[BreakReason]string{
		ReasonHeartbeatObserved: "heartbeat_observed",
		ReasonTooYoung:          "too_young",
		ReasonVanished:          "vanished",
		ReasonClockJump:         "clock_jump",
		ReasonRetaken:           "retaken",
		ReasonHolderStopped:     "holder_stopped",
	}
	for value, token := range reasons {
		if string(value) != token {
			t.Errorf("break reason token %q, want %q", value, token)
		}
	}
	for value, token := range map[KeychainWriteOutcome]string{WriteApplied: "applied", WriteUnknown: "unknown", WriteFailed: "failed", WriteDiscarded: "discarded"} {
		if string(value) != token {
			t.Errorf("write outcome token %q, want %q", value, token)
		}
	}
	if string(TargetLive) != "live" || string(NamespaceTarget("0123abcd")) != "namespace:0123abcd" {
		t.Errorf("target tokens: %q, %q", TargetLive, NamespaceTarget("0123abcd"))
	}

	// A reason meaning "it was stale" does not exist: staleness is the
	// precondition of the break rule, not an outcome of it. The decoder
	// refuses the word, like an unknown target.
	record := breakRecord(t)
	line, err := auditEntryLine(NewAuditEntry(record))
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	stale := strings.Replace(line, `"reason":"retaken"`, `"reason":"stale"`, 1)
	if _, err := decodeAuditLine([]byte(strings.TrimSuffix(stale, "\n"))); err == nil {
		t.Errorf("the vocabulary is exactly the fixed words; `stale` is not one")
	}
	badTarget := strings.Replace(line, `"target":"live"`, `"target":"something-else"`, 1)
	if _, err := decodeAuditLine([]byte(strings.TrimSuffix(badTarget, "\n"))); err == nil {
		t.Errorf("an unknown target is not silently accepted")
	}
}

func TestAuditDigest8TakesAPrefixAndRefusesAnythingElse(t *testing.T) {
	tests := map[string]struct {
		digest string
		want   string
		ok     bool
	}{
		"success: a whole digest yields its prefix": {digest: strings.Repeat("aabbccdd", 8), want: "aabbccdd", ok: true},
		"success: a bare prefix passes":             {digest: "aabbccdd", want: "aabbccdd", ok: true},
		"error: too short to be a prefix":           {digest: "aabbccd"},
		"error: hex is lowercase here":              {digest: "AABBCCDDEE"},
		"error: not a digest at all":                {digest: "sk-ant-oat01-whatever"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := Digest8(tt.digest)
			if ok != tt.ok || got != tt.want {
				t.Errorf("Digest8(%q) = %q, %v", tt.digest, got, ok)
			}
		})
	}
}

func TestAuditAnIDNamesTheEntryAndThisProcess(t *testing.T) {
	entry := NewAuditEntry(writeEvent("aabbccdd", nil))
	rendered := entry.ID().String()
	if !strings.Contains(rendered, entry.TS) {
		t.Errorf("the id carries the timestamp: %s", rendered)
	}
	if !strings.HasSuffix(rendered, fmt.Sprintf("#%d", os.Getpid())) {
		t.Errorf("and this process's id: %s", rendered)
	}
}

func TestAuditTheLogLivesBesideTheNamespaces(t *testing.T) {
	paths := newAuditStore(t)
	want := filepath.Join(paths.NamespaceRoot(), "keychain-writes.jsonl")
	if got := AuditLogPath(paths); got != want {
		t.Errorf("log path = %s, want %s", got, want)
	}
}

func TestAuditAppendRefusesASymlinkPlantedAtTheLog(t *testing.T) {
	paths := newAuditStoreWithRoot(t)

	elsewhere := filepath.Join(filepath.Dir(paths.ConfigDir()), "somebody-elses.jsonl")
	if err := os.WriteFile(elsewhere, []byte("planted\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 0600, deliberately: at a wider mode the mode check would refuse a
	// followed link too, and this test would pass while O_NOFOLLOW was
	// gone. At 0600 the only thing standing between the append and the
	// target is the flag.
	if err := os.Chmod(elsewhere, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Symlink(elsewhere, AuditLogPath(paths)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil)))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symbolic link at the log is refused: %v", err)
	}
	if strings.Contains(err.Error(), "mode") {
		t.Errorf("refused as a link, not for its mode: %v", err)
	}
	if got := readText(t, elsewhere); got != "planted\n" {
		t.Errorf("the link's target is not appended to: %q", got)
	}
	info, statErr := os.Lstat(AuditLogPath(paths))
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("what is refused is also left alone: nothing repaired or replaced")
	}

	// The reader refuses the same plant: an undo acts on what a tail
	// returns, so a log somebody else can redirect is an undo somebody
	// else can direct.
	if _, err := TailAuditLog(paths, 10); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("the reader refuses what the writer refuses: %v", err)
	}
}

func TestAuditAppendRefusesAFIFOPlantedAtTheLog(t *testing.T) {
	// The other one-command plant, and the one worse than a redirect: an
	// open for writing blocks on a FIFO until a reader arrives, and the
	// append runs with the namespace lock held, so without O_NONBLOCK the
	// store wedges rather than one entry failing.
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	if err := unix.Mkfifo(shown, 0o600); err != nil {
		t.Fatalf("the FIFO should be plantable: %v", err)
	}

	// On a helper goroutine with a deadline, so a regression to a blocking
	// flag set reads as a failed test rather than a suite that never
	// finishes.
	result := make(chan error, 1)
	go func() {
		_, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil)))
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatalf("a FIFO at the log's name is refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the append must return rather than block on the FIFO: O_NONBLOCK is missing")
	}

	info, err := os.Lstat(shown)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("what is refused is left alone: still a FIFO, neither unlinked nor replaced")
	}
}

func TestAuditAppendRefusesALogWhoseModeIsNot0600AndDoesNotRepairIt(t *testing.T) {
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	if err := os.WriteFile(shown, []byte(`{"already":"here"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(shown, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil)))
	if err == nil || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("the refusal names the mode it found: %v", err)
	}
	info, statErr := os.Stat(shown)
	if statErr != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("refused, never repaired: evidence is not chmodded")
	}
	if got := readText(t, shown); got != `{"already":"here"}`+"\n" {
		t.Errorf("and nothing was appended to it: %q", got)
	}
}

func TestAuditAppendRefusesASymlinkPlantedAtTheNamespaceRoot(t *testing.T) {
	paths := newAuditStoreWithRoot(t)
	elsewhere := filepath.Join(filepath.Dir(paths.ConfigDir()), "somebody-elses-store")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.RemoveAll(paths.NamespaceRoot()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(elsewhere, paths.NamespaceRoot()); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil)))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked namespace root is refused: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(elsewhere, AuditLogFile)); !os.IsNotExist(statErr) {
		t.Errorf("no log was created in the directory the link pointed at")
	}
	if _, err := TailAuditLog(paths, 10); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("and the reader refuses the same store: %v", err)
	}
}

func TestAuditTheStateDoctorPrintsIsTheOneAppendActsOn(t *testing.T) {
	// The drift guard: doctor's row and the append's decision come from
	// one walk and one vocabulary, so a later edit that taught only one of
	// them about a shape fails here.
	tests := map[string]struct {
		plant      func(t *testing.T, shown string)
		appendable bool
	}{
		"success: nothing at all": {
			plant:      func(*testing.T, string) {},
			appendable: true,
		},
		"success: a plain 0600 log": {
			plant: func(t *testing.T, shown string) {
				if err := os.WriteFile(shown, nil, 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			appendable: true,
		},
		"error: a world-readable log": {
			plant: func(t *testing.T, shown string) {
				if err := os.WriteFile(shown, nil, 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := os.Chmod(shown, 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
		},
		"error: a symbolic link": {
			plant: func(t *testing.T, shown string) {
				target := filepath.Join(filepath.Dir(shown), "somebody-elses.jsonl")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if err := os.Symlink(target, shown); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
		"error: a directory": {
			plant: func(t *testing.T, shown string) {
				if err := os.Mkdir(shown, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			paths := newAuditStoreWithRoot(t)
			tt.plant(t, AuditLogPath(paths))

			state := AuditLogStateOf(paths)
			_, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aabbccdd", nil)))
			if state.IsAppendable() != (err == nil) {
				t.Fatalf("doctor says `%s` and the append err = %v", state.Note(), err)
			}
			if tt.appendable != state.IsAppendable() {
				t.Errorf("appendable = %v, want %v", state.IsAppendable(), tt.appendable)
			}
			if err != nil && !strings.Contains(err.Error(), state.Note()) {
				t.Errorf("the refusal carries the sentence doctor prints: %v vs %s", err, state.Note())
			}
		})
	}
}

func TestAuditAppendThroughWritesToTheLogTheGateOpenedNotToWhatItsNameBecame(t *testing.T) {
	// Between the gate's open and the append after the write, whoever can
	// write the namespace root can rename the log away and plant a link at
	// its name. An append by name meets the link and refuses — which
	// proves the name really is hostile — while the append through the
	// held descriptor still lands in the file the gate opened.
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	held, err := OpenAuditLog(paths, shown)
	if err != nil {
		t.Fatalf("a healthy log opens: %v", err)
	}
	defer func() { _ = held.Close() }()

	base := filepath.Dir(paths.ConfigDir())
	moved := filepath.Join(base, "moved-aside.jsonl")
	if err := os.Rename(shown, moved); err != nil {
		t.Fatalf("the log can be renamed away under the descriptor: %v", err)
	}
	elsewhere := filepath.Join(base, "somebody-elses.jsonl")
	if err := os.WriteFile(elsewhere, []byte("planted\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(elsewhere, shown); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	entry := NewAuditEntry(writeEvent("aabbccdd", ptr("11223344")))
	if _, err := AuditAppend(t.Context(), paths, entry); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("an append by name meets the planted link: %v", err)
	}

	id, err := AuditAppendThrough(held, shown, entry)
	if err != nil {
		t.Fatalf("the held descriptor is still the log: %v", err)
	}
	if diff := gocmp.Diff(entry.ID(), id); diff != "" {
		t.Errorf("the id names the entry that landed (-want +got):\n%s", diff)
	}
	line, err := auditEntryLine(entry)
	if err != nil {
		t.Fatalf("serializable: %v", err)
	}
	if got := readText(t, moved); got != line {
		t.Errorf("exactly one line, in the file the gate opened: %q", got)
	}
	if got := readText(t, elsewhere); got != "planted\n" {
		t.Errorf("and not a byte through the planted name: %q", got)
	}
}

func TestAuditAppendThroughRefusesWhatAppendRefusesBeforeWritingAByte(t *testing.T) {
	// One serialisation and one digest check behind both entry points, so
	// an entry one refuses cannot reach the log through the other.
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	held, err := OpenAuditLog(paths, shown)
	if err != nil {
		t.Fatalf("a healthy log opens: %v", err)
	}
	defer func() { _ = held.Close() }()
	whole := strings.Repeat("aabbccdd", 8)

	for _, event := range []*WriteEvent{writeEvent(whole, nil), writeEvent("aabbccdd", ptr(whole))} {
		_, err := AuditAppendThrough(held, shown, NewAuditEntry(event))
		if err == nil || !strings.Contains(err.Error(), "digest prefixes only") {
			t.Errorf("a whole digest is not a digest prefix: %v", err)
		}
	}
	if got := readText(t, shown); got != "" {
		t.Errorf("nothing was written: %q", got)
	}
}

// configRecord is a config step's record with every member populated.
func configRecord(outcome ConfigOutcome, reason *ConfigReason) *ConfigWriteRecord {
	record := &ConfigWriteRecord{
		After:    ptr("2026-09-14T00:00:00Z#4242"),
		Outcome:  outcome,
		Reason:   reason,
		Account:  &IncomingIdentity{AccountUUID: "acct-t", OrganizationUUID: ptr("org-t")},
		FromSHA8: ptr("0123abcd"),
		Backup:   ptr(".claude.json.backup.1789000000000"),
		HoldMS:   new(uint64),
	}
	*record.HoldMS = 12
	if outcome == ConfigApplied {
		record.ToSHA8 = ptr("89abcdef")
	}
	return record
}

//go:fix inline
func reasonPtr(reason ConfigReason) *ConfigReason { return new(reason) }

func TestAuditConfigWriteCarriesIDsOnlyAndParsesOldLines(t *testing.T) {
	// The pre-existing lines — a write and a lock break — read back to the
	// entries they were, before and after a config_write line joins them.
	paths := newAuditStore(t)
	oldWrite := NewAuditEntry(writeEvent("aabbccdd", ptr("11223344")))
	oldBreak := NewAuditEntry(breakRecord(t))
	for _, entry := range []AuditEntry{oldWrite, oldBreak} {
		if _, err := AuditAppend(t.Context(), paths, entry); err != nil {
			t.Fatalf("appendable: %v", err)
		}
	}
	before, err := TailAuditLog(paths, 1<<30)
	if err != nil || len(before.Unreadable) != 0 {
		t.Fatalf("readable: %+v, %v", before.Unreadable, err)
	}
	if diff := gocmp.Diff([]AuditEntry{oldWrite, oldBreak}, before.Entries); diff != "" {
		t.Fatalf("the old lines (-want +got):\n%s", diff)
	}

	configEntry := NewAuditEntry(configRecord(ConfigApplied, nil))
	if _, err := AuditAppend(t.Context(), paths, configEntry); err != nil {
		t.Fatalf("a config_write entry is appendable: %v", err)
	}

	text := readText(t, AuditLogPath(paths))
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("one line per entry: %s", text)
	}
	line := lines[2]
	for _, want := range []string{`"event":"config_write"`, `"outcome":"applied"`, `"account_uuid":"acct-t"`} {
		if !strings.Contains(line, want) {
			t.Errorf("the line carries %s: %s", want, line)
		}
	}
	if strings.Contains(line, "@") || strings.Contains(line, "email") {
		t.Errorf("no email address or member can be in the line: %s", line)
	}
	if strings.Contains(line, "/") {
		t.Errorf("no path: the backup is a file name: %s", line)
	}

	after, err := TailAuditLog(paths, 1<<30)
	if err != nil || len(after.Unreadable) != 0 {
		t.Fatalf("readable: %+v, %v", after.Unreadable, err)
	}
	if diff := gocmp.Diff([]AuditEntry{oldWrite, oldBreak, configEntry}, after.Entries); diff != "" {
		t.Errorf("the round trip, old lines unchanged (-want +got):\n%s", diff)
	}

	// Every word of the vocabulary survives the trip.
	words := []struct {
		outcome     ConfigOutcome
		reason      ConfigReason
		outcomeWord string
		reasonWord  string
	}{
		{ConfigSkipped, ConfigReasonAbsent, "skipped", "absent"},
		{ConfigSkipped, ConfigReasonLockBusy, "skipped", "lock_busy"},
		{ConfigSkipped, ConfigReasonLockStale, "skipped", "lock_stale"},
		{ConfigSkipped, ConfigReasonCancelled, "skipped", "cancelled"},
		{ConfigRefused, ConfigReasonUnreadable, "refused", "unreadable"},
		{ConfigRefused, ConfigReasonUnparseable, "refused", "unparseable"},
		{ConfigRefused, ConfigReasonNotAnObject, "refused", "not_an_object"},
		{ConfigRefused, ConfigReasonNotReproducible, "refused", "not_reproducible"},
		{ConfigRefused, ConfigReasonBackupUnwritable, "refused", "backup_unwritable"},
		{ConfigAborted, ConfigReasonChangedUnderLock, "aborted", "changed_under_lock"},
		{ConfigAborted, ConfigReasonCompromised, "aborted", "compromised"},
		{ConfigAborted, ConfigReasonBudget, "aborted", "budget"},
		{ConfigFailed, ConfigReasonIO, "failed", "io"},
		{ConfigNotAttempted, ConfigReasonProfileUnavailable, "not_attempted", "profile_unavailable"},
		{ConfigNotAttempted, ConfigReasonSwapUnknown, "not_attempted", "swap_unknown"},
		{ConfigSkipped, ConfigReasonAlreadyCurrent, "skipped", "already_current"},
		{ConfigNotAttempted, ConfigReasonDeclined, "not_attempted", "declined"},
		{ConfigRefused, ConfigReasonAuditRefused, "refused", "audit_refused"},
	}
	if len(words) != 18 {
		t.Fatalf("every reason but unrecognized, one row each: %d", len(words))
	}
	for _, row := range words {
		entry := NewAuditEntry(configRecord(row.outcome, new(row.reason)))
		line, err := auditEntryLine(entry)
		if err != nil {
			t.Fatalf("%s/%s serialises: %v", row.outcomeWord, row.reasonWord, err)
		}
		for _, want := range []string{fmt.Sprintf(`"outcome":"%s"`, row.outcomeWord), fmt.Sprintf(`"reason":"%s"`, row.reasonWord)} {
			if !strings.Contains(line, want) {
				t.Errorf("the line carries %s: %s", want, line)
			}
		}
		back, err := decodeAuditLine([]byte(strings.TrimSuffix(line, "\n")))
		if err != nil {
			t.Fatalf("%s/%s reads back: %v", row.outcomeWord, row.reasonWord, err)
		}
		if diff := gocmp.Diff(entry, back); diff != "" {
			t.Errorf("%s/%s (-want +got):\n%s", row.outcomeWord, row.reasonWord, diff)
		}
	}
}

func TestAuditAConfigWriteWithAWholeDigestOrAPathIsRefused(t *testing.T) {
	paths := newAuditStore(t)
	whole := strings.Repeat("aabbccdd", 8)
	applied := func() *ConfigWriteRecord { return configRecord(ConfigApplied, nil) }

	tests := map[string]func() *ConfigWriteRecord{
		"error: a 64-hex from_sha8": func() *ConfigWriteRecord {
			record := applied()
			record.FromSHA8 = &whole
			return record
		},
		"error: a 64-hex to_sha8": func() *ConfigWriteRecord {
			record := applied()
			record.ToSHA8 = &whole
			return record
		},
		"error: an uppercase prefix": func() *ConfigWriteRecord {
			record := applied()
			record.FromSHA8 = ptr("AABBCCDD")
			return record
		},
		"error: a backup path": func() *ConfigWriteRecord {
			record := applied()
			record.Backup = ptr("/x/.claude.json.backup.1")
			return record
		},
		"error: a backup name in another shape": func() *ConfigWriteRecord {
			record := applied()
			record.Backup = ptr(".claude.json.corrupted.1")
			return record
		},
		"error: a backup name with no stamp": func() *ConfigWriteRecord {
			record := applied()
			record.Backup = ptr(".claude.json.backup.")
			return record
		},
		"error: a read-only outcome word": func() *ConfigWriteRecord {
			record := applied()
			record.Outcome = ConfigUnrecognized
			return record
		},
		"error: a read-only reason word": func() *ConfigWriteRecord {
			record := applied()
			record.Reason = new(ConfigReasonUnrecognized)
			return record
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(build())); err == nil {
				t.Fatalf("the entry must be refused")
			}
			if _, err := os.Lstat(AuditLogPath(paths)); !os.IsNotExist(err) {
				t.Errorf("a refused entry creates no log")
			}
		})
	}

	withPath := applied()
	withPath.Backup = ptr("/x/.claude.json.backup.1")
	_, err := AuditAppend(t.Context(), paths, NewAuditEntry(withPath))
	if err == nil || strings.Contains(err.Error(), "/x/") {
		t.Errorf("the refusal does not repeat the path: %v", err)
	}

	// Through the held descriptor too, and nothing lands in the log it
	// opened.
	rooted := newAuditStoreWithRoot(t)
	shown := AuditLogPath(rooted)
	held, err := OpenAuditLog(rooted, shown)
	if err != nil {
		t.Fatalf("a healthy log opens: %v", err)
	}
	defer func() { _ = held.Close() }()
	wholeDigest := applied()
	wholeDigest.FromSHA8 = &whole
	for _, record := range []*ConfigWriteRecord{wholeDigest, withPath} {
		if _, err := AuditAppendThrough(held, shown, NewAuditEntry(record)); err == nil {
			t.Errorf("the held descriptor applies the same checks")
		}
	}
	if got := readText(t, shown); got != "" {
		t.Errorf("nothing was written: %q", got)
	}

	// The shapes the step does write are accepted.
	for _, name := range []string{".claude.json.backup.1789000000000", ".config.json.backup.1"} {
		record := applied()
		record.Backup = &name
		if _, err := auditEntryLine(NewAuditEntry(record)); err != nil {
			t.Errorf("%s is a backup name: %v", name, err)
		}
	}
}

func TestAuditAnUnrecognisedConfigOutcomeOrReasonParsesAsUnrecognized(t *testing.T) {
	// A later build's outcome or reason word reads as unrecognized and the
	// line stays in the entries, so an undo that refuses on unreadable
	// lines is not blocked by it.
	paths := newAuditStore(t)
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(writeEvent("aaaaaaaa", nil))); err != nil {
		t.Fatalf("appendable: %v", err)
	}
	shown := AuditLogPath(paths)
	text := readText(t, shown) + `{"ts":"2026-09-14T00:00:00Z","monotonic_ms":1,"agctl_pid":2,"event":"config_write","after":null,"outcome":"future","reason":"future"}` + "\n"
	if err := os.WriteFile(shown, []byte(text), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	read, err := TailAuditLog(paths, 1<<30)
	if err != nil || len(read.Unreadable) != 0 {
		t.Fatalf("the line is in the entries: %+v, %v", read.Unreadable, err)
	}
	if len(read.Entries) != 2 {
		t.Fatalf("two entries: %+v", read.Entries)
	}
	record, ok := read.Entries[1].Event.(*ConfigWriteRecord)
	if !ok {
		t.Fatalf("a config_write entry: %T", read.Entries[1].Event)
	}
	if record.Outcome != ConfigUnrecognized || record.Reason == nil || *record.Reason != ConfigReasonUnrecognized {
		t.Errorf("unknown words read as unrecognized: %+v", record)
	}
	if record.Backup != nil {
		t.Errorf("absent members read as absent: %v", *record.Backup)
	}
}

func TestAuditAnUnrecognisedEventKindLandsInEntriesNotUnreadable(t *testing.T) {
	// A later build's additive event kind — with members of its own —
	// reads as unrecognized, so the next additive kind does not become a
	// downgrade hazard for older builds.
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	planted := `{"ts":"2026-09-14T00:00:00Z","monotonic_ms":7,"agctl_pid":42,"event":"future_kind","x":1}` + "\n"
	if err := os.WriteFile(shown, []byte(planted), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	read, err := TailAuditLog(paths, 1<<30)
	if err != nil || len(read.Unreadable) != 0 {
		t.Fatalf("unreadable is empty: %+v, %v", read.Unreadable, err)
	}
	if len(read.Entries) != 1 {
		t.Fatalf("one entry: %+v", read.Entries)
	}
	if _, ok := read.Entries[0].Event.(*UnrecognizedEvent); !ok {
		t.Fatalf("an unrecognized event: %T", read.Entries[0].Event)
	}
	if read.Entries[0].MonotonicMS != 7 || read.Entries[0].PID != 42 {
		t.Errorf("provenance kept: %+v", read.Entries[0])
	}

	// Read-only: the append refuses to write it, and the log is unchanged.
	if _, err := AuditAppend(t.Context(), paths, NewAuditEntry(&UnrecognizedEvent{})); err == nil {
		t.Fatalf("an unrecognised event is never written")
	}
	if got := readText(t, shown); got != planted {
		t.Errorf("the log is unchanged: %q", got)
	}
}

func TestAuditOpenLogAtAppendsAnotherProvidersLogUnderTheSameRules(t *testing.T) {
	paths := newAuditStore(t)
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatalf("the sibling tree should be creatable: %v", err)
	}
	root, err := OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if err != nil {
		t.Fatalf("walkable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(root) })
	shown := filepath.Join(paths.CodexRoot(), "writes.jsonl")

	if _, present, err := ReadAuditLogAt(root, "writes.jsonl", shown); err != nil || present {
		t.Fatalf("an absent log reads as absent: %v, %v", present, err)
	}
	for _, line := range []string{`{"n":1}` + "\n", `{"n":2}` + "\n"} {
		file, err := OpenAuditLogAt(root, "writes.jsonl", shown)
		if err != nil {
			t.Fatalf("appendable: %v", err)
		}
		if err := WriteAuditLine(file, shown, line); err != nil {
			t.Fatalf("written: %v", err)
		}
		_ = file.Close()
	}

	info, err := os.Stat(shown)
	if err != nil || info.Mode().Perm() != config.FileMode {
		t.Errorf("created 0600: %v, %v", info, err)
	}
	text, present, err := ReadAuditLogAt(root, "writes.jsonl", shown)
	if err != nil || !present || text != `{"n":1}`+"\n"+`{"n":2}`+"\n" {
		t.Errorf("O_APPEND, one line per call: %q, %v", text, err)
	}
	if _, err := os.Lstat(paths.NamespaceRoot()); !os.IsNotExist(err) {
		t.Errorf("the sibling log creates no claude/ directory")
	}

	// And the one-line-at-a-time reader sees the same file, within bounds.
	var visited []string
	found, err := ForEachAuditLineAt(root, "writes.jsonl", shown, 1024, func(line string) { visited = append(visited, line) })
	if err != nil || !found {
		t.Fatalf("iterable: %v, %v", found, err)
	}
	if diff := gocmp.Diff([]string{`{"n":1}`, `{"n":2}`}, visited); diff != "" {
		t.Errorf("visited lines (-want +got):\n%s", diff)
	}
}

func TestAuditForEachLineSkipsOverlongAndNonUTF8LinesAndKeepsAFinalPartialOne(t *testing.T) {
	paths := newAuditStore(t)
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatalf("creatable: %v", err)
	}
	root, err := OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if err != nil {
		t.Fatalf("walkable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(root) })
	shown := filepath.Join(paths.CodexRoot(), "writes.jsonl")
	body := "short\n" + strings.Repeat("x", 9000) + "\n" + "bad\xff\xfe\n" + "unterminated"
	if err := os.WriteFile(shown, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var visited []string
	found, err := ForEachAuditLineAt(root, "writes.jsonl", shown, 4096, func(line string) { visited = append(visited, line) })
	if err != nil || !found {
		t.Fatalf("iterable: %v, %v", found, err)
	}
	if diff := gocmp.Diff([]string{"short", "unterminated"}, visited); diff != "" {
		t.Errorf("an overlong line and a non-UTF-8 line are skipped, a final partial line is kept (-want +got):\n%s", diff)
	}

	missing, err := ForEachAuditLineAt(root, "absent.jsonl", shown, 4096, func(string) {})
	if err != nil || missing {
		t.Errorf("an absent log reports found=false: %v, %v", missing, err)
	}
}

func TestAuditTheRootedLogPrimitivesRefuseANameThatIsNotOneComponent(t *testing.T) {
	paths := newAuditStore(t)
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatalf("creatable: %v", err)
	}
	root, err := OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if err != nil {
		t.Fatalf("walkable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(root) })
	if err := os.Mkdir(filepath.Join(paths.CodexRoot(), "sub"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	shown := filepath.Join(paths.CodexRoot(), "writes.jsonl")
	for _, name := range []string{"sub/writes.jsonl", "..", ".", "writes.jsonl/", ""} {
		if _, err := OpenAuditLogAt(root, name, shown); err == nil || !strings.Contains(err.Error(), "single path component") {
			t.Errorf("`%s` must be refused for append: %v", name, err)
		}
		if _, _, err := ReadAuditLogAt(root, name, shown); err == nil || !strings.Contains(err.Error(), "single path component") {
			t.Errorf("`%s` must be refused for read: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(paths.CodexRoot(), "sub", "writes.jsonl")); !os.IsNotExist(err) {
		t.Errorf("nothing was created through a nested name")
	}
}

func TestAuditOpenLogAtRefusesWhatOpenLogRefusesAndTheReaderStillReportsAWrongMode(t *testing.T) {
	paths := newAuditStore(t)
	if err := paths.EnsureCodexDirs(t.Context()); err != nil {
		t.Fatalf("creatable: %v", err)
	}
	root, err := OpenDirUnder(paths.ConfigDir(), paths.CodexRoot())
	if err != nil {
		t.Fatalf("walkable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(root) })
	shown := filepath.Join(paths.CodexRoot(), "writes.jsonl")

	elsewhere := filepath.Join(filepath.Dir(paths.ConfigDir()), "somebody-elses.jsonl")
	if err := os.WriteFile(elsewhere, []byte("planted\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(elsewhere, shown); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := OpenAuditLogAt(root, "writes.jsonl", shown); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a link is refused: %v", err)
	}
	if _, _, err := ReadAuditLogAt(root, "writes.jsonl", shown); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("on read as well: %v", err)
	}
	if got := readText(t, elsewhere); got != "planted\n" {
		t.Errorf("the target is untouched: %q", got)
	}

	if err := os.Remove(shown); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(shown, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(shown, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := OpenAuditLogAt(root, "writes.jsonl", shown); err == nil || !strings.Contains(err.Error(), "mode is 0644") {
		t.Fatalf("0644 is refused: %v", err)
	}
	info, err := os.Stat(shown)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("refused, never repaired")
	}
	text, present, err := ReadAuditLogAt(root, "writes.jsonl", shown)
	if err != nil || !present || text != "" {
		t.Errorf("a reader still reports it: %q, %v, %v", text, present, err)
	}
}

func TestAuditOpenLogIsStillTheRootedOpenOfTheMainLog(t *testing.T) {
	paths := newAuditStoreWithRoot(t)
	shown := AuditLogPath(paths)
	file, err := OpenAuditLog(paths, shown)
	if err != nil {
		t.Fatalf("appendable: %v", err)
	}
	if err := WriteAuditLine(file, shown, "{}\n"); err != nil {
		t.Fatalf("written: %v", err)
	}
	_ = file.Close()
	if got := readText(t, shown); got != "{}\n" {
		t.Errorf("one line: %q", got)
	}

	if err := os.Remove(shown); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink("/nonexistent", shown); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err = OpenAuditLog(paths, shown)
	want := fmt.Sprintf("the audit log `%s` is refused: a symbolic link, which agentctl will not append through", shown)
	if err == nil || err.Error() != want {
		t.Errorf("refusal sentence:\n got: %v\nwant: %s", err, want)
	}
}
