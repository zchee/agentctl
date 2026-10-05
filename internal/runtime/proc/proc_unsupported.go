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

//go:build !darwin

package proc

import (
	"context"
	"runtime"
)

// List refuses: this platform offers no process observation.
func List(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &UnsupportedPlatformError{GOOS: runtime.GOOS}
}

// Lookup refuses: this platform offers no process observation.
func Lookup(ctx context.Context, pid int) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	return Process{}, &UnsupportedPlatformError{GOOS: runtime.GOOS}
}

// SameUserNamed refuses: this platform offers no process observation.
func SameUserNamed(ctx context.Context, name string) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &UnsupportedPlatformError{GOOS: runtime.GOOS}
}

// WriterGone can prove nothing here, and not knowing is not evidence of
// absence, so the answer is always false.
func WriterGone(ctx context.Context, pid int, recorded string) bool {
	return false
}
