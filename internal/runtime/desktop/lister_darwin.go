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

//go:build darwin

package desktop

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/agentctl/internal/runtime/proc"
)

// readRetries and readRetryPause bound the retry of a failed path read. On
// a busy Mac a process caught in the middle of exec or exit fails the read
// for a few milliseconds; a retry turns that into either a path or a gone
// process, and a failure that persists still fails the snapshot.
const (
	readRetries    = 3
	readRetryPause = 20 * time.Millisecond
)

// SystemLister lists the kernel's process table and reads the executable
// path of each of uid's processes through kern.procargs2.
func SystemLister(ctx context.Context, uid uint32) ([]Record, error) {
	procs, err := proc.List(ctx)
	if err != nil {
		return nil, err
	}
	return recordsFrom(ctx, procs, uid, readProcArgs, proc.Lookup)
}

func readProcArgs(pid int32) ([]byte, error) {
	return unix.SysctlRaw("kern.procargs2", int(pid))
}

// recordsFrom builds the table from a listing. read and lookup are the
// kernel calls, injectable so every error path is testable.
func recordsFrom(ctx context.Context, procs []proc.Process, uid uint32, read func(int32) ([]byte, error), lookup func(context.Context, int) (proc.Process, error)) ([]Record, error) {
	rows := make([]Record, 0, len(procs))
	for _, p := range procs {
		row := Record{PID: p.PID, PPID: p.PPID, UID: p.RealUID, Start: p.Start}
		if p.RealUID != uid {
			rows = append(rows, row)
			continue
		}
		if p.Holder == proc.HolderDead {
			// A zombie has exited; it holds nothing and has no arguments.
			continue
		}
		if p.Start.IsZero() {
			return nil, fmt.Errorf("process %d: the kernel reported no plausible start", p.PID)
		}
		path, gone, err := readPath(ctx, p.PID, read, lookup)
		if err != nil {
			return nil, err
		}
		if gone {
			continue
		}
		row.Path = path
		rows = append(rows, row)
	}
	return rows, nil
}

func readPath(ctx context.Context, pid int32, read func(int32) ([]byte, error), lookup func(context.Context, int) (proc.Process, error)) (path string, gone bool, err error) {
	block, err := read(pid)
	for attempt := 0; err != nil && attempt < readRetries; attempt++ {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(readRetryPause):
		}
		block, err = read(pid)
	}
	if err != nil {
		if goneOnRead(err) {
			return "", true, nil
		}
		if _, lookupErr := lookup(ctx, int(pid)); errors.Is(lookupErr, proc.ErrProcessGone) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("reading the executable path of process %d: %w", pid, err)
	}
	path, err = ExecutablePath(block)
	if err != nil {
		return "", false, fmt.Errorf("process %d: %w", pid, err)
	}
	return path, false, nil
}

// goneOnRead reports a path read that failed because the process went
// away (or became a zombie) between the listing and the read.
func goneOnRead(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EINVAL)
}
