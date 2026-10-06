#!/usr/bin/env bash
set -euo pipefail
completed=0
finish() {
  local status=$?
  if [[ "$completed" != 1 ]]; then
    printf 'FAIL parity sign-off: aborted before final summary\n' >&2
    if [[ "$status" == 0 ]]; then
      status=1
    fi
  fi
  exit "$status"
}
trap finish EXIT
umask 077

# Run from the module root. Captured files are retained for diagnosis.
root=$(pwd -P)
reference=${AGCTL_BIN:-/Users/zchee/rust/src/github.com/zchee/agctl/target/debug/agctl}
for tool in go jq perl diff cmp git shasum grep; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'FAIL prerequisite: %s is unavailable\n' "$tool"
    exit 1
  fi
done
if [[ ! -f "$root/go.mod" || ! -f "$root/fixtures/codex/auth-codex-format.json" ]]; then
  printf 'FAIL prerequisite: run from the module root\n'
  exit 1
fi
if [[ ! -x "$reference" || "$reference" != /* ]]; then
  printf 'FAIL prerequisite: AGCTL_BIN must name an executable absolute path\n'
  exit 1
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-parity.XXXXXX")
active="$work/active"
mkdir -p "$work/captures"
printf 'Evidence: %s\n' "$work"
printf 'Measured: %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
printf 'Reference: %s\n' "$reference"
if source=$(git rev-parse HEAD 2>/dev/null); then
  printf 'Source: %s\n' "$source"
else
  printf 'Source: unavailable (not a git checkout)\n'
fi
GOTOOLCHAIN=go1.27.1 GOFLAGS='' go build -trimpath -ldflags='-s -w' -o "$work/agentctl" "$root"
GOTOOLCHAIN=go1.27.1 go version -m "$work/agentctl" >| "$work/build-info"
if grep -q -- '-tags=' "$work/build-info"; then
  printf 'FAIL untagged release build: artifact contains a -tags= build setting\n' >&2
  exit 1
fi
shasum -a 256 "$reference" "$work/agentctl" >| "$work/artifact-sha256"
printf 'Artifact SHA-256:\n%s\n' "$(< "$work/artifact-sha256")"
printf 'PASS untagged release build (exit=0; no -tags= build setting)\n'

passes=0
failures=0
skips=0
result() {
  local outcome=$1
  shift
  printf '%s %s\n' "$outcome" "$*"
  case "$outcome" in
    PASS) passes=$((passes + 1)) ;;
    FAIL) failures=$((failures + 1)) ;;
    SKIP) skips=$((skips + 1)) ;;
  esac
}

# Replace only clock and process fields, without reserializing JSON.
normalize() {
  local kind=$1 file=$2
  PARITY_KIND="$kind" perl -0777 -pe '
    if ($ENV{PARITY_KIND} eq "registry") {
      s/("created_at"\s*:\s*)"[^"\\]*"/${1}"<clock>"/g;
    } elsif ($ENV{PARITY_KIND} eq "status") {
      s/("(?:generated_at|fetched_at)"\s*:\s*)"[^"\\]*"/${1}"<clock>"/g;
      s/\bagctl\b/agentctl/g;
    } elsif ($ENV{PARITY_KIND} eq "stdout") {
      s/\bagctl\b/agentctl/g;
    } elsif ($ENV{PARITY_KIND} eq "audit") {
      s/("ts"\s*:\s*)"[^"\\]*"/${1}"<clock>"/g;
      s/("agctl_pid"\s*:\s*)[0-9]+/${1}0/g;
    }
  ' "$file"
}

compare() {
  local step=$1 kind=$2
  local left="$work/captures/$step-reference.$kind"
  local right="$work/captures/$step-go.$kind"
  if [[ "$kind" == status ]] && { ! jq -e '.version == 2 and (.rows | type == "array")' "$left" >/dev/null ||
    ! jq -e '.version == 2 and (.rows | type == "array")' "$right" >/dev/null; }; then
    result FAIL "$step status bytes: cannot compare invalid or missing documents"
    return
  fi
  normalize "$kind" "$left" >| "$left.normalized"
  normalize "$kind" "$right" >| "$right.normalized"
  if diff -u "$left.normalized" "$right.normalized" >| "$work/captures/$step.$kind.diff"; then
    result PASS "$step $kind bytes (exit=0)"
  else
    result FAIL "$step $kind bytes (diff exit=1; see $work/captures/$step.$kind.diff)"
  fi
}

# Expired synthetic grants prevent release builds from contacting vendor hosts.
# Re-encode JWT payloads only when creating the fixtures, not captured output.
fixture_auth() {
  local user=$1 account=$2 email=$3
  jq --arg user "$user" --arg account "$account" --arg email "$email" '
    def jwt:
      split(".") as $parts |
      ($parts[1] | @base64d | fromjson |
        .exp = 1 | .email = $email |
        .["https://api.openai.com/auth"].chatgpt_user_id = $user |
        .["https://api.openai.com/auth"].user_id = $user |
        .["https://api.openai.com/auth"].chatgpt_account_id = $account |
        tojson | @base64 | gsub("="; "") | gsub("\\+"; "-") | gsub("/"; "_")) as $payload |
      [$parts[0], $payload, $parts[2]] | join(".");
    .tokens.id_token |= jwt |
    .tokens.access_token |= jwt |
    .tokens.account_id = $account
  ' "$root/fixtures/codex/auth-codex-format.json"
}
owned_user='owned-user'
owned_account=22222222-3333-4444-8555-666666666666
imported_user='user-0001'
imported_account=11111111-2222-4333-8444-555555555555
owned="$owned_user/$owned_account"
imported="$imported_user/$imported_account"
namespace="$active/store/codex/$owned_user/$owned_account"
claude_namespace="$active/store/claude/claude-account/claude-org"
claude_hash=$(printf '%s' "$claude_namespace" | shasum -a 256)
claude_hash=${claude_hash:0:8}
for implementation in reference go; do
  mkdir -p "$active/home" "$active/source" "$active/bin" "$namespace" "$claude_namespace"
  chmod 700 "$active" "$active/home" "$active/source" "$active/bin" "$active/store" \
    "$active/store/codex" "$active/store/codex/$owned_user" "$namespace"
  fixture_auth "$owned_user" "$owned_account" owned@example.invalid >| "$namespace/auth.json"
  fixture_auth "$imported_user" "$imported_account" codex-user@example.invalid >| "$active/source/auth.json"
  printf 'cli_auth_credentials_store = "file"\n' >| "$active/source/config.toml"
  cp "$root/fixtures/fake-security.sh" "$active/bin/security"
  cp "$root/fixtures/fake-codex.sh" "$active/bin/codex"
  chmod 700 "$active/bin/security" "$active/bin/codex"
  ln -s /usr/bin/true "$active/bin/claude"
  printf '{"hasCompletedOnboarding":true,"theme":"dark"}\n' >| "$active/home/.claude.json"
  jq -n --arg user "$owned_user" --arg account "$owned_account" --arg ns "$namespace" \
    --arg claude_ns "$claude_namespace" --arg sha "$claude_hash" \
    '{version:2, accounts:[{
      account_uuid:"claude-account",organization_uuid:"claude-org",
      email:null,org_name:null,label:null,
      kind:{kind:"owned",export_spelling:$claude_ns,export_sha8:$sha},
      forgotten:false,created_at:"2000-01-01T00:00:00Z"}],
      forgotten_services:[], codex_accounts:[{
      chatgpt_user_id:$user, chatgpt_account_id:$account,
      email:"owned@example.invalid", plan_type:"pro", label:null,
      kind:{kind:"owned",export_spelling:$ns,refresh:"auto"},
      forgotten:false,created_at:"2000-01-01T00:00:00Z"}]}' >| "$active/store/config.json"
  chmod 600 "$namespace/auth.json" "$active/source/auth.json" "$active/source/config.toml" "$active/store/config.json"
  mv "$active" "$work/$implementation"
done

run_binary() {
  local binary=$1
  shift
  env -i HOME="$active/home" USER=parity-user LOGNAME=parity-user \
    PATH="$active/bin:/usr/bin:/bin" LANG=C TMPDIR="$work" CODEX_HOME="$active/home/.codex" \
    "$binary" --config-dir "$active/store" "$@"
}

capture_file() {
  local source=$1 target=$2
  if [[ -f "$source" ]]; then
    cp "$source" "$target"
  else
    printf '<absent>\n' >| "$target"
  fi
}

step() {
  local name=$1 expected=$2
  shift 2
  local implementation binary prefix exit_code status_exit
  for implementation in reference go; do
    binary=$reference
    [[ "$implementation" == reference ]] || binary="$work/agentctl"
    prefix="$work/captures/$name-$implementation"
    mv "$work/$implementation" "$active"
    if run_binary "$binary" "$@" >| "$prefix.stdout" 2>| "$prefix.stderr"; then
      exit_code=0
    else
      exit_code=$?
    fi
    printf '%s\n' "$exit_code" >| "$prefix.exit"
    if [[ "$exit_code" == "$expected" ]]; then
      result PASS "$name $implementation command (exit=$exit_code)"
    else
      result FAIL "$name $implementation command (exit=$exit_code; expected=$expected)"
    fi
    capture_file "$active/store/config.json" "$prefix.registry"
    if [[ "$name" == remove-owned ]]; then
      if [[ ! -e "$namespace/auth.json" ]] && \
        jq -e '.outcome == "delete" and .user_id == "owned-user"' "$active/store/codex/writes.jsonl" >/dev/null; then
        result PASS "$name $implementation credential deletion and audit witness"
      else
        result FAIL "$name $implementation credential deletion or audit witness missing"
      fi
    fi
    capture_file "$active/store/codex/writes.jsonl" "$prefix.audit"
    capture_file "$active/store/claude/keychain-writes.jsonl" "$prefix.claude-audit"
    capture_file "$active/store/claude-sessions/claude-account/claude-org/.claude.json" "$prefix.session"
    if ! jq -e 'all(.codex_accounts[]?; .kind.kind != "owned" or .kind.refresh == "never")' \
      "$active/store/config.json" >/dev/null; then
      result FAIL "$name $implementation status refused by harness: automatic refresh is still enabled"
      printf '<not run>\n' >| "$prefix.status"
      status_exit=1
    elif run_binary "$binary" codex status --json --all >| "$prefix.status" 2>| "$prefix.status-stderr"; then
      status_exit=0
    else
      status_exit=$?
    fi
    printf '%s\n' "$status_exit" >| "$prefix.status-exit"
    if [[ "$status_exit" != 0 && "$status_exit" != 2 ]] || \
      ! jq -e '.version == 2 and (.rows | type == "array")' "$prefix.status" >/dev/null; then
      result FAIL "$name $implementation status document (exit=$status_exit)"
    fi
    mv "$active" "$work/$implementation"
  done
  compare "$name" registry
  compare "$name" status
  compare "$name" audit
  compare "$name" claude-audit
  compare "$name" session
  compare "$name" stdout
  if cmp -s "$work/captures/$name-reference.status-exit" "$work/captures/$name-go.status-exit"; then
    result PASS "$name status exit parity (exit=$(< "$work/captures/$name-go.status-exit"))"
  else
    result FAIL "$name status exit parity"
  fi
  result SKIP "$name Claude status v1: release reader pins /usr/bin/security; fake keychain unavailable"
}

step set-never 0 codex accounts set "$owned" --refresh never
step claude-use-isolated 0 claude use claude-account --yes --json
step claude-forget-session 0 claude use --forget claude-account --yes
step claude-undo-empty 0 claude use --undo --yes
step claude-forget-service 0 claude accounts forget "Claude Code-credentials-deadbeef"
step claude-unforget-service 0 claude accounts unforget "Claude Code-credentials-deadbeef"
step import-home 0 codex import --from codex-home --codex-home "$active/source"
step import-repeat 0 codex import --from codex-home --codex-home "$active/source"
step set-read-only 2 codex accounts set "$imported" --refresh never
step forget-owned 0 codex accounts forget "$owned"
step unforget-owned 0 codex accounts unforget "$owned"
step forget-imported 0 codex accounts forget "$imported"
step unforget-imported 0 codex accounts unforget "$imported"
step remove-owned 0 codex accounts remove "$owned" --delete-secret --yes
step remove-imported 0 codex accounts remove "$imported"

for command in 'claude login' 'claude import' 'claude use --live' 'claude use --undo (reversal)' 'claude doctor' 'codex login'; do
  result SKIP "$command: release keychain access cannot be redirected to fixtures"
done
result SKIP 'usage/refresh endpoints: release endpoint overrides are compiled out; no live network requests'
printf 'Summary: PASS=%s FAIL=%s SKIP=%s\n' "$passes" "$failures" "$skips"
printf 'Evidence retained: %s\n' "$work"
completed=1
if (( failures != 0 )); then
  exit 1
fi
printf 'Result: reachable checks passed; full release parity sign-off remains incomplete because of SKIPs\n'
