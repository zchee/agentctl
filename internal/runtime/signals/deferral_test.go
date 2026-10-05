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

package signals_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/zchee/agentctl/internal/runtime/signals"
)

func TestExecuteWithoutSignal(t *testing.T) {
	tests := map[string]struct {
		cancel bool
		want   error
	}{
		"success: normal completion":                     {},
		"error: command failure is preserved":            {want: errors.New("command failed")},
		"error: parent cancellation does not force exit": {cancel: true, want: context.Canceled},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, controller := signals.Install(parent)
			defer controller.Stop()
			var forced atomic.Bool
			err := controller.Execute(func(ctx context.Context) error {
				if tt.cancel {
					cancel()
					<-ctx.Done()
					return ctx.Err()
				}
				return tt.want
			}, func(os.Signal) { forced.Store(true) })
			if !errors.Is(err, tt.want) {
				t.Errorf("Execute error = %v, want %v", err, tt.want)
			}
			if forced.Load() {
				t.Error("Execute forced exit without a signal")
			}
			controller.Wait()
		})
	}
}
