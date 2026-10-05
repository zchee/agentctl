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
	"context"
	"fmt"
	"math"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/errs"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

// RunUndo restores the credential selected from the whole write audit log.
func (p SessionProcess) RunUndo(ctx context.Context, globals cli.Globals, opts cli.ClaudeUseOptions) error {
	paths, err := config.Resolve(globals.ConfigDir)
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(ctx); err != nil {
		return err
	}
	tail, err := secret.TailAuditLog(paths, math.MaxInt)
	if err != nil {
		return err
	}
	registry, err := config.LoadRegistry(ctx, paths)
	if err != nil {
		return err
	}
	entry := secret.SelectUndo(tail)
	var reversal useReversal
	switch entry.Kind {
	case secret.UndoUnreadable:
		return errs.NewConfig(fmt.Sprintf("the audit log's line %d could not be read, and it may be the entry `--undo` needs; agentctl will not guess which swap to reverse (`agentctl claude doctor` names the line)", entry.Line))
	case secret.UndoNothing:
		return tell(p.Out, "there is no swap to undo: the audit log records no reversible write")
	case secret.UndoNamespace:
		reversal, err = namespacedReversal(ctx, paths, registry, entry)
	case secret.UndoLive:
		reversal, err = liveReversal(ctx, paths, registry, entry)
	}
	if err != nil {
		return err
	}
	env := claude.EnvFromProcess()
	swap := useLiveSwap{paths: paths, config: registry, env: &env, live: reversal.live, inherited: reversal.inherited, process: p}
	incoming := useIncoming{record: &reversal.owner, direction: secret.DirectionUndo, source: reversal.source, undone: reversal.undone}
	report := swap.swapIn(ctx, incoming, reversal.store, opts)
	if err := p.emitUse(report, opts.JSON); err != nil {
		return err
	}
	if code := report.outcome.ExitCode(); code != 0 {
		return &errs.ChildExit{Code: code}
	}
	return nil
}
