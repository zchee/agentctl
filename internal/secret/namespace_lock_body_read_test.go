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
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestReadBodyBoundsAndRefusesLeafLinks(t *testing.T) {
	document := []byte(`{"pid":42,"pid_start_time":null,"acquired_at":"holder"}`)
	tests := map[string]struct {
		size      int64
		symlink   bool
		grow      bool
		wantBody  bool
		wantBytes int
	}{
		"success: regular holder":                                  {wantBody: true, wantBytes: len(document)},
		"success: holder at the size ceiling":                      {size: lockBodyMaxLen, wantBody: true, wantBytes: lockBodyMaxLen},
		"error: oversized sparse file is rejected before reading":  {size: 16 << 20},
		"error: leaf symlink is not followed":                      {symlink: true},
		"error: growth after stat stops one byte past the ceiling": {grow: true, wantBytes: lockBodyMaxLen + 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "holder.lock")
			contents := document
			if tt.size == lockBodyMaxLen {
				contents = append(bytes.Clone(document), bytes.Repeat([]byte(" "), lockBodyMaxLen-len(document))...)
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if tt.size > lockBodyMaxLen {
				if err := os.Truncate(path, tt.size); err != nil {
					t.Fatalf("Truncate: %v", err)
				}
			}
			if tt.symlink {
				link := filepath.Join(filepath.Dir(path), "link.lock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				path = link
			}
			originalRead := readSecretFileBytes
			t.Cleanup(func() { readSecretFileBytes = originalRead })
			readBytes := 0
			readSecretFileBytes = func(reader io.Reader) ([]byte, error) {
				if tt.grow {
					if err := os.Truncate(path, 16<<20); err != nil {
						t.Fatalf("Truncate after stat: %v", err)
					}
				}
				data, err := originalRead(reader)
				readBytes += len(data)
				return data, err
			}
			body, ok := ReadBody(path)
			if diff := gocmp.Diff(tt.wantBody, ok); diff != "" {
				t.Errorf("ReadBody present (-want +got):\n%s", diff)
			}
			want := LockBody{}
			if tt.wantBody {
				want = LockBody{PID: 42, AcquiredAt: "holder"}
			}
			if diff := gocmp.Diff(want, body); diff != "" {
				t.Errorf("ReadBody record (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantBytes, readBytes); diff != "" {
				t.Errorf("bytes read from real file (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadBodyRefusesFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holder.lock")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		body, ok := ReadBody(path)
		done <- ok || body != (LockBody{})
	}()
	select {
	case present := <-done:
		if present {
			t.Error("FIFO produced a holder record")
		}
	case <-ctx.Done():
		// Release a blocked reader so a regression does not strand the test.
		if fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); err == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("ReadBody waited for a FIFO writer")
	}
}
