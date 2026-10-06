#!/usr/bin/env bash
# Shared capture helpers. Callers initialize work, passes, failures and skips.
# work and active are supplied by the harness sourcing this library.
# shellcheck disable=SC2154

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
