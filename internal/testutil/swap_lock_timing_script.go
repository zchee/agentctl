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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("swap-hold-json", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: swap-hold-json <stdout-file> <exclusive-limit-ms>")
		}
		limit, err := strconv.ParseUint(args[1], 10, 64)
		ts.Check(err)
		decoder := jsontext.NewDecoder(strings.NewReader(ts.ReadFile(args[0])))
		outcomes := 0
		for {
			body, err := decoder.ReadValue()
			if err == io.EOF {
				break
			}
			ts.Check(err)
			var row struct {
				Kind string `json:"kind"`
				Lock struct {
					BudgetMS *uint64 `json:"budget_ms"`
					HoldMS   *uint64 `json:"hold_ms"`
				} `json:"lock"`
			}
			ts.Check(json.Unmarshal(body, &row))
			if row.Kind != "outcome" {
				continue
			}
			outcomes++
			if row.Lock.BudgetMS == nil || *row.Lock.BudgetMS != 3000 || row.Lock.HoldMS == nil || *row.Lock.HoldMS >= limit {
				ts.Fatalf("outcome must measure its hold below %d ms against the 3000 ms budget", limit)
			}
		}
		if outcomes != 1 {
			ts.Fatalf("observed %d measured outcomes; want 1", outcomes)
		}
	})
	registerScriptCmd("swap-peer-wait", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-peer-wait")
		}
		dir := ts.Getenv("SWAP_STORE")
		primary := filepath.Join(dir, ".oauth_refresh.lock")
		recordDir := filepath.Join(ts.Getenv("AGENTCTL_CONFIG_DIR"), "claude", "held-locks")
		ts.Check(os.Mkdir(primary, 0o700))
		started := time.Now()
		done := make(chan struct{})
		var monitorErr error
		go func() {
			defer close(done)
			defer func() { monitorErr = errors.Join(monitorErr, os.Remove(primary)) }()
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-timer.C:
					return
				case <-ticker.C:
					for _, path := range []string{dir + ".lock", filepath.Join(dir, ".storage-write.lock")} {
						if _, err := os.Lstat(path); !os.IsNotExist(err) {
							monitorErr = fmt.Errorf("another peer artifact appeared during the primary-lock wait: %s: %v", path, err)
							return
						}
					}
					records, err := os.ReadDir(recordDir)
					if err != nil && !os.IsNotExist(err) {
						monitorErr = err
						return
					}
					if len(records) != 0 {
						monitorErr = fmt.Errorf("%d held records appeared during the primary-lock wait", len(records))
						return
					}
				}
			}
		}()
		ts.Defer(func() { <-done; ts.Check(monitorErr) })
		ts.SetCmd("swap-peer-wait-check", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: swap-peer-wait-check")
			}
			elapsed := time.Since(started)
			<-done
			ts.Check(monitorErr)
			if elapsed < time.Second {
				ts.Fatalf("the CLI pass did not wait for the fresh peer lock: %s", elapsed)
			}
		})
	})
	registerScriptCmd("swap-load", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-load")
		}
		workers := 2 * runtime.GOMAXPROCS(0)
		var running atomic.Bool
		var rounds atomic.Uint64
		var group sync.WaitGroup
		running.Store(true)
		for range workers {
			group.Go(func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				var value uint64
				for running.Load() {
					for range 50_000 {
						value = value*6_364_136_223_846_793_005 + 1
					}
					runtime.KeepAlive(value)
					rounds.Add(1)
				}
			})
		}
		stop := func() { running.Store(false); group.Wait() }
		ts.Defer(stop)
		ts.SetCmd("swap-load-check", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: swap-load-check")
			}
			stop()
			if got := rounds.Load(); got < uint64(workers) {
				ts.Fatalf("load completed %d rounds; want at least %d", got, workers)
			}
		})
	})
}
