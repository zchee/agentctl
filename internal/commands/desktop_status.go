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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/zchee/agentctl/internal/cli"
	"github.com/zchee/agentctl/internal/provider/claude/desktop"
	rdesktop "github.com/zchee/agentctl/internal/runtime/desktop"
)

// The words status uses for what a profile holds.
const (
	profileAccount    = "account"
	profileForeign    = "foreign"
	profileFresh      = "fresh"
	profileUnknown    = "unknown"
	profileUnreadable = "unreadable"
)

// statusReport is the JSON document of `claude desktop status`.
type statusReport struct {
	Version  int             `json:"version"`
	Command  string          `json:"command"`
	Default  statusDefault   `json:"default"`
	Profiles []statusProfile `json:"profiles"`
	Open     []statusIntent  `json:"open_intents"`
	Errors   []statusError   `json:"errors"`
}

// statusDefault describes the data directory and the app's processes. The
// counts are pointers: a count that could not be observed is null, never 0.
type statusDefault struct {
	Path        string                 `json:"path"`
	Present     bool                   `json:"present"`
	Identity    desktop.IdentityRecord `json:"identity"`
	Word        string                 `json:"word"`
	Reason      string                 `json:"reason,omitzero"`
	Running     *int                   `json:"running"`
	Family      *int                   `json:"family"`
	FamilyRows  []rdesktop.FamilyProc  `json:"family_rows"`
	Foreign     []rdesktop.ForeignProc `json:"foreign"`
	OtherCopies []rdesktop.Proc        `json:"other_copies"`
}

// statusProfile is one stored profile.
type statusProfile struct {
	Name      string                 `json:"name"`
	Identity  desktop.IdentityRecord `json:"identity"`
	Word      string                 `json:"word"`
	Duplicate bool                   `json:"duplicate"`
	InPlace   bool                   `json:"in_place"`
}

// statusIntent is an operation that recorded its intent and no result.
type statusIntent struct {
	OpID             string `json:"op_id"`
	Time             string `json:"time"`
	Command          string `json:"command"`
	RequestedAccount string `json:"requested_account"`
	Outgoing         string `json:"outgoing,omitzero"`
	Target           string `json:"target,omitzero"`
}

// statusError names one observation status could not make.
type statusError struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// profileWord classifies what a stored or default profile holds.
func profileWord(id desktop.Identity, known map[string]bool) string {
	switch id.State {
	case desktop.StateUUID:
		if known[id.UUID] {
			return profileAccount
		}
		return profileForeign
	case desktop.StateFresh:
		return profileFresh
	case desktop.StateUnknown:
		return profileUnknown
	default:
		return profileUnreadable
	}
}

// RunStatus reports the data directory, every stored profile, the app's
// processes and any interrupted operation. It reads and never moves. A
// failed observation leaves its fields null, is named in errors and makes
// the run exit with the desktop refusal status.
func (p DesktopProcess) RunStatus(ctx context.Context, globals cli.Globals, opts cli.ClaudeDesktopStatusOptions) error {
	env, err := p.newEnv()
	if err != nil {
		return err
	}
	report := statusReport{
		Version: 1, Command: "status",
		Profiles: []statusProfile{}, Open: []statusIntent{}, Errors: []statusError{},
		Default: statusDefault{
			Path: env.layout.DefaultData, FamilyRows: []rdesktop.FamilyProc{},
			Foreign: []rdesktop.ForeignProc{}, OtherCopies: []rdesktop.Proc{},
		},
	}
	fail := func(step string, err error) {
		report.Errors = append(report.Errors, statusError{Step: step, Message: err.Error()})
	}

	known, err := registeredAccounts(ctx, globals)
	if err != nil {
		fail("registry", err)
	}

	present, err := desktop.Exists(env.layout.DefaultData)
	if err != nil {
		fail("default", err)
	}
	report.Default.Present = present
	// An absent data path holds no config.json, which is what fresh means.
	defaultID := desktop.Identity{State: desktop.StateFresh}
	if present {
		defaultID = desktop.ReadDesktopIdentity(env.layout.DefaultData)
	}
	report.Default.Identity = desktop.RecordOf(defaultID)
	report.Default.Word = profileWord(defaultID, known)
	report.Default.Reason = defaultID.Reason

	if snap, err := env.sys.Observer.Snapshot(ctx); err != nil {
		fail("processes", err)
	} else {
		running, family := len(snap.Mains), len(snap.Family)
		report.Default.Running, report.Default.Family = &running, &family
		report.Default.FamilyRows = append(report.Default.FamilyRows, snap.Family...)
		report.Default.Foreign = append(report.Default.Foreign, snap.Foreign...)
		report.Default.OtherCopies = append(report.Default.OtherCopies, snap.OtherCopies...)
	}

	inPlace := ""
	if entries, err := desktop.ReadJournal(env.layout.Journal); err != nil {
		fail("journal", err)
	} else {
		for _, in := range desktop.OpenIntents(entries) {
			report.Open = append(report.Open, intentRow(in))
		}
		inPlace = inPlaceName(entries)
	}

	names, err := storedNames(env.layout.ProfilesDir)
	if err != nil {
		fail("profiles", err)
	}
	for _, name := range names {
		id := desktop.ReadDesktopIdentity(env.layout.Profile(name))
		report.Profiles = append(report.Profiles, statusProfile{
			Name: name, Identity: desktop.RecordOf(id), Word: profileWord(id, known),
			Duplicate: id.State == desktop.StateUUID && defaultID.State == desktop.StateUUID && id.UUID == defaultID.UUID,
			InPlace:   name == inPlace,
		})
	}

	if opts.JSON {
		if err := writeJSON(env.out, report); err != nil {
			return err
		}
	} else if err := env.prompt.Tell(renderStatus(report)); err != nil {
		return err
	}
	if len(report.Errors) > 0 {
		steps := make([]string, 0, len(report.Errors))
		for _, e := range report.Errors {
			steps = append(steps, e.Step)
		}
		return refuse(desktopObserveFailed, "status could not read: %s", strings.Join(steps, ", "))
	}
	return nil
}

// intentRow summarizes an open intent.
func intentRow(in *desktop.Intent) statusIntent {
	row := statusIntent{OpID: in.OpID, Time: in.Time, Command: in.Command, RequestedAccount: in.RequestedAccount, Target: in.Target.Name}
	if in.Outgoing != nil {
		row.Outgoing = in.Outgoing.Name
	}
	return row
}

// inPlaceName returns the stored name of the profile the latest applied
// result put at the data path. That profile lives at the data path, not
// under profiles/, until another switch moves it out under the same name.
func inPlaceName(entries []desktop.Entry) string {
	res := desktop.LatestApplied(entries)
	if res == nil {
		return ""
	}
	if in := desktop.IntentFor(entries, res.OpID); in != nil {
		return in.Target.Name
	}
	return ""
}

// storedNames lists the stored profile names in profiles/, sorted. An
// entry whose name is outside the grammar is not a stored profile and is
// left out.
func storedNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if _, err := desktop.ParseProfileName(e.Name()); err == nil {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// renderStatus prints the status report as lines for a person.
func renderStatus(r statusReport) string {
	var b strings.Builder
	count := func(n *int) string {
		if n == nil {
			return "unknown"
		}
		return fmt.Sprint(*n)
	}
	fmt.Fprintf(&b, "data path  %s\n", r.Default.Path)
	switch {
	case !r.Default.Present:
		b.WriteString("  profile  none (the app has not created it)\n")
	case r.Default.Identity.UUID != "":
		fmt.Fprintf(&b, "  profile  %s %s\n", r.Default.Word, uuidPrefix(r.Default.Identity.UUID))
	default:
		fmt.Fprintf(&b, "  profile  %s\n", r.Default.Word)
	}
	fmt.Fprintf(&b, "  running  %s main, %s family\n", count(r.Default.Running), count(r.Default.Family))
	for _, f := range r.Default.FamilyRows {
		fmt.Fprintf(&b, "    family   pid %d parent %d %s (%s)\n", f.PID, f.PPID, f.Path, f.Reason)
	}
	for _, f := range r.Default.Foreign {
		fmt.Fprintf(&b, "    foreign  pid %d parent %d %s under %s\n", f.PID, f.PPID, f.Path, f.ParentPath)
	}
	for _, o := range r.Default.OtherCopies {
		fmt.Fprintf(&b, "    other copy pid %d %s\n", o.PID, o.Path)
	}
	if len(r.Profiles) == 0 {
		b.WriteString("stored profiles  none\n")
	} else {
		b.WriteString("stored profiles\n")
	}
	for _, p := range r.Profiles {
		line := "  " + p.Name + "  " + p.Word
		if p.Duplicate {
			line += "  duplicate"
		}
		if p.InPlace {
			line += "  in place"
		}
		b.WriteString(line + "\n")
	}
	for _, in := range r.Open {
		fmt.Fprintf(&b, "interrupted %s %s (account %s): run `agentctl claude desktop repair`\n", in.Command, in.OpID, in.RequestedAccount)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "error  %s: %s\n", e.Step, e.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}
