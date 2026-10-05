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
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/agentctl/internal/config"
	"github.com/zchee/agentctl/internal/provider/claude"
	"github.com/zchee/agentctl/internal/secret"
)

func TestImportItems(t *testing.T) {
	home := t.TempDir()
	live := filepath.Join(home, ".claude")
	alias := filepath.Join(home, "alias")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, alias); err != nil {
		t.Fatal(err)
	}
	canonical, err := claude.Canonical(live)
	if err != nil {
		t.Fatal(err)
	}
	env := claude.EnvWithHome(home)
	aliasService := claude.LiveService + "-" + claude.SHA8(alias)
	canonicalService := claude.LiveService + "-" + claude.SHA8(claude.ExportSpelling(canonical))
	tests := map[string]struct {
		dirs    []string
		listing []secret.ServiceEntry
		want    []config.ImportItem
	}{
		"success: named alias":                      {dirs: []string{alias}, listing: []secret.ServiceEntry{{Service: aliasService}}, want: []config.ImportItem{{Service: aliasService, Dir: &alias, Listed: true, SharesLiveDir: true}}},
		"success: canonical sibling":                {listing: []secret.ServiceEntry{{Service: canonicalService}}, want: []config.ImportItem{{Service: canonicalService, Listed: true, SharesLiveDir: true}}},
		"success: noncredentials excluded":          {listing: []secret.ServiceEntry{{Service: "Claude Code-12345678"}, {Service: "claude-switcher:one"}, {Service: claude.LiveService}}},
		"success: raw directory spelling preserved": {dirs: []string{"/work/../work/"}, want: []config.ImportItem{{Service: claude.LiveService + "-" + claude.SHA8("/work/../work/"), Dir: new("/work/../work/")}}},
		"success: empty spelling":                   {dirs: []string{""}, want: []config.ImportItem{{Service: claude.LiveService, Dir: new(""), Unsuffixed: true}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, importItems(tt.dirs, tt.listing, &env)); diff != "" {
				t.Fatalf("items (-want +got):\n%s", diff)
			}
		})
	}
	if ImportDeadline != 60*time.Second {
		t.Fatalf("deadline = %v", ImportDeadline)
	}
}
