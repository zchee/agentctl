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
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("swap-live-equal", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 2 {
			ts.Fatalf("usage: swap-live-equal <actual> <expected>")
		}
		var actual, expected map[string]any
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[0])), &actual))
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &expected))
		if !gocmp.Equal(actual, expected) {
			ts.Fatalf("complete credential documents differ")
		}
	})
	registerScriptCmd("swap-live-watch", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 0 {
			ts.Fatalf("usage: swap-live-watch")
		}
		home, configDir, walked := ts.Getenv("HOME"), ts.Getenv("AGENTCTL_CONFIG_DIR"), ts.Getenv("SWAP_STORE")
		resolved, err := filepath.EvalSymlinks(walked)
		ts.Check(err)
		ts.Setenv("SWAP_RESOLVED", resolved)
		paths := []string{filepath.Join(resolved, ".oauth_refresh.lock"), resolved + ".lock", filepath.Join(resolved, ".storage-write.lock")}
		namespace := filepath.Join(configDir, "claude")
		snapshot := func() map[string]string {
			found := make(map[string]string)
			for _, root := range []string{home, configDir} {
				ts.Check(filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if path == namespace {
						return filepath.SkipDir
					}
					if path == filepath.Join(configDir, "cache") || path == filepath.Join(configDir, "cache", "claude") {
						if !entry.IsDir() {
							ts.Fatalf("permitted cache path is not a directory")
						}
						return nil
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					content := ""
					if info.Mode().IsRegular() {
						body, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						content = string(body)
					} else if info.Mode()&os.ModeSymlink != 0 {
						content, err = os.Readlink(path)
						if err != nil {
							return err
						}
					}
					found[path] = info.Mode().String() + "\n" + content
					return nil
				}))
			}
			return found
		}
		before := snapshot()
		stop, done, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		seen := 0
		go func() {
			defer close(done)
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			close(ready)
			for {
				present := 0
				for _, path := range paths {
					if info, err := os.Stat(path); err == nil && info.IsDir() {
						present++
					}
				}
				seen = max(seen, present)
				select {
				case <-stop:
					return
				case <-ticker.C:
				}
			}
		}()
		finish := func() { once.Do(func() { close(stop) }); <-done }
		ts.Defer(finish)
		<-ready
		ts.SetCmd("swap-live-watch-check", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: swap-live-watch-check")
			}
			finish()
			if seen != 3 {
				ts.Fatalf("observed %d simultaneous peer artifacts at resolved store; want 3", seen)
			}
			for _, dir := range []string{walked, resolved} {
				for i, path := range []string{filepath.Join(dir, ".oauth_refresh.lock"), dir + ".lock", filepath.Join(dir, ".storage-write.lock"), filepath.Join(dir, ".credentials.json"), filepath.Join(dir, ".credentials.adopted.json"), filepath.Join(dir, ".claude.json")} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						ts.Fatalf("unexpected live credential or unreleased artifact at %s: %v", path, err)
					}
					if i < 3 {
						delete(before, path)
					}
				}
			}
			if !gocmp.Equal(before, snapshot()) {
				ts.Fatalf("paths, modes, link targets or bytes outside namespace root changed")
			}
			body := []byte(ts.ReadFile(ts.Getenv("SWAP_AUDIT")))
			if bytes.Contains(body, []byte("SENTINEL")) || bytes.Contains(body, []byte("sk-ant-")) {
				ts.Fatalf("audit contains a fixture credential")
			}
			decoder := jsontext.NewDecoder(bytes.NewReader(body))
			breaks := 0
			for {
				value, err := decoder.ReadValue()
				if err == io.EOF {
					break
				}
				ts.Check(err)
				var row struct {
					Event   string `json:"event"`
					Tree    string `json:"tree"`
					Target  string `json:"target"`
					Service string `json:"service"`
					Path    string `json:"path"`
					Outcome string `json:"outcome"`
				}
				ts.Check(json.Unmarshal(value, &row))
				if row.Event == "lock_break" {
					breaks++
					if row.Tree != "live" || row.Target != "live" || row.Service != LiveService || row.Outcome != "broken" || !strings.HasPrefix(row.Path, resolved+string(filepath.Separator)) {
						ts.Fatalf("live break does not identify the resolved live store")
					}
				}
			}
			if breaks == 0 {
				ts.Fatalf("no live stale break was audited")
			}
		})
	})
}
