#!/bin/bash
# Run only in Terminal.app in a prepared disposable macOS guest, never on the host.
set -euo pipefail
umask 077
completed=0
tty_state=
finish() {
  local status=$?
  if [[ -n "$tty_state" ]]; then stty "$tty_state" || status=1; fi
  if [[ "$completed" != 1 ]]; then
    printf 'FAIL guest sequence aborted (exit=%s); evidence retained; reset the clone before retrying\n' "$status" >&2
    [[ "$status" != 0 ]] || status=1
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 1' INT TERM HUP
if [[ $# != 1 ]]; then printf 'Usage: parity-vm-guest.sh <reference|go>\n'; exit 1; fi
implementation=$1
case "$implementation" in reference) executable=agctl ;; go) executable=agentctl ;; *) exit 1 ;; esac
if [[ -n "${SSH_CONNECTION:-}" || -n "${SSH_TTY:-}" || ! -t 0 || ! -t 1 ]]; then
  printf 'FAIL run inside guest Terminal.app, not SSH or a pipe\n' >&2
  exit 1
fi
home="$HOME/agentctl-parity"
[[ -f "$home/guest-only" && "$(< "$home/guest-only")" == disposable-tart-guest ]] || {
  printf 'FAIL this is not a prepared disposable guest\n' >&2
  exit 1
}
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export LC_ALL=C
for tool in jq perl shasum stat; do command -v "$tool" >/dev/null || exit 1; done
# shellcheck source=/dev/null
source "$home/bin/parity-common.sh"
binary="$home/bin/$executable"
[[ -x "$binary" ]] || exit 1
store="$home/store"
captures="$home/captures"
if [[ -e "$store" || -L "$store" ]]; then
  printf 'FAIL store already exists; run host parity-vm.sh reset %s before retrying\n' "$implementation" >&2
  exit 1
fi
[[ -d "$captures" && -z "$(ls -A "$captures")" ]] || {
  printf 'FAIL captures already exist; reset the clone before retrying\n' >&2
  exit 1
}
# No -p: simultaneous or repeated runs must not share an existing store.
mkdir "$store"
printf 'Measured: %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')" >| "$captures/versions.txt"
{
  sw_vers
  claude --version
  codex --version
} >> "$captures/versions.txt"
shasum -a 256 "$binary" >| "$captures/executable-sha256.txt"

pause() {
  printf '%s\nPress Enter when ready: ' "$1"
  read -r _reply
}
credential_metadata() {
  local prefix=$1 namespace name file relative
  : >| "$prefix.credentials"
  : >| "$prefix.credential-sizes"
  # Resolve namespaces from registry kind.export_spelling, never from credential content.
  jq -r '[.accounts[]?, .codex_accounts[]?] | .[] | select(.kind.kind == "owned") | .kind.export_spelling' \
    "$store/config.json" | LC_ALL=C sort -u >| "$prefix.namespaces"
  while IFS= read -r namespace; do
    case "$namespace" in "$store/claude/"*|"$store/codex/"*) ;; *) exit 1 ;; esac
    [[ "$namespace" != *'/../'* && "$namespace" != *$'\n'* ]] || exit 1
    for name in .credentials.json .credentials.adopted.json .credentials.json.pending .pending.meta auth.json auth.json.pending auth.pending.meta; do
      file="$namespace/$name"
      relative=${file#"$store/"}
      if [[ -f "$file" && ! -L "$file" ]]; then
        printf '%s present %s\n' "$relative" "$(stat -f '%Sp' "$file")" >> "$prefix.credentials"
        printf '%s %s\n' "$relative" "$(stat -f '%Sp %z' "$file")" >> "$prefix.credential-sizes"
      elif [[ -e "$file" || -L "$file" ]]; then
        printf '%s refused-nonregular\n' "$relative" >> "$prefix.credentials"
      else printf '%s absent\n' "$relative" >> "$prefix.credentials"; fi
    done
  done < "$prefix.namespaces"
  for file in "$HOME/.claude/.credentials.json" "$HOME/.codex/auth.json"; do
    relative=${file#"$HOME/"}
    if [[ -f "$file" && ! -L "$file" ]]; then
      printf '%s present %s\n' "$relative" "$(stat -f '%Sp' "$file")" >> "$prefix.credentials"
      printf '%s %s\n' "$relative" "$(stat -f '%Sp %z' "$file")" >> "$prefix.credential-sizes"
    else printf '%s absent\n' "$relative" >> "$prefix.credentials"; fi
  done
}
capture() {
  local prefix=$1 provider code
  capture_file "$store/config.json" "$prefix.registry"
  capture_file "$store/codex/writes.jsonl" "$prefix.audit"
  capture_file "$store/claude/keychain-writes.jsonl" "$prefix.claude-audit"
  if [[ -f "$store/config.json" ]]; then credential_metadata "$prefix"
  else printf '<registry absent>\n' >| "$prefix.credentials"; fi
  for provider in claude codex; do
    if "$binary" --config-dir "$store" "$provider" status --json --all \
      >| "$prefix.$provider-status" 2>| "$prefix.$provider-status-stderr"; then code=0
    else code=$?; fi
    printf '%s\n' "$code" >| "$prefix.$provider-status-exit"
    printf 'CAPTURED %s status (exit=%s)\n' "$provider" "$code"
  done
}
step() {
  local name=$1 code prefix
  shift
  prefix="$captures/$name-$implementation"
  printf 'Running %s; command output stays in guest captures. Answer any guest keychain ACL prompts.\n' "$name"
  # Stdin remains a terminal for vendor login and consent, but pasted codes are
  # not echoed and command output never becomes host logs or the handoff.
  tty_state=$(stty -g)
  stty -echo
  if "$binary" --config-dir "$store" "$@" >| "$prefix.stdout" 2>| "$prefix.stderr"; then code=0
  else code=$?; fi
  stty "$tty_state"
  tty_state=
  printf '%s\n' "$code" >| "$prefix.exit"
  printf 'CAPTURED %s command (exit=%s)\n' "$name" "$code"
  capture "$prefix"
  [[ "$code" == 0 ]] || { printf 'FAIL command refused; inspect captures locally, then reset the clone\n' >&2; exit 1; }
}

pause 'Use the ONE disposable Claude account (account A) for this manual login too. The browser opens automatically; paste code#state here and press Enter (input will be hidden). If needed, inspect the local command .stdout/.stderr files in a second guest Terminal for the URL or consent prompt; do not copy them to a report.'
step claude-login claude login --manual --label parity
# Pick exactly one owned label=parity row, not a display-name or email guess.
claude_id=$(jq -er '[.accounts[] | select(.kind.kind == "owned" and .label == "parity")] |
  if length == 1 then .[0] | .account_uuid + "/" + .organization_uuid else error("ambiguous Claude account") end' "$store/config.json")
step claude-import claude import --from keychain
step claude-live claude use "$claude_id" --live --yes --json
if jq -e '.outcome == "already_active"' "$captures/claude-live-$implementation.stdout" >/dev/null 2>&1; then
  printf 'RECORDED already-active outcome; not a reversal test\n' | tee "$captures/forward-undo.txt"
elif jq -e '.outcome == "applied"' "$captures/claude-live-$implementation.stdout" >/dev/null 2>&1; then
  # The manual login above mints a grant newer than the keychain item of the
  # same account, so both implementations install it instead of answering
  # already-active; the undo that follows reverses that credential swap.
  printf 'RECORDED applied outcome on the single account: a newer grant replaced the keychain item; the undo reverses it\n' | tee "$captures/forward-undo.txt"
else
  printf 'FAIL expected the already-active or applied outcome of the single account; no other reversal claim is permitted\n' >&2
  exit 1
fi
if grep -q '^RECORDED applied outcome' "$captures/forward-undo.txt"; then
  # Both implementations discard the displaced grant when it is the incoming
  # account's own, older credential, so this undo has nothing to put back and
  # refuses; the refusal is the recorded fact, not a failure of the sequence.
  undo_prefix="$captures/claude-undo-$implementation"
  tty_state=$(stty -g)
  stty -echo
  if "$binary" --config-dir "$store" claude use --undo --yes --json >| "$undo_prefix.stdout" 2>| "$undo_prefix.stderr"; then undo_code=0
  else undo_code=$?; fi
  stty "$tty_state"
  tty_state=
  printf '%s\n' "$undo_code" >| "$undo_prefix.exit"
  printf 'CAPTURED claude-undo command (exit=%s)\n' "$undo_code"
  capture "$undo_prefix"
  if [[ "$undo_code" == 1 ]] && grep -Eq 'nothing to put back|recorded no displaced credential' "$undo_prefix.stderr"; then
    printf 'RECORDED undo refused after the discarded displaced grant; no reversal on the single account\n' | tee -a "$captures/forward-undo.txt"
  else
    printf 'FAIL expected the undo to refuse with nothing to put back (exit=%s); inspect captures locally, then reset the clone\n' "$undo_code" >&2
    exit 1
  fi
else
  step claude-undo claude use --undo --yes --json
fi
step claude-doctor claude doctor
pause 'Authorize Codex in the guest browser with a disposable account. Inspect local codex-login .stdout/.stderr in a second guest Terminal for any consent question; answer in this terminal. No output or authorization code may be shared.'
step codex-login codex login --no-refresh --label parity
# Codex identity is the registry user/account pair of exactly one owned parity row.
codex_id=$(jq -er '[.codex_accounts[] | select(.kind.kind == "owned" and .label == "parity")] |
  if length == 1 then .[0] | .chatgpt_user_id + "/" + .chatgpt_account_id else error("ambiguous Codex account") end' "$store/config.json")
step codex-auto codex accounts set "$codex_id" --refresh auto
printf 'RECORDED refresh coverage deferred; do not manufacture expired grants\n'
printf '%s\n' "$implementation" >| "$captures/DONE"
completed=1
printf 'DONE\n'
