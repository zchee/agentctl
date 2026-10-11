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

package desktop

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

const (
	me      uint32 = 501
	other   uint32 = 502
	dataDir        = "/Users/u/Library/Application Support/Claude"
	helper         = AppBundle + "/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper"
	gpu            = AppBundle + "/Contents/Frameworks/Claude Helper (GPU).app/Contents/MacOS/Claude Helper (GPU)"
	hosted         = dataDir + "/claude-code/2.1.0/claude"
	chrome         = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	relay          = "/usr/bin/relay"
)

var t0 = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

func row(pid, ppid int32, uid uint32, path string) Record {
	r := Record{PID: pid, PPID: ppid, UID: uid, Start: t0.Add(time.Duration(pid) * time.Millisecond)}
	if uid == me {
		r.Path = path
	}
	return r
}

// summary flattens a snapshot into pid lists so a table can state it.
type summary struct {
	Mains       []int32
	Family      map[int32]Reason
	Foreign     []int32
	OtherCopies []int32
}

func summarize(s Snapshot) summary {
	out := summary{Family: map[int32]Reason{}}
	for _, p := range s.Mains {
		out.Mains = append(out.Mains, p.PID)
	}
	for _, p := range s.Family {
		out.Family[p.PID] = p.Reason
	}
	for _, p := range s.Foreign {
		out.Foreign = append(out.Foreign, p.PID)
	}
	for _, p := range s.OtherCopies {
		out.OtherCopies = append(out.OtherCopies, p.PID)
	}
	return out
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rows    []Record
		want    summary
		wantErr bool
	}{
		"success: main with two helpers whose paths have spaces": {
			rows: []Record{row(1, 0, 0, ""), row(100, 1, me, MainPath), row(101, 100, me, helper), row(102, 100, me, gpu)},
			want: summary{Mains: []int32{100}, Family: map[int32]Reason{101: ReasonDescendant, 102: ReasonDescendant}},
		},
		"success: a helper of a helper is a descendant": {
			rows: []Record{row(1, 0, 0, ""), row(100, 1, me, MainPath), row(101, 100, me, helper), row(103, 101, me, gpu)},
			want: summary{Mains: []int32{100}, Family: map[int32]Reason{101: ReasonDescendant, 103: ReasonDescendant}},
		},
		"success: a hosted binary under the data directory counts as data, never a main": {
			rows: []Record{row(1, 0, 0, ""), row(100, 1, me, MainPath), row(110, 100, me, hosted)},
			want: summary{Mains: []int32{100}, Family: map[int32]Reason{110: ReasonData}},
		},
		"success: a hosted binary left alone after the main exited still counts": {
			rows: []Record{row(1, 0, 0, ""), row(110, 1, me, hosted)},
			want: summary{Family: map[int32]Reason{110: ReasonData}},
		},
		"success: the native host under a live Chrome main is foreign, not family": {
			rows: []Record{row(1, 0, 0, ""), row(200, 1, me, chrome), row(201, 200, me, NativeHostPath)},
			want: summary{Foreign: []int32{201}, Family: map[int32]Reason{}},
		},
		"success: the native host under Chromium is foreign": {
			rows: []Record{row(1, 0, 0, ""), row(200, 1, me, "/Applications/Chromium.app/Contents/MacOS/Chromium"), row(201, 200, me, NativeHostPath)},
			want: summary{Foreign: []int32{201}, Family: map[int32]Reason{}},
		},
		"success: the native host under a non-browser parent is unknown": {
			rows: []Record{row(1, 0, 0, ""), row(200, 1, me, "/usr/local/bin/node"), row(201, 200, me, NativeHostPath)},
			want: summary{Family: map[int32]Reason{201: ReasonUnknown}},
		},
		"success: the native host under a Chrome-like path outside /Applications is unknown": {
			rows: []Record{row(1, 0, 0, ""), row(200, 1, me, "/Users/u/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"), row(201, 200, me, NativeHostPath)},
			want: summary{Family: map[int32]Reason{201: ReasonUnknown}},
		},
		"success: the native host orphaned is an orphan": {
			rows: []Record{row(1, 0, 0, ""), row(201, 1, me, NativeHostPath)},
			want: summary{Family: map[int32]Reason{201: ReasonOrphan}},
		},
		"success: the native host under another user's Chrome is unknown": {
			rows: []Record{row(1, 0, 0, ""), row(200, 1, other, chrome), row(201, 200, me, NativeHostPath)},
			want: summary{Family: map[int32]Reason{201: ReasonUnknown}},
		},
		"success: an orphaned bundle process counts": {
			rows: []Record{row(1, 0, 0, ""), row(101, 1, me, helper)},
			want: summary{Family: map[int32]Reason{101: ReasonOrphan}},
		},
		"success: a bundle process whose parent is not in the table counts": {
			rows: []Record{row(1, 0, 0, ""), row(101, 999, me, helper)},
			want: summary{Family: map[int32]Reason{101: ReasonParentMissing}},
		},
		"success: a helper reached through an executable outside both prefixes is a descendant": {
			rows: []Record{row(1, 0, 0, ""), row(100, 1, me, MainPath), row(150, 100, me, relay), row(151, 150, me, helper)},
			want: summary{Mains: []int32{100}, Family: map[int32]Reason{151: ReasonDescendant}},
		},
		"success: that helper after the main is gone and the relay is orphaned is unknown": {
			rows: []Record{row(1, 0, 0, ""), row(150, 1, me, relay), row(151, 150, me, helper)},
			want: summary{Family: map[int32]Reason{151: ReasonUnknown}},
		},
		"success: a helper under an orphaned helper is a descendant of a counted process": {
			rows: []Record{row(1, 0, 0, ""), row(101, 1, me, helper), row(102, 101, me, gpu)},
			want: summary{Family: map[int32]Reason{101: ReasonOrphan, 102: ReasonDescendant}},
		},
		"success: another copy's main is listed apart and is neither main nor family": {
			rows: []Record{row(1, 0, 0, ""), row(300, 1, me, "/Users/u/Applications/Claude.app/Contents/MacOS/Claude"), row(301, 300, me, "/Users/u/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper")},
			want: summary{OtherCopies: []int32{300}, Family: map[int32]Reason{}},
		},
		"success: a sibling bundle is in none of the four": {
			rows: []Record{row(1, 0, 0, ""), row(400, 1, me, "/Applications/Claude Mail.app/Contents/MacOS/Claude Mail")},
			want: summary{Family: map[int32]Reason{}},
		},
		"success: other users' processes are excluded": {
			rows: []Record{row(1, 0, 0, ""), {PID: 500, PPID: 1, UID: other, Start: t0, Path: MainPath}, {PID: 501, PPID: 500, UID: other, Start: t0, Path: helper}},
			want: summary{Family: map[int32]Reason{}},
		},
		"success: a parent cycle in a corrupt table ends as unknown": {
			rows: []Record{row(1, 0, 0, ""), row(160, 161, me, relay), row(161, 160, me, relay), row(162, 160, me, helper)},
			want: summary{Family: map[int32]Reason{162: ReasonUnknown}},
		},
		"error: a row of this user without a path": {
			rows:    []Record{{PID: 100, PPID: 1, UID: me, Start: t0}},
			wantErr: true,
		},
		"error: a row of this user without a start": {
			rows:    []Record{{PID: 100, PPID: 1, UID: me, Path: MainPath}},
			wantErr: true,
		},
		"error: a duplicate pid": {
			rows:    []Record{row(100, 1, me, MainPath), row(100, 1, me, helper)},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snap, err := Classify(tt.rows, me, dataDir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Classify succeeded: %+v", snap)
				}
				t.Logf("error: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			got := summarize(snap)
			t.Logf("snapshot %+v", got)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("Classify mismatch (-want +got):\n%s", diff)
			}
			if wantQuiet := len(tt.want.Mains) == 0 && len(tt.want.Family) == 0; snap.Quiet() != wantQuiet {
				t.Fatalf("Quiet = %v, want %v", snap.Quiet(), wantQuiet)
			}
		})
	}
}

func TestClassifyRefusesRelativeDataDir(t *testing.T) {
	t.Parallel()
	if _, err := Classify(nil, me, "Library/Application Support/Claude"); err == nil {
		t.Fatal("Classify accepted a relative data directory")
	}
}

func TestExecutablePath(t *testing.T) {
	t.Parallel()

	block := func(argc uint32, rest string) []byte {
		b := binary.NativeEndian.AppendUint32(nil, argc)
		return append(b, rest...)
	}
	tests := map[string]struct {
		block   []byte
		want    string
		wantErr bool
	}{
		"success: path with spaces, padding and arguments after it": {
			block: block(2, helper+"\x00\x00\x00\x00"+helper+"\x00--type=renderer\x00"),
			want:  helper,
		},
		"success: arguments are never part of the answer": {
			block: block(3, MainPath+"\x00"+MainPath+"\x00--token=secret\x00"),
			want:  MainPath,
		},
		"error: empty block":                {block: nil, wantErr: true},
		"error: only the count":             {block: block(1, ""), wantErr: true},
		"error: truncated path without NUL": {block: block(1, "/Applications/Claude.app/Cont"), wantErr: true},
		"error: empty path":                 {block: block(1, "\x00rest\x00"), wantErr: true},
		"error: implausible count":          {block: block(1<<30, MainPath+"\x00"), wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ExecutablePath(tt.block)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ExecutablePath = %q; want an error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ExecutablePath = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestObserverFailsClosed(t *testing.T) {
	t.Parallel()

	tests := map[string]Observer{
		"error: no lister":        {DataDir: dataDir, UID: me},
		"error: the lister fails": {DataDir: dataDir, UID: me, List: func(context.Context, uint32) ([]Record, error) { return nil, errors.New("kernel said no") }},
		"error: the lister returns a malformed row": {DataDir: dataDir, UID: me, List: func(context.Context, uint32) ([]Record, error) {
			return []Record{{PID: 100, PPID: 1, UID: me, Start: t0}}, nil
		}},
	}
	for name, obs := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snap, err := obs.Snapshot(t.Context())
			if err == nil {
				t.Fatalf("Snapshot succeeded with %+v; an observation that cannot be made must fail, never count zero", snap)
			}
			t.Logf("error: %v", err)
		})
	}
}
