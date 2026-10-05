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

package testutil

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"

	"github.com/rogpeppe/go-internal/testscript"
)

func init() {
	registerScriptCmd("swap-attribution-server", func(ts *testscript.TestScript, neg bool, args []string) {
		if neg || len(args) != 1 {
			ts.Fatalf("usage: swap-attribution-server <200|401|403|500|malformed>")
		}
		status := http.StatusOK
		malformed := args[0] == "malformed"
		if !malformed {
			var err error
			status, err = strconv.Atoi(args[0])
			ts.Check(err)
			if status != 200 && status != 401 && status != 403 && status != 500 {
				ts.Fatalf("unsupported outgoing profile status")
			}
		}
		owner, incoming := ts.Getenv("OWNER"), ts.Getenv("INCOMING")
		namespace := "namespace:" + Sha8(ExportSpelling(ts.Getenv("UNDO_OWNER_DIR")))
		ts.Setenv("ATTRIBUTION_NAMESPACE_TARGET", namespace)
		ts.SetCmd("swap-attribution-history", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 0 {
				ts.Fatalf("usage: swap-attribution-history")
			}
			var write struct {
				Event     string `json:"event"`
				Target    string `json:"target"`
				ToDigest8 string `json:"to_digest8"`
			}
			ts.Check(json.Unmarshal([]byte(ts.ReadFile(ts.Getenv("SWAP_AUDIT"))), &write))
			if write.Event != "write" || write.Target != namespace || write.ToDigest8 != Sha8("SENTINEL-access-outgoing-attribution") {
				ts.Fatalf("namespace attribution fixture does not describe this namespace and credential")
			}
		})
		var outgoingGET, incomingGET, posts, unexpected atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			account := ""
			switch r.Header.Get("Authorization") {
			case "Bearer SENTINEL-access-outgoing-attribution":
				outgoingGET.Add(1)
				if status != http.StatusOK {
					w.WriteHeader(status)
					return
				}
				if malformed {
					say(w, `{"account":{"uuid":"unusable-profile"}}`)
					return
				}
				account = owner
			case "Bearer SENTINEL-access-incoming":
				incomingGET.Add(1)
				account = incoming
			default:
				unexpected.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			say(w, `{"account":{"uuid":%q,"email":"user@example.invalid"},"organization":{"uuid":%q,"name":"Example"}}`, account, Org)
		}))
		ts.Defer(server.Close)
		ts.Setenv("AGENTCTL_CLAUDE_TOKEN_URL", server.URL+"/token")
		ts.Setenv("AGENTCTL_CLAUDE_PROFILE_URL", server.URL+"/profile")
		ts.SetCmd("swap-attribution-check", func(ts *testscript.TestScript, neg bool, args []string) {
			if neg || len(args) != 3 {
				ts.Fatalf("usage: swap-attribution-check <outgoing-GET> <incoming-GET> <POST>")
			}
			for i, got := range []int32{outgoingGET.Load(), incomingGET.Load(), posts.Load()} {
				want, err := strconv.ParseInt(args[i], 10, 32)
				ts.Check(err)
				if got != int32(want) {
					ts.Fatalf("profile/POST class %d=%d; want %d", i, got, want)
				}
			}
			if unexpected.Load() != 0 {
				ts.Fatalf("unexpected profile request")
			}
		})
	})
}
