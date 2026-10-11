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

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

// writeBundle lays out a stand-in app bundle: a main executable (unless
// noMain) and an Info.plist with the given bytes.
func writeBundle(t *testing.T, info []byte, noMain bool) string {
	t.Helper()

	bundle := filepath.Join(t.TempDir(), "Claude.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !noMain {
		if err := os.WriteFile(filepath.Join(bundle, "Contents", "MacOS", "Claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if info != nil {
		if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), info, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bundle
}

// encodePlist renders the identifier in one of the property-list formats
// an installed bundle can carry.
func encodePlist(t *testing.T, id string, format int) []byte {
	t.Helper()

	data, err := plist.Marshal(map[string]string{"CFBundleIdentifier": id, "CFBundleName": "Claude"}, format)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCheckBundle(t *testing.T) {
	t.Parallel()

	oversized := encodePlist(t, BundleID, plist.XMLFormat)
	padding := "<!--" + strings.Repeat("x", maxInfoPlist) + "-->"
	oversized = []byte(strings.Replace(string(oversized), "<plist", padding+"<plist", 1))

	tests := map[string]struct {
		info    []byte
		noMain  bool
		wantErr error
		wantMsg string
	}{
		"success: an XML property list with the app's identifier": {
			info: encodePlist(t, BundleID, plist.XMLFormat),
		},
		"success: a binary property list with the app's identifier": {
			info: encodePlist(t, BundleID, plist.BinaryFormat),
		},
		"error: another bundle identifier is a mismatch, named in the message": {
			info:    encodePlist(t, "com.example.other", plist.BinaryFormat),
			wantErr: ErrBundleMismatch,
			wantMsg: `"com.example.other"`,
		},
		"error: no main executable means the app is not installed": {
			info:    encodePlist(t, BundleID, plist.XMLFormat),
			noMain:  true,
			wantErr: ErrAppNotFound,
		},
		"error: a missing Info.plist is a mismatch, never a pass": {
			wantErr: ErrBundleMismatch,
			wantMsg: "reading Info.plist",
		},
		"error: an Info.plist over the read bound is refused unparsed": {
			info:    oversized,
			wantErr: ErrBundleMismatch,
			wantMsg: "larger than",
		},
		"error: bytes that are no property list are a mismatch": {
			info:    []byte("\x00\x01not a plist"),
			wantErr: ErrBundleMismatch,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := &System{Bundle: writeBundle(t, tt.info, tt.noMain)}
			err := s.CheckBundle()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("CheckBundle() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckBundle() = %v, want an error wrapping %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("CheckBundle() = %q, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}

func TestCheckBundleRefusesALinkedBundle(t *testing.T) {
	t.Parallel()

	real := writeBundle(t, encodePlist(t, BundleID, plist.XMLFormat), false)
	link := filepath.Join(t.TempDir(), "Claude.app")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s := &System{Bundle: link}
	err := s.CheckBundle()
	t.Logf("CheckBundle: %v", err)
	if !errors.Is(err, ErrBundleMismatch) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckBundle() = %v, want a mismatch naming the link", err)
	}
}

func TestLaunchChecksAnUncheckedBundle(t *testing.T) {
	t.Parallel()

	sys, w := newFakeSystem(t)
	if err := os.Remove(filepath.Join(sys.Bundle, "Contents", "Info.plist")); err != nil {
		t.Fatal(err)
	}
	report := sys.Launch(t.Context(), nil)
	t.Logf("report %+v", report)
	if !errors.Is(report.OpenErr, ErrBundleMismatch) || report.Ready {
		t.Fatalf("Launch = %+v, want a refused launch", report)
	}
	if got := logLines(t, w, "open.log"); len(got) != 0 {
		t.Fatalf("the launcher ran: %q", got)
	}
}

func TestLaunchRefusesABundleChangedSinceTheCheck(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// change runs between CheckBundle and Launch.
		change func(t *testing.T, bundle string)
	}{
		"error: the bundle directory replaced by another one": {
			change: func(t *testing.T, bundle string) {
				if err := os.Rename(bundle, bundle+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.CopyFS(bundle, os.DirFS(bundle+".old")); err != nil {
					t.Fatal(err)
				}
			},
		},
		"error: the bundle path turned into a link to another directory": {
			change: func(t *testing.T, bundle string) {
				if err := os.Rename(bundle, bundle+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(bundle+".old", bundle); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sys, w := newFakeSystem(t)
			if err := sys.CheckBundle(); err != nil {
				t.Fatalf("CheckBundle: %v", err)
			}
			tt.change(t, sys.Bundle)
			report := sys.Launch(t.Context(), nil)
			t.Logf("report %+v", report)
			if !errors.Is(report.OpenErr, ErrBundleMismatch) || report.Ready {
				t.Fatalf("Launch = %+v, want a refused launch", report)
			}
			if got := logLines(t, w, "open.log"); len(got) != 0 {
				t.Fatalf("the launcher ran: %q", got)
			}
		})
	}
}
