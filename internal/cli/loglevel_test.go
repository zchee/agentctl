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

package cli

import (
	"log/slog"
	"testing"
)

func TestLogLevel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  slog.Level
	}{
		"success: debug":                           {value: "debug", want: slog.LevelDebug},
		"success: info":                            {value: "info", want: slog.LevelInfo},
		"success: warn":                            {value: "warn", want: slog.LevelWarn},
		"success: error":                           {value: "error", want: slog.LevelError},
		"success: unset falls back to warn":        {value: "", want: slog.LevelWarn},
		"success: unknown falls back to warn":      {value: "verbose", want: slog.LevelWarn},
		"success: uppercase is not in the grammar": {value: "DEBUG", want: slog.LevelWarn},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := LogLevel(tt.value); got != tt.want {
				t.Fatalf("LogLevel(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
