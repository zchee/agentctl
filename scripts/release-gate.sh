#!/usr/bin/env bash
# Usage: scripts/release-gate.sh
# Builds isolated release and testing artifacts, checks seam isolation, and parses completions.
set -euo pipefail

unset CDPATH
repo_root=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
export GOTOOLCHAIN=go1.27.2
# Persisted go env can carry GOEXPERIMENT; an empty override does not clear it.
export GOENV=off
# Inherited or discovered workspaces must not change dependency selection.
export GOWORK=off
# Caller-supplied tags must not turn the release artifact into a testing build.
export GOFLAGS=
export LC_ALL=C

for tool in go rg sort comm diff mktemp; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'FAIL required tool: %s is not on PATH\n' "$tool" >&2
		exit 1
	fi
done

work=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-release-gate.XXXXXX")
completed=0
cleanup() {
	local status=$?
	rm -rf -- "$work" || status=1
	if [ "$completed" -ne 1 ]; then
		printf 'FAIL release gate: aborted before final summary\n' >&2
		if [ "$status" -eq 0 ]; then
			status=1
		fi
	fi
	exit "$status"
}
trap cleanup EXIT
failures=0

testing_env=(
	AGENTCTL_CLAUDE_AUTHORIZE_URL
	AGENTCTL_CLAUDE_PROFILE_URL
	AGENTCTL_CLAUDE_TOKEN_URL
	AGENTCTL_CLAUDE_USAGE_URL
	AGENTCTL_CODEX_BIN
	AGENTCTL_CODEX_TOKEN_URL
	AGENTCTL_CODEX_USAGE_URL
	AGENTCTL_FAKE_CODEX_
	AGENTCTL_FAULT
	AGENTCTL_FAULT_RESUME
	AGENTCTL_KEYCHAIN_BACKEND
	AGENTCTL_NO_BROWSER
	AGENTCTL_REMOTE_CONTROL_TIME_SCALE
	AGENTCTL_SECURITY_BIN
	AGENTCTL_SWAP_DEADLINE_MS
	AGENTCTL_TEST_EXIT_DEFERRAL
)
# Pause stems, rather than the runtime-concatenated pause_ prefix, are artifact strings.
fault_points=(
	before_invalid_grant_reread
	before_migrated_reread
	before_migrated_write
	before_refresh_recheck
	before_rename
	before_swap_write
	codex_abort_after_marker
	codex_abort_after_pending
	codex_after_post_snapshot
	codex_before_post_snapshot
	codex_before_rename
	codex_error_after_rename
	codex_install_rename_fail
	codex_login_after_write
	codex_login_before_install
	codex_refresh_state_before_rename
	codex_refresh_state_dir_sync
	codex_refresh_state_file_sync
	codex_refresh_state_rename
	codex_refresh_state_write
	codex_rename_fail
	flock_enotsup
	lock_contended
	lock_resume_after_sample_b
	rename_fail
	swap_lock_leak
	swap_namespace_acquired
	swap_pause_in_locks
	swap_write_fail
)
seams=(
	"${testing_env[@]}"
	"${fault_points[@]}"
	'agentctl lock order violated'
	AGCTL_FAKE_CODEX_
	127.0.0.1:9
)
production=(
	AGENTCTL_CONFIG_DIR
	AGENTCTL_CLAUDE_USER_AGENT
	AGENTCTL_CLAUDE_OAUTH_SCOPES
	AGENTCTL_CODEX_USER_AGENT
)
seam_files=(
	internal/commands/use_live_deadline_testing.go
	internal/commands/use_live_remote_control_testing.go
	internal/provider/claude/browser_testing.go
	internal/provider/claude/oauth_endpoint_testing.go
	internal/provider/claude/oauth_login_endpoint_testing.go
	internal/provider/claude/usage_endpoint_testing.go
	internal/provider/codex/login_child_testing.go
	internal/provider/codex/oauth_endpoint_testing.go
	internal/provider/codex/refresh_fault_testing.go
	internal/provider/codex/usage_endpoint_testing.go
	internal/runtime/fault/fault_testing.go
	internal/runtime/lockorder/witness_testing.go
	internal/runtime/signals/deferral_testing.go
	internal/secret/keychain_backend_testing.go
	internal/secret/namespace_lock_testing.go
	internal/testutil/buildtag_testing.go
)
# Test-only files the testing tag selects with no release counterpart: the
# scripted helpers the test binary links, never a seam the product reads.
testing_only_files=(
	internal/testutil/rcmod_script.go
)

for mode in release testing; do
	args=()
	if [ "$mode" = testing ]; then
		args=(-tags agentctl_testing)
	fi
	if go -C "$repo_root" build ${args[@]+"${args[@]}"} -trimpath -ldflags='-s -w' -o "$work/agentctl-$mode" .; then
		printf 'PASS build %s\n' "$mode"
	else
		printf 'FAIL build %s\n' "$mode" >&2
		exit 1
	fi
	go version -m "$work/agentctl-$mode" >|"$work/$mode-build-info"
	IFS= read -r build_version <"$work/$mode-build-info"
	if [ "$build_version" = "$work/agentctl-$mode: $GOTOOLCHAIN" ]; then
		printf 'PASS toolchain %s: %s\n' "$mode" "$GOTOOLCHAIN"
	else
		printf 'FAIL toolchain %s: expected %s, got %s\n' "$mode" "$GOTOOLCHAIN" "$build_version" >&2
		exit 1
	fi
	go -C "$repo_root" list ${args[@]+"${args[@]}"} -deps -f '{{range .GoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' ./... |
		rg -v '^$' | sort -u >|"$work/$mode-files"
done

printf 'Source selection diff (release -> testing):\n'
if diff -u "$work/release-files" "$work/testing-files"; then
	printf 'FAIL source selection: the testing tag selected no different files\n' >&2
	failures=$((failures + 1))
else
	status=$?
	if [ "$status" -ne 1 ]; then
		exit "$status"
	fi
fi
comm -13 "$work/release-files" "$work/testing-files" >|"$work/testing-only"
comm -23 "$work/release-files" "$work/testing-files" >|"$work/release-only"
for file in "${seam_files[@]}"; do
	printf '%s/%s\n' "$repo_root" "$file"
done | sort -u >|"$work/expected-testing"
for file in "${testing_only_files[@]}"; do
	printf '%s/%s\n' "$repo_root" "$file"
done | sort -u -m - "$work/expected-testing" >|"$work/expected-testing.all"
mv -f "$work/expected-testing.all" "$work/expected-testing"
for file in "${seam_files[@]}"; do
	printf '%s/%s\n' "$repo_root" "${file%_testing.go}_release.go"
done | sort -u >|"$work/expected-release"
for mode in testing release; do
	if diff -u "$work/expected-$mode" "$work/$mode-only"; then
		printf 'PASS source selection %s: only the declared seam files differ\n' "$mode"
	else
		printf 'FAIL source selection %s\n' "$mode" >&2
		failures=$((failures + 1))
	fi
done

# Discover names from the selected tagged sources so a new override cannot evade the list.
tagged_sources=()
while IFS= read -r file; do
	tagged_sources+=("$file")
done <"$work/testing-only"
if [ "${#tagged_sources[@]}" -eq 0 ]; then
	printf 'FAIL testing environment inventory: no tagged sources selected\n' >&2
	exit 1
fi
rg --no-filename -o 'AGENTCTL_[A-Z0-9_]+' ${tagged_sources[@]+"${tagged_sources[@]}"} | sort -u >|"$work/discovered-env"
printf '%s\n' "${testing_env[@]}" | sort -u >|"$work/expected-env"
if diff -u "$work/expected-env" "$work/discovered-env"; then
	printf 'PASS testing environment inventory: %s names\n' "${#testing_env[@]}"
else
	printf 'FAIL testing environment inventory: update the artifact scan list\n' >&2
	failures=$((failures + 1))
fi

# Fault-point names are declared only in this exported constant block; callers
# reference the constants, so the block is the inventory to scan for.
# A Go test rejects fault-point names outside this exported constant inventory.
# This gate checks that the inventory and artifact scans agree.
rg --no-filename -o --replace '$2' '^[[:space:]]+[A-Z][A-Za-z0-9_]*([[:space:]]+string)?[[:space:]]*=[[:space:]]*"([a-z][a-z0-9_]*)"$' \
	"$repo_root/internal/runtime/fault/fault_testing.go" | sort -u >|"$work/discovered-faults"
printf '%s\n' "${fault_points[@]}" lock_stale | sort -u >|"$work/expected-faults"
if diff -u "$work/expected-faults" "$work/discovered-faults"; then
	printf 'PASS fault point inventory: %s strict stems and 1 shared production string\n' "${#fault_points[@]}"
else
	printf 'FAIL fault point inventory: update the artifact scan list\n' >&2
	failures=$((failures + 1))
fi

count_matches() {
	local count status
	if count=$(rg -a -c -F -- "$1" "$2"); then
		printf '%s\n' "$count"
	else
		status=$?
		if [ "$status" -eq 1 ]; then
			printf '0\n'
		else
			printf 'FAIL artifact scan for %s (rg exit %s)\n' "$1" "$status" >&2
			return "$status"
		fi
	fi
}

for name in "${seams[@]}"; do
	release_count=$(count_matches "$name" "$work/agentctl-release")
	testing_count=$(count_matches "$name" "$work/agentctl-testing")
	if [ "$release_count" -eq 0 ] && [ "$testing_count" -gt 0 ]; then
		printf 'PASS seam %s: release=%s testing=%s\n' "$name" "$release_count" "$testing_count"
	else
		printf 'FAIL seam %s: release=%s testing=%s\n' "$name" "$release_count" "$testing_count" >&2
		failures=$((failures + 1))
	fi
done
# The production audit reason value shares lock_stale, so it cannot be release-absent.
release_count=$(count_matches lock_stale "$work/agentctl-release")
testing_count=$(count_matches lock_stale "$work/agentctl-testing")
printf 'INFO fault string lock_stale: release=%s testing=%s (production audit reason value shares the string; not a release gate)\n' "$release_count" "$testing_count"
for name in "${production[@]}"; do
	count=$(count_matches "$name" "$work/agentctl-release")
	if [ "$count" -gt 0 ]; then
		printf 'PASS production %s: %s\n' "$name" "$count"
	else
		printf 'FAIL production %s: 0\n' "$name" >&2
		failures=$((failures + 1))
	fi
done

for shell in bash zsh fish powershell; do
	completion="$work/completions-$shell"
	if ! "$work/agentctl-release" completions "$shell" >|"$completion"; then
		printf 'FAIL completions %s generation\n' "$shell" >&2
		failures=$((failures + 1))
		continue
	fi
	printf 'PASS completions %s generation\n' "$shell"
	parser=$shell
	if [ "$shell" = powershell ]; then
		parser=pwsh
	fi
	if ! command -v "$parser" >/dev/null 2>&1; then
		printf 'SKIP completions %s syntax: %s is not installed\n' "$shell" "$parser"
		continue
	fi
	if [ "$shell" = powershell ]; then
		# PowerShell, not Bash, expands these variables.
		# shellcheck disable=SC2016
		if COMPLETION_FILE="$completion" pwsh -NoLogo -NoProfile -NonInteractive -Command \
			'$tokens = $null; $parseErrors = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($env:COMPLETION_FILE, [ref]$tokens, [ref]$parseErrors); if ($parseErrors.Count -gt 0) { $parseErrors | Out-String | Write-Error; exit 1 }'; then
			printf 'PASS completions %s syntax\n' "$shell"
		else
			printf 'FAIL completions %s syntax\n' "$shell" >&2
			failures=$((failures + 1))
		fi
	elif "$parser" -n "$completion"; then
		printf 'PASS completions %s syntax\n' "$shell"
	else
		printf 'FAIL completions %s syntax\n' "$shell" >&2
		failures=$((failures + 1))
	fi
done

status=0
"$work/agentctl-release" completions elvish >|"$work/elvish-out" 2>|"$work/elvish-err" || status=$?
expected='agentctl: invalid value "elvish" for the shell argument: supported shells are bash, zsh, fish, and powershell'
printf '%s\n' "$expected" >|"$work/elvish-expected"
if [ "$status" -eq 2 ] && [ ! -s "$work/elvish-out" ] && diff -u "$work/elvish-expected" "$work/elvish-err"; then
	printf 'PASS completions elvish refusal: exit=2, exact diagnostic\n'
else
	printf 'FAIL completions elvish refusal: exit=%s\n' "$status" >&2
	failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
	printf 'FAIL release gate: %s failed checks\n' "$failures" >&2
	exit 1
fi
printf 'PASS release gate: %s seams absent, tagged controls present, %s production controls present\n' "${#seams[@]}" "${#production[@]}"
completed=1
