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
	"encoding/json/jsontext"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zchee/agentctl/internal/cli"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
)

const desktopTestRegistry = `{
  "version": 1,
  "accounts": [{
    "account_uuid": "11111111-2222-3333-4444-555555555555",
    "organization_uuid": "66666666-7777-8888-9999-000000000000",
    "email": "a@example.com",
    "org_name": "Acme",
    "label": "a",
    "kind": {"kind": "owned", "export_spelling": "", "export_sha8": ""},
    "forgotten": false,
    "created_at": "2026-09-08T00:00:00Z"
  }]
}
`

// desktopTestSystem builds a system whose bundle is a stand-in under root,
// whose process table is always empty and whose programs never run: the
// consent question comes before any quit or launch.
func desktopTestSystem(t *testing.T, root string) func(string) (*rdesktop.System, error) {
	t.Helper()
	bundle := filepath.Join(root, "Claude.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o700); err != nil {
		t.Fatal(err)
	}
	plistData := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + rdesktop.BundleID + `</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plistData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "MacOS", "Claude"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return func(dataDir string) (*rdesktop.System, error) {
		return &rdesktop.System{
			Observer: rdesktop.Observer{
				List:    func(context.Context, uint32) ([]rdesktop.Record, error) { return nil, nil },
				UID:     uint32(os.Getuid()),
				DataDir: dataDir,
			},
			Osascript: filepath.Join(root, "no-osascript"), Open: filepath.Join(root, "no-open"),
			Bundle: bundle, PollInterval: time.Millisecond, QuitPolls: 1, LaunchPolls: 1,
			DeviceOf: func(string) (uint64, error) { return 1, nil },
		}, nil
	}
}

// TestRunUseJSONKeepsTheQuestionOffStdout drives the consent question
// through a terminal while --json is set: a program reading stdout must get
// exactly one JSON document, so the question has to go to the error stream.
func TestRunUseJSONKeepsTheQuestionOffStdout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, configDir := filepath.Join(root, "home"), filepath.Join(root, "config")
	for _, dir := range []string{home, configDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(desktopTestRegistry), 0o600); err != nil {
		t.Fatal(err)
	}

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	process := DesktopProcess{In: slave, Out: &stdout, Err: &stderr, System: desktopTestSystem(t, root), LockWait: time.Second}
	runErr := process.RunUse(t.Context(), cli.Globals{ConfigDir: configDir}, cli.ClaudeDesktopUseOptions{ID: "a", JSON: true})
	t.Logf("run error: %v\nstdout: %s\nstderr: %s", runErr, stdout.String(), stderr.String())
	if runErr == nil {
		t.Fatal("declining the question must not report success")
	}

	if !strings.Contains(stderr.String(), "[y/N]") {
		t.Errorf("the question did not reach the error stream: %q", stderr.String())
	}
	dec := jsontext.NewDecoder(&stdout)
	if _, err := dec.ReadValue(); err != nil {
		t.Fatalf("stdout does not start with a JSON document: %v", err)
	}
	if _, err := dec.ReadValue(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries more than one JSON document: %v", err)
	}
}
