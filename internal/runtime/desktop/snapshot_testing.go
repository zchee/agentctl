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

//go:build agentctl_testing

package desktop

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The testing build replaces the whole system boundary: the process table
// comes from a file, and the bundle and the programs come from a directory
// that stands in for the root of the file system. Both must be set; the
// testing build never falls back to the real process table, the real
// programs or the installed app.
const (
	processTableEnv = "AGENTCTL_TEST_PROCESS_TABLE"
	testRootEnv     = "AGENTCTL_DESKTOP_TEST_ROOT"
	testPollPause   = time.Millisecond
)

func init() {
	testWorld = testingWorld
}

func testingWorld(dataDir string) (*System, error) {
	table, root := os.Getenv(processTableEnv), os.Getenv(testRootEnv)
	if table == "" || root == "" || !filepath.IsAbs(table) || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the testing build needs absolute %s and %s", processTableEnv, testRootEnv)
	}
	stat := filepath.Join(root, "usr", "bin", "stat")
	deviceOf := StatDevice
	if _, err := os.Stat(stat); err == nil {
		deviceOf = func(path string) (uint64, error) { return fakeDevice(stat, path) }
	}
	return &System{
		Observer:     Observer{List: TableLister(table), UID: uint32(os.Getuid()), DataDir: dataDir},
		Osascript:    filepath.Join(root, "usr", "bin", "osascript"),
		Open:         filepath.Join(root, "usr", "bin", "open"),
		Bundle:       filepath.Join(root, strings.TrimPrefix(AppBundle, "/")),
		PollInterval: testPollPause,
		QuitPolls:    QuitPolls,
		LaunchPolls:  LaunchPolls,
		DeviceOf:     deviceOf,
	}, nil
}

// fakeDevice asks the fixture's stat program for a device id with the same
// `stat -f %d` call a shell would make, so an arm can make two directories
// look like two devices.
func fakeDevice(stat, path string) (uint64, error) {
	out, err := exec.Command(stat, "-f", "%d", path).Output()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", stat, err)
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// TableRecord is one row of the testing process table.
type TableRecord struct {
	PID   int32   `json:"pid"`
	PPID  *int32  `json:"ppid"`
	UID   *uint32 `json:"uid"`
	Start string  `json:"start"`
	Path  string  `json:"path"`
	// AppearAtRead hides the row until the table has been read this many
	// times, so a launch can be made visible exactly after the readiness
	// wait has ended.
	AppearAtRead int `json:"appear_at_read,omitzero"`
	// Gone makes the path read report a process that exited after the
	// listing.
	Gone bool `json:"gone,omitzero"`
	// ReadError makes the path read fail with this errno name.
	ReadError string `json:"read_error,omitzero"`
}

// Table is the testing process table file.
type Table struct {
	Records []TableRecord `json:"records"`
}

// TableLister reads the testing process table at path. Every read
// increments a counter beside it; an absent or malformed file fails the
// listing exactly as a kernel error would.
func TableLister(path string) Lister {
	return func(ctx context.Context, uid uint32) ([]Record, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reads, err := bumpReads(path + ".reads")
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the process table: %w", err)
		}
		var table Table
		if err := json.Unmarshal(data, &table, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("the process table is malformed: %w", err)
		}
		rows := make([]Record, 0, len(table.Records))
		for i, rec := range table.Records {
			if rec.PPID == nil || rec.UID == nil || rec.PID == 0 {
				return nil, fmt.Errorf("process table record %d lacks pid, ppid or uid", i)
			}
			if rec.AppearAtRead > reads {
				continue
			}
			row := Record{PID: rec.PID, PPID: *rec.PPID, UID: *rec.UID}
			if rec.Start != "" {
				start, err := time.Parse(time.RFC3339Nano, rec.Start)
				if err != nil {
					return nil, fmt.Errorf("process table record %d: %w", i, err)
				}
				row.Start = start
			}
			if row.UID == uid {
				switch {
				case rec.Gone:
					continue
				case rec.ReadError != "":
					return nil, fmt.Errorf("reading the executable path of process %d: %w", rec.PID, errnoNamed(rec.ReadError))
				case rec.Path == "":
					return nil, fmt.Errorf("process %d: the argument block is empty", rec.PID)
				}
				row.Path = rec.Path
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
}

func bumpReads(path string) (int, error) {
	count := 0
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		count, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return 0, fmt.Errorf("the process table read counter is malformed: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	count++
	if err := os.WriteFile(path, []byte(strconv.Itoa(count)+"\n"), 0o600); err != nil {
		return 0, err
	}
	return count, nil
}

func errnoNamed(name string) error {
	switch name {
	case "EIO":
		return syscall.EIO
	case "EPERM":
		return syscall.EPERM
	default:
		return fmt.Errorf("errno %s", name)
	}
}
