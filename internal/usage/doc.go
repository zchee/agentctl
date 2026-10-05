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

// Package usage holds the normalized usage vocabulary shared by every
// provider: limit windows, percentages, credits and money.
//
// Three decisions in here are load-bearing rather than stylistic.
//
// A percentage is optional. The API sends percentages as JSON numbers, and
// a JSON number can be a NaN once it has been through a float. NaN compares
// false against everything, so a clamp would silently pass it through and
// the table would print "NaN%". [ClampPercent] rejects it instead and the
// renderer prints an em dash, which is a true statement. Absence is kept
// distinct from zero throughout: a window the server never measured and a
// window at 0% are different facts about an account.
//
// Two percentages are kept, not one. [LimitWindow.Percent] is what the
// server said; [LimitWindow.PercentFloor] is that value floored to a whole
// number. The table shows the floor so that agentctl and the web UI agree
// to the point — the site floors, and rounding half-up here would show 36%
// where the site shows 35%.
//
// Credits distinguish "switched off" from "the response said nothing",
// because those are different facts about an account and only one of them
// is worth acting on.
package usage
