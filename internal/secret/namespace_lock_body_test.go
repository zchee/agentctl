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
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

func TestTheBodySerializesToTheStoreFormatByteForByte(t *testing.T) {
	t.Parallel()

	// The on-disk spelling existing stores hold: compact JSON, members in
	// this order, null for an unknown start, no trailing newline.
	start := "2025-09-09T06:40:00.123456Z"
	tests := map[string]struct {
		body LockBody
		want string
	}{
		"success: a holder with a start identity": {
			body: LockBody{PID: 1, PIDStartTime: &start, AcquiredAt: "2026-09-08T00:00:00Z"},
			want: `{"pid":1,"pid_start_time":"2025-09-09T06:40:00.123456Z","acquired_at":"2026-09-08T00:00:00Z"}`,
		},
		"success: an unknown start is null, not omitted": {
			body: LockBody{PID: 7, AcquiredAt: "2026-09-08T00:00:00Z"},
			want: `{"pid":7,"pid_start_time":null,"acquired_at":"2026-09-08T00:00:00Z"}`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, err := json.Marshal(tt.body)
			if err != nil {
				t.Fatalf("Marshal() = %v", err)
			}
			if diff := gocmp.Diff(tt.want, string(out)); diff != "" {
				t.Errorf("body bytes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAcquiringWritesABodyNamingThisProcess(t *testing.T) {
	t.Parallel()

	locksDir := filepath.Join(t.TempDir(), ".locks")
	guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	defer func() { _ = guard.Release() }()

	body, ok := ReadBody(guard.Path())
	if !ok {
		t.Fatalf("ReadBody(%q) = absent, want the holder record", guard.Path())
	}
	if body.PID != uint32(os.Getpid()) {
		t.Errorf("pid = %d, want %d", body.PID, os.Getpid())
	}
	if body.AcquiredAt == "" {
		t.Errorf("the body records when it was taken")
	}
	rec, err := proc.Lookup(t.Context(), os.Getpid())
	if err != nil {
		t.Fatalf("Lookup(self) = %v", err)
	}
	if body.PIDStartTime == nil || *body.PIDStartTime != rec.StartIdentity() {
		t.Errorf("pid_start_time = %v, want %q: a recycled pid must be detectable", body.PIDStartTime, rec.StartIdentity())
	}
}

func TestReacquiringTruncatesTheOldBodyRatherThanAppending(t *testing.T) {
	t.Parallel()

	// The body is replaced in place, so a reacquired lock whose previous
	// body was longer must not leave the old record's tail behind: half of
	// one JSON document after the end of another reads as absent, and a
	// holder would go unreported.
	locksDir := filepath.Join(t.TempDir(), ".locks")
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	path := filepath.Join(locksDir, "acct.org.lock")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1024)), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	guard, err := Acquire(t.Context(), locksDir, "acct.org.lock", time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	defer func() { _ = guard.Release() }()

	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if strings.Contains(string(bytes), "x") {
		t.Errorf("the previous contents survived the rewrite:\n%s", bytes)
	}
	if _, ok := ReadBody(path); !ok {
		t.Errorf("ReadBody(%q) = absent, want the fresh holder record:\n%s", path, bytes)
	}
}

func TestACorruptBodyReadsAsAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(t *testing.T, name string, contents []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("WriteFile(%q) = %v", path, err)
		}
		return path
	}

	tests := map[string]struct {
		path func(t *testing.T) string
	}{
		"success: a missing file": {
			path: func(*testing.T) string { return filepath.Join(dir, "absent") },
		},
		"success: bytes that are not json": {
			path: func(t *testing.T) string { return write(t, "corrupt", []byte("not json")) },
		},
		"success: a file too large to be a record": {
			path: func(t *testing.T) string { return write(t, "huge", []byte(strings.Repeat("x", 8192))) },
		},
		"success: a record missing its pid": {
			path: func(t *testing.T) string {
				return write(t, "no-pid", []byte(`{"pid_start_time":null,"acquired_at":"2026-09-08T00:00:00Z"}`))
			},
		},
		"success: a pid that is not a number": {
			path: func(t *testing.T) string {
				return write(t, "bad-pid", []byte(`{"pid":"bad","pid_start_time":null,"acquired_at":"x"}`))
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if body, ok := ReadBody(tt.path(t)); ok {
				t.Errorf("ReadBody() = %+v, want absent", body)
			}
		})
	}
}

func TestABodyWithoutAStartIdentityStillReads(t *testing.T) {
	t.Parallel()

	// A null start is a holder whose kernel would not answer, not a
	// corrupt record: doctor falls back to the pid alone.
	path := filepath.Join(t.TempDir(), "null-start.lock")
	if err := os.WriteFile(path, []byte(`{"pid":42,"pid_start_time":null,"acquired_at":"2026-09-08T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	body, ok := ReadBody(path)
	if !ok {
		t.Fatalf("ReadBody() = absent, want the record")
	}
	want := LockBody{PID: 42, AcquiredAt: "2026-09-08T00:00:00Z"}
	if diff := gocmp.Diff(want, body); diff != "" {
		t.Errorf("body mismatch (-want +got):\n%s", diff)
	}
}
