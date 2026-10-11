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

// Package desktop observes and drives the lifecycle of the Claude desktop
// app: which of this user's processes belong to it, asking it to quit and
// proving that it did, and starting the one installed copy again.
//
// It never signals, kills or otherwise touches a process; the only
// actions are an AppleScript quit request and an open of the fixed bundle
// path. Everything else is observation, and an observation that cannot be
// made fails closed: an error, never a count of zero.
package desktop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// AppBundle is the one supported install of the app.
	AppBundle = "/Applications/Claude.app"
	// MainPath is the app's main executable inside AppBundle.
	MainPath = AppBundle + "/Contents/MacOS/Claude"
	// NativeHostPath is the native messaging host the Claude browser
	// extension starts through Chrome. It lives in the app's bundle but
	// belongs to the browser that started it.
	NativeHostPath = AppBundle + "/Contents/Helpers/chrome-native-host"
	// BundleID is the bundle identifier the quit request addresses.
	BundleID = "com.anthropic.claudefordesktop"

	bundlePrefix = AppBundle + "/"
	// mainSuffix ends the main executable path of any copy of the app.
	mainSuffix = "/Contents/MacOS/Claude"
)

// browserMains are the main executables whose direct child the native
// host may be without belonging to the app.
var browserMains = func() []string {
	var mains []string
	for _, name := range []string{"Google Chrome", "Google Chrome Beta", "Google Chrome Canary", "Google Chrome Dev", "Chromium"} {
		mains = append(mains, "/Applications/"+name+".app/Contents/MacOS/"+name)
	}
	return mains
}()

// Record is one row of a process listing. Rows of other users carry no
// path: their executable path is never read.
type Record struct {
	// PID is the process id.
	PID int32
	// PPID is the parent process id.
	PPID int32
	// UID is the real user id the process belongs to.
	UID uint32
	// Start is when the process started.
	Start time.Time
	// Path is the executable path, read only for this user's processes.
	Path string
}

// Lister produces the complete process table: every row with its parent,
// and the executable path of every row that belongs to uid. A process that
// disappears between the listing and the path read is omitted; any other
// failure is an error, never a shorter table.
type Lister func(ctx context.Context, uid uint32) ([]Record, error)

// Proc is a process the snapshot names.
type Proc struct {
	PID   int32     `json:"pid"`
	PPID  int32     `json:"ppid"`
	Start time.Time `json:"start"`
	Path  string    `json:"path"`
}

// StartIdentity renders the start instant the way the journal records it.
func (p Proc) StartIdentity() string { return p.Start.UTC().Format(time.RFC3339Nano) }

// Reason says why a process counts as part of the app's instance.
type Reason string

const (
	// ReasonData is a process run from the app's data directory, such as
	// the Code tab's hosted binary.
	ReasonData Reason = "data"
	// ReasonOrphan is a bundle process whose parent is pid 1, typically
	// left behind by a main that exited.
	ReasonOrphan Reason = "orphan"
	// ReasonParentMissing is a bundle process whose parent is not in the
	// table; unknown ancestry is never evidence of absence.
	ReasonParentMissing Reason = "parent-missing"
	// ReasonDescendant is a bundle process with a main or a counted
	// process among its ancestors.
	ReasonDescendant Reason = "descendant"
	// ReasonUnknown is every other bundle process; it counts because its
	// ownership is not proved otherwise.
	ReasonUnknown Reason = "unknown"
)

// FamilyProc is a process that blocks a move of the data directory.
type FamilyProc struct {
	Proc
	Reason Reason `json:"reason"`
}

// ForeignProc is the native host running under a live browser main: it
// is reported and never waited for.
type ForeignProc struct {
	Proc
	ParentPath string `json:"parent_path"`
}

// Snapshot is one classified observation of this user's processes.
type Snapshot struct {
	// Mains are the app's main executables.
	Mains []Proc
	// Family are this user's other processes that block a move.
	Family []FamilyProc
	// Foreign are native hosts owned by a browser.
	Foreign []ForeignProc
	// OtherCopies are main executables of another installed copy.
	OtherCopies []Proc
}

// Quiet reports whether nothing of the app runs: no main and no counted
// process. Other copies are a separate refusal, checked by the caller.
func (s Snapshot) Quiet() bool { return len(s.Mains) == 0 && len(s.Family) == 0 }

// MalformedError reports a listing that cannot be trusted.
type MalformedError struct{ Reason string }

// Error names what was wrong with the listing.
func (e *MalformedError) Error() string { return "process listing: " + e.Reason }

// Classify sorts the table's processes of uid into mains, family, foreign
// hosts and other copies. dataDir is the app's data directory. A row of
// uid without a path, a duplicate pid or a row without a plausible start
// fails the whole classification.
func Classify(rows []Record, uid uint32, dataDir string) (Snapshot, error) {
	if dataDir == "" || !strings.HasPrefix(dataDir, "/") {
		return Snapshot{}, &MalformedError{Reason: "the data directory is not an absolute path"}
	}
	byPID := make(map[int32]Record, len(rows))
	for _, row := range rows {
		if row.PID <= 0 && row.UID == uid {
			return Snapshot{}, &MalformedError{Reason: fmt.Sprintf("a row of this user has pid %d", row.PID)}
		}
		if _, dup := byPID[row.PID]; dup {
			return Snapshot{}, &MalformedError{Reason: fmt.Sprintf("pid %d is listed twice", row.PID)}
		}
		byPID[row.PID] = row
		if row.UID != uid {
			continue
		}
		if row.Path == "" {
			return Snapshot{}, &MalformedError{Reason: fmt.Sprintf("pid %d has no executable path", row.PID)}
		}
		if row.Start.IsZero() {
			return Snapshot{}, &MalformedError{Reason: fmt.Sprintf("pid %d has no plausible start", row.PID)}
		}
	}

	c := classifier{byPID: byPID, uid: uid, dataPrefix: strings.TrimSuffix(dataDir, "/") + "/", memo: map[int32]bool{}, visiting: map[int32]bool{}}
	var snap Snapshot
	for _, row := range rows {
		if row.UID != uid {
			continue
		}
		proc := Proc{PID: row.PID, PPID: row.PPID, Start: row.Start, Path: row.Path}
		switch {
		case row.Path == MainPath:
			snap.Mains = append(snap.Mains, proc)
		case strings.HasPrefix(row.Path, c.dataPrefix):
			snap.Family = append(snap.Family, FamilyProc{Proc: proc, Reason: ReasonData})
		case strings.HasPrefix(row.Path, bundlePrefix):
			if parent, ok := c.browserParent(row); ok {
				snap.Foreign = append(snap.Foreign, ForeignProc{Proc: proc, ParentPath: parent})
				continue
			}
			snap.Family = append(snap.Family, FamilyProc{Proc: proc, Reason: c.bundleReason(row)})
		case strings.HasSuffix(row.Path, mainSuffix):
			snap.OtherCopies = append(snap.OtherCopies, proc)
		}
	}
	byPIDOrder := func(a, b Proc) int { return cmp.Compare(a.PID, b.PID) }
	slices.SortFunc(snap.Mains, byPIDOrder)
	slices.SortFunc(snap.OtherCopies, byPIDOrder)
	slices.SortFunc(snap.Family, func(a, b FamilyProc) int { return byPIDOrder(a.Proc, b.Proc) })
	slices.SortFunc(snap.Foreign, func(a, b ForeignProc) int { return byPIDOrder(a.Proc, b.Proc) })
	return snap, nil
}

type classifier struct {
	byPID      map[int32]Record
	uid        uint32
	dataPrefix string
	// memo caches whether a pid counts; visiting breaks a parent cycle,
	// which a consistent kernel table never has but a corrupt one could.
	memo     map[int32]bool
	visiting map[int32]bool
}

// browserParent returns the parent's path when row is the native host and
// its direct parent is a live browser main of the same user.
func (c *classifier) browserParent(row Record) (string, bool) {
	if row.Path != NativeHostPath {
		return "", false
	}
	parent, ok := c.byPID[row.PPID]
	if !ok || parent.UID != c.uid || !slices.Contains(browserMains, parent.Path) {
		return "", false
	}
	return parent.Path, true
}

// bundleReason classifies a bundle process that is not a foreign host.
func (c *classifier) bundleReason(row Record) Reason {
	if row.PPID == 1 {
		return ReasonOrphan
	}
	if _, ok := c.byPID[row.PPID]; !ok {
		return ReasonParentMissing
	}
	if c.ancestorCounts(row) {
		return ReasonDescendant
	}
	return ReasonUnknown
}

// ancestorCounts walks the parent chain of row through the whole table
// and reports whether any ancestor is a main or a counted process.
func (c *classifier) ancestorCounts(row Record) bool {
	seen := map[int32]bool{row.PID: true}
	pid := row.PPID
	for pid > 1 {
		if seen[pid] {
			return false
		}
		seen[pid] = true
		parent, ok := c.byPID[pid]
		if !ok {
			return false
		}
		if c.counts(parent) {
			return true
		}
		pid = parent.PPID
	}
	return false
}

// counts reports whether a row is a main or a counted process of uid.
func (c *classifier) counts(row Record) bool {
	if row.UID != c.uid {
		return false
	}
	if done, ok := c.memo[row.PID]; ok {
		return done
	}
	if c.visiting[row.PID] {
		return false
	}
	c.visiting[row.PID] = true
	defer delete(c.visiting, row.PID)
	var result bool
	switch {
	case row.Path == MainPath, strings.HasPrefix(row.Path, c.dataPrefix):
		result = true
	case strings.HasPrefix(row.Path, bundlePrefix):
		_, foreign := c.browserParent(row)
		result = !foreign
	}
	c.memo[row.PID] = result
	return result
}

// Observer takes classified snapshots of the app's processes.
type Observer struct {
	// List reads the process table.
	List Lister
	// UID is the user whose processes are classified.
	UID uint32
	// DataDir is the app's data directory.
	DataDir string
}

// Snapshot lists and classifies once.
func (o Observer) Snapshot(ctx context.Context) (Snapshot, error) {
	if o.List == nil {
		return Snapshot{}, errors.New("no process lister is configured")
	}
	rows, err := o.List(ctx, o.UID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("listing processes: %w", err)
	}
	return Classify(rows, o.UID, o.DataDir)
}
