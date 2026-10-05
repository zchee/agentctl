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

package cleanup_test

import (
	"sync"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/runtime/cleanup"
)

// appendEntry registers a function that records its own name, so a test can
// assert exactly which entries ran and in which order.
func appendEntry(r *cleanup.Registry, log *[]string, name string) cleanup.Token {
	return r.Register(func() { *log = append(*log, name) })
}

func TestRegistryRun(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		arrange func(t *testing.T, r *cleanup.Registry, log *[]string)
		want    []string
	}{
		"success: entries run newest first": {
			arrange: func(t *testing.T, r *cleanup.Registry, log *[]string) {
				appendEntry(r, log, "first")
				appendEntry(r, log, "second")
				appendEntry(r, log, "third")
			},
			want: []string{"third", "second", "first"},
		},
		"success: a withdrawn entry does not run": {
			arrange: func(t *testing.T, r *cleanup.Registry, log *[]string) {
				appendEntry(r, log, "kept early")
				token := appendEntry(r, log, "withdrawn")
				appendEntry(r, log, "kept late")
				if !r.Unregister(token) {
					t.Fatal("Unregister reported no entry removed for a live token")
				}
			},
			want: []string{"kept late", "kept early"},
		},
		"success: a panicking entry does not stop the others": {
			arrange: func(t *testing.T, r *cleanup.Registry, log *[]string) {
				appendEntry(r, log, "first")
				r.Register(func() { panic("one entry failing") })
				appendEntry(r, log, "third")
			},
			want: []string{"third", "first"},
		},
		"success: an empty registry runs nothing": {
			arrange: func(t *testing.T, r *cleanup.Registry, log *[]string) {},
			want:    nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var r cleanup.Registry
			var log []string
			tt.arrange(t, &r, &log)

			r.Run()

			if diff := gocmp.Diff(tt.want, log); diff != "" {
				t.Errorf("run order mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRegistryRunOnce(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		runs int
	}{
		"success: a second run finds nothing left": {runs: 2},
		"success: a third run is equally empty":    {runs: 3},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var r cleanup.Registry
			var ran atomic.Int64
			r.Register(func() { ran.Add(1) })

			for range tt.runs {
				r.Run()
			}

			if got := ran.Load(); got != 1 {
				t.Errorf("entry ran %d times, want exactly once across %d runs", got, tt.runs)
			}
		})
	}
}

func TestRegistryUnregister(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		arrange func(r *cleanup.Registry) cleanup.Token
		want    bool
	}{
		"success: a live token removes its entry": {
			arrange: func(r *cleanup.Registry) cleanup.Token {
				return r.Register(func() {})
			},
			want: true,
		},
		"success: a second withdrawal reports nothing removed": {
			arrange: func(r *cleanup.Registry) cleanup.Token {
				token := r.Register(func() {})
				r.Unregister(token)
				return token
			},
			want: false,
		},
		"success: a token already consumed by a run reports nothing removed": {
			arrange: func(r *cleanup.Registry) cleanup.Token {
				token := r.Register(func() {})
				r.Run()
				return token
			},
			want: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var r cleanup.Registry
			token := tt.arrange(&r)

			if got := r.Unregister(token); got != tt.want {
				t.Errorf("Unregister = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRegistryConcurrentRegistration(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		goroutines int
		perG       int
	}{
		"success: every concurrently registered entry runs exactly once": {
			goroutines: 8,
			perG:       64,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var r cleanup.Registry
			var ran atomic.Int64
			var wg sync.WaitGroup
			for range tt.goroutines {
				wg.Go(func() {
					for range tt.perG {
						r.Register(func() { ran.Add(1) })
					}
				})
			}
			wg.Wait()

			// Two runs together must account for every entry exactly once,
			// however the first drain interleaved with registration.
			r.Run()
			r.Run()

			want := int64(tt.goroutines * tt.perG)
			if got := ran.Load(); got != want {
				t.Errorf("ran %d entries, want %d", got, want)
			}
		})
	}
}

func TestRegisterNil(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{}{
		"error: registering a nil function panics": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Error("Register(nil) did not panic")
				}
			}()
			var r cleanup.Registry
			r.Register(nil)
		})
	}
}

func TestDefaultRegistry(t *testing.T) {
	// Not parallel: the process-wide registry is shared state, and another
	// test running entries concurrently would make the counts ambiguous.
	tests := map[string]struct{}{
		"success: the package-level functions share one process-wide registry": {},
	}

	for name := range tests {
		t.Run(name, func(t *testing.T) {
			var ran atomic.Int64
			kept := cleanup.Register(func() { ran.Add(1) })
			withdrawn := cleanup.Register(func() { ran.Add(1) })

			if !cleanup.Unregister(withdrawn) {
				t.Fatal("Unregister reported no entry removed for a live token")
			}
			cleanup.Run()

			if got := ran.Load(); got != 1 {
				t.Errorf("ran %d entries, want 1", got)
			}
			if cleanup.Unregister(kept) {
				t.Error("Unregister removed an entry the run should have consumed")
			}
		})
	}
}
