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
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/errs"
)

func TestTerminalPrompt(t *testing.T) {
	tests := map[string]struct {
		answer string
		yes    bool
	}{
		"success: y":                    {answer: "y\n", yes: true},
		"success: uppercase yes":        {answer: " YES \n", yes: true},
		"success: no":                   {answer: "no\n"},
		"success: default declines":     {answer: "\n"},
		"success: other input declines": {answer: "yes please\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
			if _, err := master.WriteString(tt.answer); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			prompt := TerminalPrompt{In: slave, Out: &out}
			if err := prompt.Tell("Action"); err != nil {
				t.Fatal(err)
			}
			yes, err := prompt.Confirm(t.Context(), "Proceed?")
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.yes, yes); diff != "" {
				t.Errorf("confirmation (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("Action\nProceed? [y/N] ", out.String()); diff != "" {
				t.Errorf("output (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTerminalPromptRefusesPipedConsent(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	if _, err := output.WriteString("yes\n"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	yes, err := (TerminalPrompt{In: input, Out: &out}).Confirm(t.Context(), "Delete?")
	if yes || err == nil || errs.ExitCode(err) != 2 {
		t.Fatalf("Confirm = %v, %v", yes, err)
	}
	if !strings.Contains(err.Error(), "standard input is not a terminal") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("nonterminal prompt printed %q", out.String())
	}
}

func TestTerminalPromptCancellation(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	yes, err := (TerminalPrompt{In: slave, Out: &out}).Confirm(ctx, "Delete?")
	if yes || !errors.Is(err, context.Canceled) {
		t.Fatalf("Confirm = %v, %v", yes, err)
	}
}
