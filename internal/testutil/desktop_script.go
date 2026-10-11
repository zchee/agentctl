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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// desktopMainPath is the desktop app's main executable as the process table
// records it. The table holds paths as data; nothing is run from them.
const desktopMainPath = "/Applications/Claude.app/Contents/MacOS/Claude"

// procRecord mirrors one row of the testing process table the desktop
// observer reads in the testing build.
type procRecord struct {
	PID          int32   `json:"pid"`
	PPID         *int32  `json:"ppid"`
	UID          *uint32 `json:"uid"`
	Start        string  `json:"start"`
	Path         string  `json:"path"`
	AppearAtRead int     `json:"appear_at_read,omitzero"`
	Gone         bool    `json:"gone,omitzero"`
	ReadError    string  `json:"read_error,omitzero"`
}

type procTable struct {
	Records []procRecord `json:"records"`
}

func loadProcTable(path string) (procTable, error) {
	var table procTable
	data, err := os.ReadFile(path)
	if err != nil {
		return table, err
	}
	err = json.Unmarshal(data, &table)
	return table, err
}

func saveProcTable(path string, table procTable) error {
	if table.Records == nil {
		table.Records = []procRecord{}
	}
	data, err := json.Marshal(table, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// tableReads returns how many times the observer has listed the table.
func tableReads(path string) (int, error) {
	data, err := os.ReadFile(path + ".reads")
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// proctableMain edits the testing process table, standing in for the
// processes the fake desktop programs start and end:
//
//	proctable init <table>
//	proctable add <table> <pid> <ppid> <path> [delay=N] [uid=N] [read_error=E] [gone] [nostart]
//	proctable rm <table> <pid>...
//	proctable reparent <table> <pid> <ppid>
//	proctable nextpid <table>
//	proctable quit <table> <data-dir> quit|mains|keep-data
//	proctable list <table>
//
// delay=N hides the row until N more listings have been taken.
func proctableMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		say(stderr, "usage: proctable <init|add|rm|reparent|nextpid|quit|list> <table> ...\n")
		return 2
	}
	verb, path, rest := args[0], args[1], args[2:]
	if verb == "init" {
		if err := saveProcTable(path, procTable{}); err != nil {
			say(stderr, "proctable: %v\n", err)
			return 1
		}
		return 0
	}
	table, err := loadProcTable(path)
	if err != nil {
		say(stderr, "proctable: %v\n", err)
		return 1
	}
	switch verb {
	case "add":
		err = procAdd(path, &table, rest)
	case "rm":
		err = procRemove(&table, rest)
	case "reparent":
		err = procReparent(&table, rest)
	case "nextpid":
		next := int32(1000)
		for _, r := range table.Records {
			next = max(next, r.PID+1)
		}
		say(stdout, "%d\n", next)
		return 0
	case "quit":
		if len(rest) != 2 {
			err = fmt.Errorf("quit needs <data-dir> <mode>")
			break
		}
		err = procQuit(&table, rest[0], rest[1])
	case "list":
		for _, r := range table.Records {
			say(stdout, "%d %d %s\n", r.PID, *r.PPID, r.Path)
		}
		return 0
	default:
		err = fmt.Errorf("unknown verb %q", verb)
	}
	if err == nil {
		err = saveProcTable(path, table)
	}
	if err != nil {
		say(stderr, "proctable %s: %v\n", verb, err)
		return 1
	}
	return 0
}

func procAdd(path string, table *procTable, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("add needs <pid> <ppid> <path>")
	}
	pid, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil {
		return err
	}
	ppid, err := strconv.ParseInt(args[1], 10, 32)
	if err != nil {
		return err
	}
	parent, uid := int32(ppid), uint32(os.Getuid())
	rec := procRecord{PID: int32(pid), PPID: &parent, UID: &uid, Path: args[2], Start: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, opt := range args[3:] {
		key, value, _ := strings.Cut(opt, "=")
		switch key {
		case "delay":
			n, err := strconv.Atoi(value)
			if err != nil {
				return err
			}
			if n > 0 {
				reads, err := tableReads(path)
				if err != nil {
					return err
				}
				rec.AppearAtRead = reads + n
			}
		case "uid":
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return err
			}
			other := uint32(n)
			rec.UID = &other
		case "read_error":
			rec.ReadError = value
		case "gone":
			rec.Gone = true
		case "nostart":
			rec.Start = ""
		default:
			return fmt.Errorf("unknown option %q", opt)
		}
	}
	if slices.ContainsFunc(table.Records, func(r procRecord) bool { return r.PID == rec.PID }) {
		return fmt.Errorf("pid %d is already in the table", rec.PID)
	}
	table.Records = append(table.Records, rec)
	return nil
}

func procRemove(table *procTable, args []string) error {
	for _, arg := range args {
		pid, err := strconv.ParseInt(arg, 10, 32)
		if err != nil {
			return err
		}
		table.Records = slices.DeleteFunc(table.Records, func(r procRecord) bool { return r.PID == int32(pid) })
	}
	return nil
}

func procReparent(table *procTable, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("reparent needs <pid> <ppid>")
	}
	pid, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil {
		return err
	}
	ppid, err := strconv.ParseInt(args[1], 10, 32)
	if err != nil {
		return err
	}
	for i := range table.Records {
		if table.Records[i].PID == int32(pid) {
			parent := int32(ppid)
			table.Records[i].PPID = &parent
			return nil
		}
	}
	return fmt.Errorf("pid %d is not in the table", pid)
}

// procQuit ends what the app's quit ends. The app's tree is every process
// below one of its mains; processes run from the data directory belong to
// the app too. A survivor whose parent exited is reparented to pid 1, as
// the kernel does.
func procQuit(table *procTable, dataDir, mode string) error {
	uid := uint32(os.Getuid())
	ends := map[int32]bool{}
	for _, r := range table.Records {
		if *r.UID == uid && r.Path == desktopMainPath {
			ends[r.PID] = true
		}
	}
	underData := func(r procRecord) bool { return strings.HasPrefix(r.Path, dataDir+"/") }
	switch mode {
	case "mains":
	case "quit", "keep-data":
		for changed := true; changed; {
			changed = false
			for _, r := range table.Records {
				if !ends[r.PID] && ends[*r.PPID] && (mode == "quit" || !underData(r)) {
					ends[r.PID], changed = true, true
				}
			}
		}
		if mode == "quit" {
			for _, r := range table.Records {
				if *r.UID == uid && underData(r) {
					ends[r.PID] = true
				}
			}
		}
	default:
		return fmt.Errorf("unknown quit mode %q", mode)
	}
	table.Records = slices.DeleteFunc(table.Records, func(r procRecord) bool { return ends[r.PID] })
	for i := range table.Records {
		if ends[*table.Records[i].PPID] {
			one := int32(1)
			table.Records[i].PPID = &one
		}
	}
	return nil
}

// desktopInfoPlist is the bundle description the fake install carries: the
// desktop app's bundle identifier, as its real Info.plist names it.
const desktopInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>com.anthropic.claudefordesktop</string>
	<key>CFBundleExecutable</key>
	<string>Claude</string>
</dict>
</plist>
`

// installDesktopWorld builds the directory that stands in for the root of
// the file system in the testing build: the fake osascript and open, and a
// fake app bundle at Applications/Claude.app. It returns that directory and
// an empty process table. The testing build refuses to run the desktop
// commands without both, so no script can reach the installed app.
func installDesktopWorld(harness string) (root, table string, err error) {
	root = filepath.Join(harness, "desktop-root")
	bin := filepath.Join(root, "usr", "bin")
	contents := filepath.Join(root, "Applications", "Claude.app", "Contents")
	for _, dir := range []string{bin, filepath.Join(contents, "MacOS")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
	}
	if err := installScriptFile("fake-osascript.sh", filepath.Join(bin, "osascript")); err != nil {
		return "", "", err
	}
	if err := installScriptFile("fake-open.sh", filepath.Join(bin, "open")); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(desktopInfoPlist), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(contents, "MacOS", "Claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		return "", "", err
	}
	table = filepath.Join(harness, "process-table.json")
	if err := saveProcTable(table, procTable{}); err != nil {
		return "", "", err
	}
	return root, table, nil
}

// treesigMain prints one line per entry of the tree at args[0]: relative
// path, type, size, modification time in nanoseconds and inode, sorted by
// path. The root's own modification time is left out, because the only
// change a switch may make to a profile directory is its rename, which can
// touch the directory itself. Two equal signatures prove nothing inside the
// profile was created, removed, rewritten or replaced.
func treesigMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		say(stderr, "usage: treesig <dir>\n")
		return 2
	}
	root := args[0]
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no inode", path)
		}
		mtime := strconv.FormatInt(info.ModTime().UnixNano(), 10)
		if rel == "." {
			mtime = "-"
		}
		size := int64(0)
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		lines = append(lines, fmt.Sprintf("%s %s %d %s %d", rel, info.Mode().Type(), size, mtime, st.Ino))
		return nil
	})
	if err != nil {
		say(stderr, "treesig: %v\n", err)
		return 1
	}
	slices.Sort(lines)
	for _, line := range lines {
		say(stdout, "%s\n", line)
	}
	return 0
}
