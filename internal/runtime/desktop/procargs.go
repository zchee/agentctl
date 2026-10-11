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

package desktop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ExecutablePath extracts the executable path from a kern.procargs2
// block: a native-endian argc word, the NUL-terminated path the process
// was started from, NUL padding, then the arguments. Only the path is
// read. The kernel copies the block from the top of the process's own
// stack, where the path was placed at exec, so the process can rewrite it
// like the rest of that area. It is still required: a block too short for
// the word, without a NUL after the path or with an empty path is
// unusable, and that is a failed read and an error. Every failure goes the
// closed way: a failed read fails the snapshot, which blocks every move,
// and a rewritten path can only hide the process that rewrote it or make
// it count as one of the app's, which also blocks a move. The arguments
// after it are not counted against argc: a process may overwrite its own
// argument area to retitle itself, and failing on that would make every
// snapshot fail for as long as such a process runs.
func ExecutablePath(block []byte) (string, error) {
	const word = 4
	if len(block) <= word {
		return "", errors.New("the argument block is empty")
	}
	if argc := binary.NativeEndian.Uint32(block[:word]); argc > 1<<20 {
		return "", fmt.Errorf("the argument block's count %d is implausible", argc)
	}
	rest := block[word:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", errors.New("the argument block is truncated")
	}
	if end == 0 {
		return "", errors.New("the argument block names no executable")
	}
	return string(rest[:end]), nil
}
