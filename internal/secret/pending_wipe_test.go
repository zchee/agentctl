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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/awnumar/memguard"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestPendingResolutionWipesReadBuffers(t *testing.T) {
	old := tokenDocJSON("at-old", "rt-old")
	replacement := tokenDocJSON("at-new", "rt-new")
	external := tokenDocJSON("at-external", "rt-external")
	tests := map[string]struct {
		current  string
		meta     string
		taken    bool
		want     PendingDecision
		wantErr  bool
		wantFile string
		reads    int
	}{
		"success: replay preserves rotated credential": {
			current: old, want: PendingDecision{Kind: PendingReplayed},
			wantFile: replacement, reads: 3,
		},
		"success: first write replays": {
			want:     PendingDecision{Kind: PendingReplayed, FirstWrite: true},
			wantFile: replacement, reads: 2,
		},
		"success: changed credential discards pending": {
			current:  external,
			want:     PendingDecision{Kind: PendingDiscarded, Reason: PendingFileChanged},
			wantFile: external, reads: 3,
		},
		"success: namespace takeover discards before target read": {
			current: old, taken: true,
			want:     PendingDecision{Kind: PendingDiscarded, Reason: PendingNamespaceTakenOver},
			wantFile: old, reads: 2,
		},
		"error: malformed meta discards pending": {
			current: old, meta: `{"access_token":"unexpected-secret"`,
			want:     PendingDecision{Kind: PendingDiscarded, Reason: PendingInvalid},
			wantFile: old, reads: 2,
		},
		"error: malformed target keeps pending": {
			current: `{"tokens":{"access_token":"torn-secret"`,
			wantErr: true, wantFile: `{"tokens":{"access_token":"torn-secret"`, reads: 3,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			if tt.current != "" {
				put(t, store, testAuth, tt.current)
			}
			put(t, store, testAuthPending, replacement)
			meta := tt.meta
			if meta == "" {
				var prior *Digests
				if tt.current != "" {
					prior = docDigests(t, old)
				}
				meta = metaBody(t, prior)
			}
			put(t, store, testAuthMeta, meta)

			var buffers [][]byte
			original := readSecretFileBytes
			readSecretFileBytes = func(reader io.Reader) ([]byte, error) {
				buffer, err := original(reader)
				buffers = append(buffers, buffer)
				return buffer, err
			}
			t.Cleanup(func() { readSecretFileBytes = original })

			decision, write, err := ResolvePendingWith(store.codexFD, store.codexNS, testSpec(nil), tt.taken, tokenDoc{strictUnusable: true})
			if tt.wantErr {
				if !isErrorType[*errs.ConfigError](err) {
					t.Fatalf("a malformed target must return a configuration error, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("resolution: %v", err)
				}
				if diff := gocmp.Diff(tt.want, decision); diff != "" {
					t.Errorf("decision mismatch (-want +got):\n%s", diff)
				}
				if write == nil {
					t.Fatal("resolution must report the pending digests")
				}
				if diff := gocmp.Diff(docDigests(t, replacement), write.Pending); diff != "" {
					t.Errorf("pending digests mismatch (-want +got):\n%s", diff)
				}
			}
			if len(buffers) != tt.reads {
				t.Fatalf("read buffer count = %d, want %d", len(buffers), tt.reads)
			}
			for i, buffer := range buffers {
				if len(buffer) == 0 {
					t.Errorf("read %d did not capture a document", i)
				}
				if !bytes.Equal(buffer, make([]byte, len(buffer))) {
					t.Errorf("read %d retained nonzero document bytes after resolution", i)
				}
			}
			if diff := gocmp.Diff(tt.wantFile, readText(t, filepath.Join(store.codexNS, testAuth))); diff != "" {
				t.Errorf("credential bytes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSecretFileWipesRejectedReadBuffers(t *testing.T) {
	tests := map[string]struct {
		partial bool
	}{
		"error: file grows beyond the limit after stat": {},
		"error: descriptor closes after a partial read": {partial: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newSecretFileStore(t)
			put(t, store, testAuth, "credential-bytes")
			path := filepath.Join(store.codexNS, testAuth)
			var captured []byte
			original := readSecretFileBytes
			readSecretFileBytes = func(reader io.Reader) ([]byte, error) {
				if tt.partial {
					prefix := make([]byte, 4)
					defer memguard.WipeBytes(prefix)
					if _, err := io.ReadFull(reader, prefix); err != nil {
						t.Fatalf("read prefix: %v", err)
					}
					file := reader.(*io.LimitedReader).R.(*os.File)
					if err := file.Close(); err != nil {
						t.Fatalf("close descriptor: %v", err)
					}
					reader = io.MultiReader(bytes.NewReader(prefix), reader)
				} else {
					file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatalf("open growing file: %v", err)
					}
					if _, err := file.WriteString("-more-credential-bytes"); err != nil {
						_ = file.Close()
						t.Fatalf("grow file: %v", err)
					}
					if err := file.Close(); err != nil {
						t.Fatalf("close growing file: %v", err)
					}
				}
				buffer, err := original(reader)
				captured = buffer
				return buffer, err
			}
			t.Cleanup(func() { readSecretFileBytes = original })

			outcome, err := readFileAt(store.codexFD, testAuth, 16, path)
			if tt.partial {
				if !isErrorType[*errs.IOError](err) || !errors.Is(err, os.ErrClosed) {
					t.Fatalf("partial read must return a closed-file I/O error, got %v", err)
				}
			} else if !isErrorType[*TooLargeError](err) {
				t.Fatalf("grown file must return a size error, got %v", err)
			}
			if outcome.Present || outcome.Bytes != nil {
				t.Errorf("failed read must not return a document")
			}
			if len(captured) == 0 {
				t.Fatal("the rejected allocation must contain a partial or oversized read")
			}
			if !bytes.Equal(captured, make([]byte, len(captured))) {
				t.Error("rejected read allocation retained nonzero credential bytes")
			}
		})
	}
}
