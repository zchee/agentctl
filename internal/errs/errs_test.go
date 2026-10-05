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

package errs

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestExitCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want int
	}{
		"success: nil maps to the healthy status": {
			err:  nil,
			want: ExitOK,
		},
		"error: config is fatal": {
			err:  NewConfig("bad config"),
			want: ExitFatal,
		},
		"error: io is fatal": {
			err:  NewIO("reading the store", fs.ErrPermission),
			want: ExitFatal,
		},
		"error: keychain is partial": {
			err:  NewKeychain(KeychainLocked),
			want: ExitPartial,
		},
		"error: http is partial": {
			err:  NewHTTPRetryAfter(429, 30*time.Second),
			want: ExitPartial,
		},
		"error: auth is partial": {
			err:  NewAuth(true),
			want: ExitPartial,
		},
		"error: refused without a code is partial": {
			err:  NewRefused(0, "claude session detected"),
			want: ExitPartial,
		},
		"error: refused carries its own exit code": {
			err:  NewRefused(16, "peer locks held"),
			want: 16,
		},
		"error: lettered refusal carries its own exit code": {
			err:  NewRefusedLetter(11, "C"),
			want: 11,
		},
		"error: partial is partial": {
			err:  NewPartial(2),
			want: ExitPartial,
		},
		"error: wrapped errors keep their mapping": {
			err:  fmt.Errorf("while rendering: %w", NewKeychain(KeychainTimeout)),
			want: ExitPartial,
		},
		"error: wrapped refusal keeps its own exit code": {
			err:  fmt.Errorf("while swapping: %w", NewRefused(17, "item changed under the hold")),
			want: 17,
		},
		"error: unclassified failures are fatal": {
			err:  errors.New("unexpected"),
			want: ExitFatal,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := ExitCode(tt.err)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ExitCode(%v) mismatch (-want +got):\n%s", tt.err, diff)
			}
			if tt.err != nil && got == ExitOK {
				t.Errorf("ExitCode(%v) = %d: an error must not map to the success status", tt.err, got)
			}
		})
	}
}

func TestExitStatusConstants(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		got  int
		want int
	}{
		"success: ok is 0":      {got: ExitOK, want: 0},
		"success: fatal is 1":   {got: ExitFatal, want: 1},
		"success: partial is 2": {got: ExitPartial, want: 2},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("exit status constant mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestKeychainErrorMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		class KeychainClass
		want  string
	}{
		"success: locked": {
			class: KeychainLocked,
			want:  "locked",
		},
		"success: unavailable": {
			class: KeychainUnavailable,
			want:  "unavailable",
		},
		"success: timeout": {
			class: KeychainTimeout,
			want:  "timeout",
		},
		"success: not found": {
			class: KeychainNotFound,
			want:  "not found",
		},
		"success: other classes carry their label verbatim": {
			class: KeychainClass("errSec-25300"),
			want:  "errSec-25300",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if diff := gocmp.Diff(tt.want, string(tt.class)); diff != "" {
				t.Errorf("class label mismatch (-want +got):\n%s", diff)
			}
			rendered := NewKeychain(tt.class).Error()
			if !strings.Contains(rendered, tt.want) {
				t.Errorf("NewKeychain(%q).Error() = %q: should mention %q", tt.class, rendered, tt.want)
			}
		})
	}
}

func TestHTTPErrorMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err      error
		want     string
		contains []string
		excludes []string
	}{
		"success: retry window appears when the server sent one": {
			err:      NewHTTPRetryAfter(429, 30*time.Second),
			want:     "HTTP 429 (retry in 30s)",
			contains: []string{"429", "30"},
		},
		"success: a zero-second hint is still a hint": {
			err:      NewHTTPRetryAfter(429, 0),
			want:     "HTTP 429 (retry in 0s)",
			contains: []string{"retry in 0s"},
		},
		"success: sub-second hints truncate to whole seconds": {
			err:      NewHTTPRetryAfter(429, 1500*time.Millisecond),
			want:     "HTTP 429 (retry in 1s)",
			contains: []string{"retry in 1s"},
		},
		"success: no retry window is claimed when none was sent": {
			err:      NewHTTP(503),
			want:     "HTTP 503",
			contains: []string{"503"},
			excludes: []string{"retry"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := tt.err.Error()
			if diff := gocmp.Diff(tt.want, rendered); diff != "" {
				t.Errorf("message mismatch (-want +got):\n%s", diff)
			}
			for _, sub := range tt.contains {
				if !strings.Contains(rendered, sub) {
					t.Errorf("message %q should contain %q", rendered, sub)
				}
			}
			for _, sub := range tt.excludes {
				if strings.Contains(rendered, sub) {
					t.Errorf("message %q must not contain %q", rendered, sub)
				}
			}
		})
	}
}

func TestAuthErrorMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		invalidGrant bool
		contains     []string
		excludes     []string
	}{
		"success: invalid_grant names the upstream code and the recovery": {
			invalidGrant: true,
			contains:     []string{"invalid_grant", "login", "agentctl claude login"},
		},
		"success: a generic rejection claims nothing it does not know": {
			invalidGrant: false,
			contains:     []string{"authentication failed"},
			excludes:     []string{"invalid_grant"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := NewAuth(tt.invalidGrant).Error()
			for _, sub := range tt.contains {
				if !strings.Contains(rendered, sub) {
					t.Errorf("message %q should contain %q", rendered, sub)
				}
			}
			for _, sub := range tt.excludes {
				if strings.Contains(rendered, sub) {
					t.Errorf("message %q must not contain %q", rendered, sub)
				}
			}
		})
	}
}

func TestIOErrorKeepsItsSource(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		context string
		source  error
	}{
		"success: the message is the context only and the cause survives unwrapping": {
			context: "writing the credential file",
			source:  fs.ErrPermission,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := NewIO(tt.context, tt.source)
			if diff := gocmp.Diff(tt.context, err.Error()); diff != "" {
				t.Errorf("message mismatch (-want +got):\n%s", diff)
			}
			if !errors.Is(err, tt.source) {
				t.Errorf("errors.Is(%v, %v) = false: the underlying cause must survive", err, tt.source)
			}
			var ioErr *IOError
			if !errors.As(err, &ioErr) {
				t.Fatalf("errors.As(%v, *IOError) = false", err)
			}
			if diff := gocmp.Diff(tt.context, ioErr.Context); diff != "" {
				t.Errorf("context mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefusedErrorMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err      error
		contains []string
	}{
		"success: the reason survives rendering": {
			err:      NewRefused(16, "claude session detected (lock .oauth_refresh.lock)"),
			contains: []string{"refused", "claude session detected"},
		},
		"success: a lettered refusal names its letter": {
			err:      NewRefusedLetter(11, "C"),
			contains: []string{"refused", "refusal C"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := tt.err.Error()
			for _, sub := range tt.contains {
				if !strings.Contains(rendered, sub) {
					t.Errorf("message %q should contain %q", rendered, sub)
				}
			}
		})
	}
}

func TestRefusedErrorCarriesLetterOrReasonNeverBoth(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err error
	}{
		"success: the reason constructor sets no letter": {
			err: NewRefused(15, "not_owned"),
		},
		"success: the letter constructor sets no reason": {
			err: NewRefusedLetter(14, "F"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var refused *RefusedError
			if !errors.As(tt.err, &refused) {
				t.Fatalf("errors.As(%v, *RefusedError) = false", tt.err)
			}
			if refused.Letter != "" && refused.Reason != "" {
				t.Errorf("refusal carries letter %q and reason %q: it must carry one, never both", refused.Letter, refused.Reason)
			}
			if refused.Letter == "" && refused.Reason == "" {
				t.Errorf("refusal carries neither a letter nor a reason")
			}
		})
	}
}

func TestPartialErrorMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failed   int
		contains string
	}{
		"success: the count is visible": {
			failed:   3,
			contains: "3",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := NewPartial(tt.failed).Error()
			if !strings.Contains(rendered, tt.contains) {
				t.Errorf("message %q should contain %q", rendered, tt.contains)
			}
		})
	}
}

func TestPartialFromShown(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failed   int
		wantNil  bool
		wantCode int
	}{
		"success: no failed shown rows is a healthy run": {
			failed:  0,
			wantNil: true,
		},
		"error: failed shown rows aggregate into a partial exit": {
			failed:   2,
			wantCode: ExitPartial,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := PartialFromShown(tt.failed)
			if tt.wantNil {
				if err != nil {
					t.Fatalf("PartialFromShown(%d) = %v, want nil", tt.failed, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("PartialFromShown(%d) = nil, want a partial error", tt.failed)
			}
			var partial *PartialError
			if !errors.As(err, &partial) {
				t.Fatalf("errors.As(%v, *PartialError) = false", err)
			}
			if diff := gocmp.Diff(tt.failed, partial.Failed); diff != "" {
				t.Errorf("failed count mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantCode, ExitCode(err)); diff != "" {
				t.Errorf("exit code mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewNotImplemented(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		command  string
		contains []string
	}{
		"error: an unbuilt command names itself, says why, and is fatal": {
			command:  "agentctl claude status",
			contains: []string{"agentctl claude status", "not implemented"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := NewNotImplemented(tt.command)
			if got := ExitCode(err); got != ExitFatal {
				t.Errorf("ExitCode(%v) = %d, want %d: an unbuilt command must not exit 0 or 2", err, got, ExitFatal)
			}
			rendered := err.Error()
			for _, sub := range tt.contains {
				if !strings.Contains(rendered, sub) {
					t.Errorf("message %q should contain %q", rendered, sub)
				}
			}
		})
	}
}
