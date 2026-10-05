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

package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBrowserDetachedLaunch(t *testing.T) {
	dir := t.TempDir()
	program, output := filepath.Join(dir, "browser"), filepath.Join(dir, "argument")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > '" + output + "'\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := launchBrowser(t.Context(), program, "http://127.0.0.1/authorize?state=one"); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, err := os.ReadFile(output)
		if err == nil && string(body) == "http://127.0.0.1/authorize?state=one" {
			break
		}
		select {
		case <-timer.C:
			t.Fatalf("opener argument not received: %v", err)
		case <-ticker.C:
		}
	}
	if err := launchBrowser(t.Context(), filepath.Join(dir, "missing"), "http://127.0.0.1"); err == nil {
		t.Fatal("missing opener accepted")
	}
}
