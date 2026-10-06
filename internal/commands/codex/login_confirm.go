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

package codex

import (
	"context"
	"fmt"

	"github.com/zchee/agentctl/internal/commands"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
)

func loginAlreadyOwned(ctx context.Context, paths *config.Paths, user, account string) (string, error) {
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return "", err
	}
	for _, record := range registry.CodexAccounts {
		if record.ChatGPTUserID == user && record.ChatGPTAccountID == account && record.Kind.Owned != nil {
			return record.ChatGPTUserID + "/" + record.ChatGPTAccountID, nil
		}
	}
	return "", nil
}

func loginConfirmOverwrite(ctx context.Context, shown string, terminal bool, prompt commands.Prompter) error {
	if !terminal {
		return errs.NewRefused(0, fmt.Sprintf("`%s` is already logged in and standard input is not a terminal, so there is nobody to confirm replacing its stored grant; the grant agentctl holds was left alone. Run this from a terminal, or `agentctl codex accounts remove %s` first", shown, shown))
	}
	yes, err := prompt.Confirm(ctx, fmt.Sprintf("replace the grant agentctl already holds for `%s`?", shown))
	if err != nil {
		return err
	}
	if yes {
		return nil
	}
	return errs.NewRefused(0, fmt.Sprintf("`%s` was left as it was; the new grant was discarded", shown))
}
