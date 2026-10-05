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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

const doctorAnomalousFile = "anomalous (regular file; Claude Code makes lock directories)"

type doctorArtefact struct {
	path string
	age  time.Duration
	kind os.FileMode
}

func doctorSample(path string) (doctorArtefact, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return doctorArtefact{}, false
	}
	return doctorArtefact{path: path, age: max(time.Since(info.ModTime()), 0), kind: info.Mode()}, true
}

func doctorArtefacts(nsDir string) []doctorArtefact {
	var out []doctorArtefact
	for _, path := range []string{filepath.Join(nsDir, secret.RefreshLockName), filepath.Join(nsDir, secret.StorageWriteLockName)} {
		if artefact, ok := doctorSample(path); ok {
			out = append(out, artefact)
		}
	}
	lexical := nsDir + ".lock"
	legacy, ok := doctorSample(lexical)
	if !ok {
		if canonical, err := claude.Canonical(nsDir); err == nil {
			legacy, ok = doctorSample(canonical + ".lock")
		}
	}
	if ok {
		legacy.path = lexical
		out = append(out, legacy)
	}
	return out
}

func doctorLegacyNotice(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	kind := "other file type"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symbolic link"
	case info.IsDir():
		kind = "directory"
	case info.Mode().IsRegular():
		kind = "regular file"
	}
	return fmt.Sprintf("Legacy agentctl artefact: `%s` (`%s`; %s); not a Claude Code mutex; left unchanged. agentctl does not remove or migrate this artefact.", path, secret.LegacyStorageWriteArtefact, kind)
}

func (d *Doctor) namespaceSection(ctx context.Context, registry *config.Registry) ([]string, error) {
	out := []string{"namespaces"}
	var artefacts []doctorArtefact
	owned := 0
	for _, record := range registry.Accounts {
		if record.Kind.Owned == nil {
			continue
		}
		owned++
		nsDir := d.Paths.NamespaceDir(record.AccountUUID, record.OrganizationUUID)
		out = append(out, "  "+nsDir, "    credentials    "+doctorStateOf(filepath.Join(nsDir, secret.CredentialsFile)))
		adopted := filepath.Join(nsDir, secret.AdoptedFile)
		if _, err := os.Lstat(adopted); err == nil {
			out = append(out, "    adopted copy   "+doctorStateOf(adopted)+" — the credential `use --live` displaced; it holds token material at rest, `use --undo` restores it and `accounts remove --delete-secret` clears it")
		}
		pending := filepath.Join(nsDir, secret.PendingFile)
		if _, err := os.Lstat(pending); err == nil {
			out = append(out, "    pending write  "+doctorStateOf(pending)+" — a refresh that could not be renamed into place; the next `status` resolves it", "    pending meta   "+doctorStateOf(filepath.Join(nsDir, secret.PendingMetaFile)))
		}
		stray, err := secret.ListStrayTmp(nsDir)
		if err != nil {
			out = append(out, "    stray tmp      could not be listed: "+err.Error())
		}
		for _, path := range stray {
			out = append(out, "    stray tmp      "+path+" — a crashed write; it holds token material at rest and `login` or `accounts remove` clears it")
		}
		stray, err = secret.ListStrayAdoptedTmp(nsDir)
		if err != nil {
			out = append(out, "    stray tmp      could not be listed: "+err.Error())
		}
		for _, path := range stray {
			out = append(out, "    stray tmp      "+path+" — a crashed adoption; it holds token material at rest and `accounts remove --delete-secret` clears it")
		}
		if notice := doctorLegacyNotice(filepath.Join(nsDir, secret.LegacyStorageWriteArtefact)); notice != "" {
			out = append(out, "    "+notice)
		}
		artefacts = append(artefacts, doctorArtefacts(nsDir)...)
	}
	if owned == 0 {
		return append(out, "  none created by agentctl"), nil
	}
	if len(artefacts) == 0 {
		return append(out, "  no Claude Code lock artefacts"), nil
	}
	var candidates []doctorArtefact
	for _, artefact := range artefacts {
		if artefact.kind.IsDir() {
			candidates = append(candidates, artefact)
			continue
		}
		note := "anomalous (neither a directory nor a regular file — refused)"
		if artefact.kind&os.ModeSymlink != 0 {
			note = "anomalous (a symbolic link — refused)"
		} else if artefact.kind.IsRegular() {
			note = doctorAnomalousFile
		}
		out = append(out, fmt.Sprintf("  %s  %s — agentctl refuses to refresh this namespace and will not remove it", artefact.path, note))
	}
	if len(candidates) == 0 {
		return out, nil
	}
	if err := tell(d.Out, fmt.Sprintf("Found %d Claude Code lock artefact(s); sampling again in %ds to tell a live holder from a lapsed one…", len(candidates), int64(d.interval()/time.Second))); err != nil {
		return nil, err
	}
	// All candidates share one observation window rather than adding a full wait per namespace.
	alive := make([]bool, len(candidates))
	var workers sync.WaitGroup
	for i, artefact := range candidates {
		workers.Go(func() { alive[i] = doctorHolderAlive(ctx, artefact.path, d.interval()) })
	}
	workers.Wait()
	for i, artefact := range candidates {
		state, removal := "not beating", fmt.Sprintf("; `doctor --remove-stale %s --yes` removes it", artefact.path)
		if alive[i] {
			state, removal = "alive (heartbeat seen)", ""
		}
		if !DoctorCanRemoveStale {
			removal = "; " + DoctorStaleRemovalUnsupported
		}
		out = append(out, fmt.Sprintf("  %s  age %ds, holder %s — agentctl refuses to refresh this namespace%s", artefact.path, int64(artefact.age/time.Second), state, removal))
	}
	return out, nil
}

func doctorHolderAlive(ctx context.Context, path string, interval time.Duration) bool {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return true
	}
	defer func() { _ = parent.Close() }()
	at := secret.LockSlot{Dir: int(parent.Fd()), Name: filepath.Base(path), Shown: path}
	return secret.SampleHolderAcrossInterval(ctx, at, interval, secret.SystemClock(), secret.RealFS{})
}
