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

//go:build !darwin

package desktop

import "context"

func platformDefault(string) (*System, error) { return nil, ErrUnsupportedPlatform }

// SystemLister refuses: process observation exists only on macOS.
func SystemLister(context.Context, uint32) ([]Record, error) { return nil, ErrUnsupportedPlatform }

// StatDevice refuses: the desktop switch exists only on macOS.
func StatDevice(string) (uint64, error) { return 0, ErrUnsupportedPlatform }
