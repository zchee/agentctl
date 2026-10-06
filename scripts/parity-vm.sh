#!/usr/bin/env bash

# Disposable macOS parity guests are retained by default. Disable Tart's
# automatic pruning because the operator keeps other VMs for end-to-end tests.
# Single-quoted instructions are literal; remote shell expressions expand only in the guest.
# shellcheck disable=SC2016,SC1091
set -euo pipefail
export TART_NO_AUTO_PRUNE=1
export GOTOOLCHAIN=go1.27.1
# Persisted go env can carry GOEXPERIMENT; an empty override does not clear it.
export GOENV=off
# Inherited or discovered workspaces must not change dependency selection.
export GOWORK=off
# Caller-supplied tags must not turn the release artifact into a testing build.
export GOFLAGS=
umask 077
completed=0
lifecycle_lock=
finish() {
  local status=$?
  if [[ -n "$lifecycle_lock" ]]; then
    rm -f "$lifecycle_lock/owner" && rmdir "$lifecycle_lock" || status=1
  fi
  if [[ "$completed" != 1 ]]; then
    printf 'FAIL parity VM: aborted before completion (exit=%s); VMs retained\n' "$status" >&2
    [[ "$status" != 0 ]] || status=1
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 1' INT TERM HUP
root=$(pwd -P)
script_dir=${BASH_SOURCE[0]%/*}
# shellcheck source=scripts/lib/parity-common.sh
source "$script_dir/lib/parity-common.sh"
usage() {
  printf '%s\n' \
    'Usage: scripts/parity-vm.sh <command> [options]' \
    '  prepare [--image IMAGE] [--base NAME]' \
    '  snapshot [--base NAME]' \
    '  run <reference|go> [--base NAME] [--vm NAME] [--fresh]' \
    '  compare [--work ABSOLUTE_PATH]' \
    '  reset <reference|go> [--base NAME] [--vm NAME]' \
    '  destroy [--base NAME | --vm NAME]' \
    'Options: --work ABSOLUTE_PATH, --ssh-user USER' \
    'Default image: macos-golden-gate-xcode; base: agentctl-e2e-base.' \
    'Default work: .omc/artifacts/parity-vm/<local-date>; reuse --work for every command.' \
    'SSH defaults match cirruslabs image templates: admin/admin.' \
    'PARITY_SSH_PASSWORD overrides the disposable guest password; it is never printed.' \
    'Only VMs created and recorded in this work directory may be reused or deleted.' \
    'No VM is removed unless destroy, reset or run --fresh is explicitly requested.'
}
if [[ $# == 0 || "$1" == --help || "$1" == -h ]]; then
  usage
  completed=1
  exit 0
fi
command=$1
shift
implementation=
case "$command" in
  run | reset)
    [[ $# -gt 0 ]] || {
      usage; exit 1
    }
    implementation=$1
    shift
    case "$implementation" in
      reference | go)
        ;;
      *)
        usage; exit 1
        ;;
    esac
    ;;
  prepare | snapshot | compare | destroy) ;;
  *)
    usage; exit 1
    ;;
esac
base=agentctl-e2e-base
vm=
image=macos-golden-gate-xcode
work="$root/.omc/artifacts/parity-vm/$(date '+%Y-%m-%d')"
ssh_user='admin'
fresh=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --work | --base | --vm | --image | --ssh-user)
      [[ $# -ge 2 && -n "$2" ]] || {
        usage; exit 1
      }
      case "$1" in
        --work)
          work=$2
          ;;
        --base)
          base=$2
          ;;
        --vm)
          vm=$2
          ;;
        --image)
          image=$2
          ;;
        --ssh-user)
          ssh_user=$2
          ;;
      esac
      shift 2
      ;;
    --fresh)
      fresh=1
      shift
      ;;
    *)
      usage; exit 1
      ;;
  esac
done
[[ "$work" == /* && "$work" != *$'\n'* && "$work" != *$'\r'* ]] || exit 1
[[ "$base" =~ ^agentctl-[a-zA-Z0-9_-]+$ ]] || exit 1
[[ -z "$vm" || "$vm" =~ ^agentctl-[a-zA-Z0-9_-]+$ ]] || exit 1
[[ "$ssh_user" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 1
[[ -z "$implementation" || -n "$vm" ]] || vm="agentctl-parity-$implementation"
[[ -z "$vm" || "$vm" != "$base" ]] || exit 1
[[ "$fresh" == 0 || "$command" == run ]] || exit 1
mkdir -p "$work/vms"
passes=0
failures=0
skips=0
recorded=0
for tool in jq perl diff cmp shasum; do
  command -v "$tool" >/dev/null || exit 1
done

# This classifier never rewrites captures: the original lexical diff stays local.
# Changed paths must be scalar leaves; added keys or different array lengths fail.
recorded_paths() {
  local kind=$1 left=$2 right=$3
  jq -ers --arg kind "$kind" --slurpfile right "$right" '
    # Only these declared optional leaves admit null, with their non-null type.
    def nullable:
      if $kind == "registry" then {
        "[].accounts.[].email": "string", "[].accounts.[].org_name": "string",
        "[].codex_accounts.[].email": "string", "[].codex_accounts.[].plan_type": "string"
      } elif $kind == "audit" or $kind == "claude-audit" then {
        "[].digest8_before": "string", "[].digest8_after": "string",
        "[].from_digest8": "string", "[].from_sha8": "string", "[].to_sha8": "string",
        "[].hold_ms": "number", "[].after": "string"
      } elif $kind == "claude-status" or $kind == "codex-status" then {
        "[].rows.[].email": "string", "[].rows.[].org_name": "string",
        "[].rows.[].identity.email": "string", "[].rows.[].identity.org_name": "string",
        "[].rows.[].identity.plan_type": "string", "[].rows.[].windows.[].percent": "number",
        "[].rows.[].windows.[].percent_floor": "number", "[].rows.[].windows.[].resets_at": "string",
        "[].rows.[].credits.used_minor": "number", "[].rows.[].credits.limit_minor": "number",
        "[].rows.[].credits.percent": "number", "[].rows.[].next_reset": "string",
        "[].rows.[].session_reset": "string", "[].rows.[].weekly_reset": "string"
      } + (if $kind == "codex-status" then {"[].rows.[].credits.balance": "string"} else {} end)
      else {} end;
    def changes($a; $b; $p):
      if ($a|type) == "object" or ($b|type) == "object" then
        if ($a|type) != ($b|type) or ($a|keys) != ($b|keys) then error("structure")
        else ($a|keys[]) as $k | changes($a[$k]; $b[$k]; $p + [$k]) end
      elif ($a|type) == "array" or ($b|type) == "array" then
        if ($a|type) != ($b|type) or ($a|length) != ($b|length) then error("structure")
        else range(0; $a|length) as $i | changes($a[$i]; $b[$i]; $p + [$i]) end
      elif $a != $b then
        ($p | map(if type == "number" then "[]" else . end) | join(".")) as $path |
        if ($a|type) == ($b|type) or
          (nullable[$path] as $type | $type != null and
            all($a, $b; type == "null" or type == $type))
        then $path else error("scalar type") end
      else empty end;
    # Vendor identities may come from independent authorizations.
    def registry_vendor: [
      "[].accounts.[].account_uuid", "[].accounts.[].organization_uuid",
      "[].accounts.[].email", "[].accounts.[].org_name",
      "[].codex_accounts.[].chatgpt_user_id", "[].codex_accounts.[].chatgpt_account_id",
      "[].codex_accounts.[].email", "[].codex_accounts.[].plan_type"
    ];
    # Independent grants produce different digest prefixes, not different outcomes.
    def audit_vendor: [
      "[].user_id", "[].account_id", "[].digest8_before", "[].digest8_after",
      "[].from_digest8", "[].to_digest8",
      "[].incoming_identity.account_uuid", "[].incoming_identity.organization_uuid",
      "[].account.account_uuid", "[].account.organization_uuid", "[].from_sha8", "[].to_sha8"
    ];
    # Independent invocations have different durations and referenced audit IDs.
    def audit_provenance: ["[].monotonic_ms", "[].hold_ms", "[].after"];
    # Usage snapshots and their vendor identity fields are sampled independently.
    def status_vendor: [
      "[].rows.[].id", "[].rows.[].account_uuid", "[].rows.[].organization_uuid",
      "[].rows.[].email", "[].rows.[].org_name", "[].rows.[].identity.user_id",
      "[].rows.[].identity.account_id", "[].rows.[].identity.email",
      "[].rows.[].identity.org_name", "[].rows.[].identity.plan_type",
      "[].rows.[].windows.[].percent", "[].rows.[].windows.[].percent_floor",
      "[].rows.[].windows.[].resets_at", "[].rows.[].credits.used_minor",
      "[].rows.[].credits.limit_minor", "[].rows.[].credits.percent",
      "[].rows.[].credits.balance", "[].rows.[].next_reset",
      "[].rows.[].session_reset", "[].rows.[].weekly_reset"
    ];
    (if $kind == "registry" then registry_vendor
     elif $kind == "audit" or $kind == "claude-audit" then audit_vendor + audit_provenance
     elif $kind == "claude-status" or $kind == "codex-status" then status_vendor
     else [] end) as $allowed |
    [changes(.; $right; [])] | unique as $paths |
    if ($paths|length) > 0 and all($paths[]; . as $p | $allowed | index($p) != null)
    then $paths | join(", ") else error("unapproved difference") end
  ' "$left" 2>/dev/null
}
compare_capture() {
  local step=$1 kind=$2 normal_kind=$2 left right status paths
  left="$work/captures/reference/$step-reference.$kind"
  right="$work/captures/go/$step-go.$kind"
  if [[ ! -f "$left" || ! -f "$right" ]]; then
    result FAIL "$step $kind: missing capture"
    return
  fi
  case "$kind" in
    claude-status | codex-status)
      normal_kind=status
      local version=1
      [[ "$kind" != codex-status ]] || version=2
      if ! jq -e --argjson version "$version" '.version == $version and (.rows|type == "array")' "$left" >/dev/null 2>&1 \
        || ! jq -e --argjson version "$version" '.version == $version and (.rows|type == "array")' "$right" >/dev/null 2>&1; then
        result FAIL "$step $kind: invalid status document"
        return
      fi
      ;;
    claude-audit) normal_kind=audit ;;
  esac
  normalize "$normal_kind" "$left" >|"$left.normalized"
  normalize "$normal_kind" "$right" >|"$right.normalized"
  if diff -u "$left.normalized" "$right.normalized" >|"$work/captures/$step.$kind.diff"; then
    result PASS "$step $kind bytes (exit=0)"
  else
    status=$?
    [[ "$status" == 1 ]] || exit "$status"
    if paths=$(recorded_paths "$kind" "$left.normalized" "$right.normalized"); then
      recorded=$((recorded + 1))
      printf 'RECORDED %s %s independent-run differences: %s; retained diff\n' "$step" "$kind" "$paths"
    else
      result FAIL "$step $kind bytes (diff exit=1; retained diff)"
    fi
  fi
}
compare_runs() {
  local step kind implementation file code
  for step in claude-login claude-import claude-live claude-undo claude-doctor codex-login codex-auto; do
    for implementation in reference go; do
      file="$work/captures/$implementation/$step-$implementation.exit"
      if [[ ! -f "$file" ]]; then
        result FAIL "$step $implementation command: missing exit"
      else
        code=$(<"$file")
        if [[ "$code" == 0 ]]; then
          result PASS "$step $implementation command (exit=0)"
        elif [[ "$code" =~ ^[0-9]+$ ]]; then
          result FAIL "$step $implementation command (exit=$code)"
        else result FAIL "$step $implementation command: invalid exit capture"; fi
      fi
    done
    for kind in exit registry audit claude-audit credentials claude-status codex-status claude-status-exit codex-status-exit; do
      compare_capture "$step" "$kind"
    done
  done
  for implementation in reference go; do
    if jq -e '.outcome == "already_active"' "$work/captures/$implementation/claude-live-$implementation.stdout" >/dev/null 2>&1; then
      recorded=$((recorded + 1))
      printf 'RECORDED %s already-active outcome; not a reversal test\n' "$implementation"
    else result FAIL "$implementation expected already-active forward outcome not observed"; fi
    if [[ -f "$work/captures/$implementation/DONE" ]] && [[ "$(<"$work/captures/$implementation/DONE")" == "$implementation" ]]; then
      result PASS "$implementation completed guest sequence"
    else result FAIL "$implementation missing DONE marker"; fi
    for kind in claude codex; do
      file="$work/captures/$implementation/codex-auto-$implementation.$kind-status-exit"
      if [[ -f "$file" && "$(<"$file")" == 0 ]] \
        && jq -e '[.rows[] | select(.state == "ok")] | length > 0' \
          "$work/captures/$implementation/codex-auto-$implementation.$kind-status" >/dev/null 2>&1; then
        result PASS "$implementation final $kind real usage success"
      else result FAIL "$implementation final $kind real usage success unavailable"; fi
    done
  done
  result SKIP 'refresh coverage: deferred until a disposable grant legitimately expires'
  printf 'PASS=%s FAIL=%s SKIP=%s RECORDED=%s\n' "$passes" "$failures" "$skips" "$recorded"
  completed=1
  [[ "$failures" == 0 ]]
}
if [[ "$command" == compare ]]; then
  compare_runs
  exit $?
fi
for tool in tart ssh scp sshpass; do
  command -v "$tool" >/dev/null || exit 1
done
# Serialize decisions and marker updates made by this work directory.
if ! mkdir "$work/lifecycle.lock"; then
  owner='unavailable'
  if [[ -f "$work/lifecycle.lock/owner" ]]; then
    IFS= read -r owner <"$work/lifecycle.lock/owner" || true
  fi
  printf 'FAIL lifecycle lock held: %s; owner: %s\n' "$work/lifecycle.lock" "$owner" >&2
  owner_pid=$(printf '%s\n' "$owner" | perl -ne 'print $1 if /^pid=([0-9]+) /')
  if [[ -n "$owner_pid" ]] && ps -p "$owner_pid" >/dev/null 2>&1; then
    printf 'FAIL lifecycle lock owner is active (pid=%s); do not remove the lock\n' "$owner_pid" >&2
  fi
  printf 'Recovery: check `ps -p %s` and establish that no invocation is active; only if the process is gone, run `rm -f %q` then `rmdir %q`. Never remove an active lock.\n' \
    "${owner_pid:-<pid>}" "$work/lifecycle.lock/owner" "$work/lifecycle.lock" >&2
  exit 1
fi
lifecycle_lock="$work/lifecycle.lock"
printf 'pid=%s started=%s command=%s\n' "$$" "$(date '+%Y-%m-%d %H:%M:%S %Z')" "$command" >|"$lifecycle_lock/owner"
tart_home=${TART_HOME:-$HOME/.tart}
[[ "$tart_home" == /* ]] || exit 1
export SSHPASS=${PARITY_SSH_PASSWORD:-admin}
ssh_options=(-o "UserKnownHostsFile=$work/known_hosts" -o StrictHostKeyChecking=accept-new
  -o ConnectTimeout=10 -o LogLevel=ERROR -o PubkeyAuthentication=no -o PreferredAuthentications=password)
ip=
exists() {
  local names
  names=$(tart list --source local --quiet) || exit $?
  grep -Fqx -- "$1" <<<"$names"
}
vm_identity() {
  local directory="$tart_home/vms/$1" identity
  [[ -d "$directory" && ! -L "$directory" && -f "$directory/config.json" ]] || return 1
  # Native stat avoids a GNU stat earlier on PATH interpreting -f differently.
  identity=$(/usr/bin/stat -f '%d:%i:%B' "$directory") || return 1
  jq -ce --arg directory "$directory" --arg identity "$identity" '
    select((.macAddress|type) == "string" and (.ecid|type) == "string") |
    {directory: $directory, identity: $identity, mac_address: .macAddress, ecid: .ecid}
  ' "$directory/config.json"
}
snapshot_digest() {
  local file="$work/base-state.txt"
  [[ -f "$file" ]] || file="$work/base-state.destroyed.txt"
  [[ -f "$file" ]] || return 1
  shasum -a 256 "$file" | perl -ane 'print "$F[0]\n"'
}
# Snapshot metadata is added later; the creation provenance remains immutable.
creation_digest() {
  jq -cS 'del(.base_state_sha256)' "$1" | shasum -a 256 | perl -ane 'print "$F[0]\n"'
}
owned() {
  local name=$1 identity digest nonce provenance sidecar="$tart_home/vms/$1/.agentctl-parity-owner.json"
  if ! exists "$name" || ! identity=$(vm_identity "$name") \
    || ! jq -e --arg name "$name" --argjson identity "$identity" '
      .name == $name and .vm == $identity and
      (.source|type == "string" and length > 0) and
      (.created_at|type == "string" and length > 0) and
      (.base_state_sha256|type == "string")
    ' "$work/vms/$name" >/dev/null 2>&1; then
    printf 'FAIL refusing missing or unowned VM/provenance mismatch: %s (snapshot metadata: %s)\n' "$name" "$work/base-state.txt" >&2
    exit 1
  fi
  nonce=$(jq -r '.nonce // ""' "$work/vms/$name") || exit 1
  provenance=$(creation_digest "$work/vms/$name") || exit 1
  if [[ ! "$nonce" =~ ^[0-9a-f]{32}$ || ! -f "$sidecar" || -L "$sidecar" ]] \
    || ! jq -e --arg nonce "$nonce" --arg digest "$provenance" \
      '.nonce == $nonce and .provenance_sha256 == $digest' "$sidecar" >/dev/null 2>&1; then
    printf 'FAIL VM creation sidecar missing or nonce/provenance mismatch: %s\n' "$name" >&2
    exit 1
  fi
  digest=$(jq -r '.base_state_sha256' "$work/vms/$name")
  if [[ -n "$digest" ]] && [[ "$digest" != "$(snapshot_digest)" ]]; then
    printf 'FAIL VM snapshot provenance mismatch: %s (snapshot metadata: %s)\n' "$name" "$work/base-state.txt" >&2
    exit 1
  fi
}
frozen_base() {
  if ! grep -Fqx "Base: $base" "$work/base-state.txt" || ! exists "$base"; then
    printf 'FAIL stale snapshot metadata: %s; restore the recorded base or select a new work directory\n' "$work/base-state.txt" >&2
    exit 1
  fi
  owned "$base"
  if running "$base" || ! jq -e --arg digest "$(snapshot_digest)" \
    '.base_state_sha256 == $digest' "$work/vms/$base" >/dev/null; then
    printf 'FAIL stale or running snapshot base: %s; stop the recorded base before retrying\n' "$work/base-state.txt" >&2
    exit 1
  fi
}
running() {
  local inventory state
  inventory=$(tart list --source local --format json) || exit $?
  state=$(jq -er --arg name "$1" '
    [.[] | select(.Name == $name)] | select(length == 1) |
    .[0].Running | select(type == "boolean") | tostring
  ' <<<"$inventory") || exit $?
  [[ "$state" == true ]]
}
connect() {
  local name=$1 attempt
  owned "$name"
  if ! running "$name"; then
    # Detach stdin and all output so the hypervisor survives this script returning.
    nohup tart run --no-graphics "$name" </dev/null >|"$work/$name-tart.log" 2>&1 &
  fi
  ip=$(tart ip "$name" --wait 180)
  [[ "$ip" =~ ^[0-9a-fA-F:.]+$ ]] || exit 1
  for ((attempt = 0; attempt < 30; attempt++)); do
    if sshpass -e ssh "${ssh_options[@]}" "$ssh_user@$ip" true; then return; fi
    sleep 2
  done
  printf 'FAIL guest SSH did not become ready\n' >&2
  exit 1
}
remote() { sshpass -e ssh "${ssh_options[@]}" "$ssh_user@$ip" "$@"; }
clone() {
  local source=$1 name=$2 identity digest='' nonce sidecar provenance
  if exists "$name"; then
    owned "$name"
    jq -e --arg source "$source" '.source == $source' "$work/vms/$name" >/dev/null || exit 1
    return
  fi
  # An absent inventory entry revokes any stale ownership before a new attempt.
  rm -f "$work/vms/$name"
  tart clone "$source" "$name" || return $?
  exists "$name" || exit 1
  identity=$(vm_identity "$name") || exit 1
  if [[ "$source" == "$base" ]]; then digest=$(snapshot_digest) || exit 1; fi
  # Never adopt creation evidence inherited from the source VM.
  nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  [[ "$nonce" =~ ^[0-9a-f]{32}$ ]] || exit 1
  jq -n --arg name "$name" --arg source "$source" --arg created "$(date '+%Y-%m-%d %H:%M:%S %Z')" \
    --arg digest "$digest" --argjson identity "$identity" --arg nonce "$nonce" \
    '{name: $name, source: $source, created_at: $created, vm: $identity, base_state_sha256: $digest, nonce: $nonce}' \
    >|"$work/vms/$name.pending"
  provenance=$(creation_digest "$work/vms/$name.pending") || exit 1
  sidecar="$tart_home/vms/$name/.agentctl-parity-owner.json"
  jq -n --arg nonce "$nonce" --arg digest "$provenance" \
    '{nonce: $nonce, provenance_sha256: $digest}' >|"$sidecar.pending"
  chmod 600 "$sidecar.pending"
  mv -f "$sidecar.pending" "$sidecar"
  mv "$work/vms/$name.pending" "$work/vms/$name"
  owned "$name"
}
stop() {
  if running "$1"; then tart stop "$1"; fi
}
delete_vm() {
  local name=$1
  if exists "$name"; then
    owned "$name"
    stop "$name"
    # Recheck after stopping, immediately before the only destructive Tart call.
    owned "$name"
    tart delete "$name"
    if exists "$name" || [[ -e "$tart_home/vms/$name" ]]; then
      printf 'FAIL deletion not verified: %s; ownership retained\n' "$name" >&2
      exit 1
    fi
  fi
  rm -f "$work/vms/$name"
  printf 'PASS destroy %s (other VMs retained)\n' "$name"
}
case "$command" in
  prepare)
    [[ -f "$root/go.mod" ]] || exit 1
    if [[ -f "$work/base-state.txt" ]]; then
      frozen_base
      printf 'PASS base already snapshotted; leave it unchanged\n'
      completed=1
      exit 0
    fi
    reference=${AGCTL_BIN:-/Users/zchee/rust/src/github.com/zchee/agctl/target/debug/agctl}
    [[ "$reference" == /* && -x "$reference" ]] || exit 1
    mkdir -p "$work/bin"
    go build -trimpath -ldflags='-s -w' -o "$work/bin/agentctl" "$root"
    go version -m "$work/bin/agentctl" >|"$work/build-info.txt"
    IFS= read -r build_version <"$work/build-info.txt"
    if [[ "$build_version" != "$work/bin/agentctl: $GOTOOLCHAIN" ]]; then
      printf 'FAIL artifact toolchain: expected %s, got %s\n' "$GOTOOLCHAIN" "$build_version" >&2
      exit 1
    fi
    printf 'PASS artifact toolchain: %s\n' "$GOTOOLCHAIN"
    if grep -q -- '-tags=' "$work/build-info.txt"; then exit 1; fi
    cp "$reference" "$work/bin/agctl"
    shasum -a 256 "$work/bin/agctl" "$work/bin/agentctl" >|"$work/artifact-sha256.txt"
    clone "$image" "$base"
    connect "$base"
    remote /bin/bash -s >|"$work/guest-versions.txt" 2>|"$work/provision.stderr" <<'GUEST'
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HOMEBREW_NO_AUTO_UPDATE=1
mkdir -p "$HOME/agentctl-parity/bin" "$HOME/agentctl-parity/captures"
printf 'disposable-tart-guest\n' >| "$HOME/agentctl-parity/guest-only"
chmod 700 "$HOME/agentctl-parity" "$HOME/agentctl-parity/bin" "$HOME/agentctl-parity/captures"
[[ ! -e "$HOME/agentctl-parity/store" ]] || exit 1
command -v claude >/dev/null || brew install --cask claude-code >| "$HOME/agentctl-parity/install-claude.log" 2>&1
command -v codex >/dev/null || brew install --cask codex >| "$HOME/agentctl-parity/install-codex.log" 2>&1
command -v jq >/dev/null || brew install jq >| "$HOME/agentctl-parity/install-jq.log" 2>&1
sw_vers
claude --version
codex --version
jq --version
GUEST
    sshpass -e scp "${ssh_options[@]}" "$work/bin/agctl" "$work/bin/agentctl" \
      "$script_dir/parity-vm-guest.sh" "$script_dir/lib/parity-common.sh" "$ssh_user@$ip:agentctl-parity/bin/"
    remote 'chmod 700 ~/agentctl-parity/bin/* && shasum -a 256 ~/agentctl-parity/bin/agctl ~/agentctl-parity/bin/agentctl' \
      >|"$work/guest-sha256.txt"
    perl -ane 'print "$F[0]\n"' "$work/artifact-sha256.txt" >|"$work/host-digests.txt"
    perl -ane 'print "$F[0]\n"' "$work/guest-sha256.txt" >|"$work/guest-digests.txt"
    if ! cmp -s "$work/host-digests.txt" "$work/guest-digests.txt"; then
      printf 'FAIL copied executable SHA-256 mismatch\n' >&2
      exit 1
    fi
    stop "$base"
    printf 'PASS prepared %s; untagged artifacts and guest versions recorded in %s\n' "$base" "$work"
    printf 'Operator: open the VM with `tart run %s` (graphics).\n' "$base"
    printf 'Inside the VM, open Terminal.app and run `claude`, then log in with disposable account A.\n'
    printf 'Answer guest keychain prompts; never use your daily-use account. Quit Claude Code afterwards.\n'
    printf 'Then run on the host: scripts/parity-vm.sh snapshot --base %s --work %q\n' "$base" "$work"
    ;;
  snapshot)
    if [[ -f "$work/base-state.txt" ]]; then
      frozen_base
      printf 'PASS snapshot already recorded; base unchanged\n'
    else
      owned "$base"
      connect "$base"
      remote 'export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"; test ! -e ~/agentctl-parity/store && shasum -a 256 ~/agentctl-parity/bin/agctl ~/agentctl-parity/bin/agentctl && sw_vers && claude --version && codex --version' \
        >|"$work/base-state.pending"
      perl -ne 'print "$1\n" if /^([0-9a-f]{64}) /' "$work/base-state.pending" >|"$work/snapshot-digests.txt"
      if ! cmp -s "$work/host-digests.txt" "$work/snapshot-digests.txt"; then
        printf 'FAIL snapshot executable SHA-256 mismatch\n' >&2
        exit 1
      fi
      stop "$base"
      printf 'Measured: %s\nBase: %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')" "$base" >>"$work/base-state.pending"
      mv "$work/base-state.pending" "$work/base-state.txt"
      jq --arg digest "$(snapshot_digest)" '.base_state_sha256 = $digest' "$work/vms/$base" \
        >|"$work/vms/$base.pending"
      mv "$work/vms/$base.pending" "$work/vms/$base"
      frozen_base
      printf 'PASS immutable starting-state snapshot recorded; run only clones after this point\n'
    fi
    ;;
  run | reset)
    [[ -f "$work/base-state.txt" ]] || {
      printf 'FAIL snapshot the base first\n'; exit 1
    }
    frozen_base
    if [[ "$command" == reset || "$fresh" == 1 ]]; then
      if exists "$vm"; then delete_vm "$vm"; fi
      if [[ -e "$work/captures/$implementation" ]]; then
        archive=$(mktemp -d "$work/previous-$implementation.XXXXXX")
        mv "$work/captures/$implementation" "$archive/captures"
        printf 'PASS previous host captures retained at %s\n' "$archive"
      fi
    fi
    clone "$base" "$vm"
    connect "$vm"
    if [[ "$command" == reset ]]; then
      stop "$vm"
      printf 'PASS reset %s from the unchanged base; old host captures retained\n' "$implementation"
    else
      if ! remote 'test ! -e ~/agentctl-parity/store && test ! -e ~/agentctl-parity/captures/DONE && test -z "$(ls -A ~/agentctl-parity/captures)"'; then
        printf 'FAIL clone contains a previous run; use run --fresh or reset %s\n' "$implementation"
        exit 1
      fi
      stop "$vm"
      # Credential reads stay in Terminal.app: a locked guest login keychain can
      # refuse SSH reads, and a new executable may need an operator-approved ACL.
      printf 'Operator: start `tart run %s`, then in guest Terminal.app run:\n' "$vm"
      printf '  ~/agentctl-parity/bin/parity-vm-guest.sh %s\n' "$implementation"
      printf 'Press Enter here only after the guest script prints DONE: '
      read -r _reply
      connect "$vm"
      remote "test \"\$(cat ~/agentctl-parity/captures/DONE)\" = '$implementation'"
      if [[ -e "$work/captures/$implementation" ]]; then
        printf 'FAIL host captures already exist; select a new --work or retain them before retrying\n'
        exit 1
      fi
      mkdir -p "$work/captures/$implementation"
      sshpass -e scp -r "${ssh_options[@]}" "$ssh_user@$ip:agentctl-parity/captures/." "$work/captures/$implementation/"
      printf 'PASS pulled %s captures; VM retained\n' "$implementation"
    fi
    ;;
  destroy)
    if [[ -n "$vm" ]]; then
      delete_vm "$vm"
    else
      delete_vm "$base"
      if [[ -f "$work/base-state.txt" ]]; then mv "$work/base-state.txt" "$work/base-state.destroyed.txt"; fi
    fi
    ;;
esac
completed=1
