#!/bin/sh
# A stand-in for security(1): the read subcommands, plus the write as
# `add-generic-password` with the payload in argv. Nothing is read from
# stdin; `-i` is refused like any other subcommand this stand-in lacks.
#
# Write knobs:
#   AGCTL_FAKE_SECURITY_WRITE_EXIT    exit with this code before storing, after
#                                     printing AGCTL_FAKE_SECURITY_STDERR.
#   AGCTL_FAKE_WRITE_DELAY_MS         sleep this many milliseconds before storing.
#   AGCTL_FAKE_WRITE_DELAY_AFTER_MS   sleep this many milliseconds after storing,
#                                     so a timeout lands after the commit.
#   AGCTL_FAKE_SECURITY_HOLD_PIPES    after storing, leave a background `sleep`
#                                     of this many seconds holding stdout and
#                                     stderr open, and exit 0 at once. It is the
#                                     only process this stand-in forks that
#                                     outlives it, and it stays in its process
#                                     group.
#   AGCTL_FAKE_SECURITY_PIDFILE       write this shell's pid there before the
#                                     first delay, and while delaying write a
#                                     beat counter to "<pidfile>.heartbeat"
#                                     before every 10 ms step and before the
#                                     remainder of a delay that is not a whole
#                                     number of steps, so a positive delay
#                                     beats ceil(ms / 10) times. The beat comes
#                                     from this shell's own foreground loop,
#                                     never from a background job, so it stops
#                                     the moment this shell is killed.
# The delay and hold values are decimal counts; leading zeros are dropped, so
# 010 means ten under every shell, never the octal eight.
#
# Read knob:
#   AGCTL_FAKE_SECURITY_FIND_EXIT_IF  once this file exists, every
#                                     find-generic-password exits with
#                                     AGCTL_FAKE_SECURITY_FIND_EXIT_IF_CODE
#                                     (default 36) instead of reading, so a test
#                                     can let the first reads through and fail a
#                                     later one.
#
# Every path knob (ITEMS, LOG, DUMP, DUMP_EXIT_IF, FIND_EXIT_IF, PIDFILE) must
# name a place under AGCTL_FAKE_SECURITY_ROOT, the fixture directory the
# harness exports. A path knob that is set while the root is not, or that
# points outside the root or climbs out of it through "..", stops the
# stand-in before it logs, creates or appends anything.

# confine_path stops the stand-in when the path knob named $1, whose value
# is $2, is set and does not name a place under the fixture root.
confine_path() {
    if [ -z "$2" ]; then
        return 0
    fi
    root=${AGCTL_FAKE_SECURITY_ROOT:-}
    root=${root%/}
    case "$root" in
    /?*) ;;
    *)
        printf 'security: %s is set, but AGCTL_FAKE_SECURITY_ROOT is not an absolute directory\n' "$1" >&2
        exit 1
        ;;
    esac
    case "$root/" in
    */../* | */./*)
        printf 'security: AGCTL_FAKE_SECURITY_ROOT climbs through a dot segment\n' >&2
        exit 1
        ;;
    esac
    case "$2" in
    */../* | */.. | */./* | */.)
        printf 'security: %s climbs through a dot segment\n' "$1" >&2
        exit 1
        ;;
    "$root"/?*) ;;
    *)
        printf 'security: %s is outside AGCTL_FAKE_SECURITY_ROOT\n' "$1" >&2
        exit 1
        ;;
    esac
}

confine_path AGCTL_FAKE_SECURITY_LOG "${AGCTL_FAKE_SECURITY_LOG:-}"
confine_path AGCTL_FAKE_SECURITY_ITEMS "${AGCTL_FAKE_SECURITY_ITEMS:-}"
confine_path AGCTL_FAKE_SECURITY_DUMP "${AGCTL_FAKE_SECURITY_DUMP:-}"
confine_path AGCTL_FAKE_SECURITY_DUMP_EXIT_IF "${AGCTL_FAKE_SECURITY_DUMP_EXIT_IF:-}"
confine_path AGCTL_FAKE_SECURITY_FIND_EXIT_IF "${AGCTL_FAKE_SECURITY_FIND_EXIT_IF:-}"
confine_path AGCTL_FAKE_SECURITY_PIDFILE "${AGCTL_FAKE_SECURITY_PIDFILE:-}"

log_line() {
    if [ -n "${AGCTL_FAKE_SECURITY_LOG:-}" ]; then
        printf '%s\n' "$1" >> "$AGCTL_FAKE_SECURITY_LOG"
    fi
}

# An argv write carries the payload in -X, so the generic argv line would put
# a credential in the log. Such a write is logged here instead, before
# anything else runs, with the payload reduced to its length. An argv that is
# not exactly the shape the writer issues is logged without any of its values
# and refused below.
argv_write=''
if [ "${1:-}" = add-generic-password ]; then
    if [ $# -eq 8 ] && [ "$2" = -U ] && [ "$3" = -a ] && [ -n "$4" ] &&
        [ "$5" = -s ] && [ -n "$6" ] && [ "$7" = -X ]; then
        account=$4
        service=$6
        hex=$8
        hexlen=${#hex}
        log_line "add-generic-password -U -a \"$account\" -s \"$service\" -X <REDACTED:$hexlen>"
        argv_write=1
    else
        log_line "add-generic-password <REFUSED:$(($# - 1)) arguments>"
    fi
else
    log_line "$*"
fi
if [ -n "${AGCTL_FAKE_SECURITY_SLEEP:-}" ]; then
    sleep "$AGCTL_FAKE_SECURITY_SLEEP"
fi

# decimal_count prints the knob named $1, whose value is $2, as a decimal
# count without leading zeros, so no shell's arithmetic reads it as octal. A
# value that is not a plain decimal count fails the write loudly instead of
# silently skipping the delay.
decimal_count() {
    case "$2" in
    '' | *[!0-9]*)
        printf 'security: %s must be a decimal count, and is %s\n' "$1" "$2" >&2
        exit 1
        ;;
    esac
    digits=${2#"${2%%[!0]*}"}
    printf '%s\n' "${digits:-0}"
}

beat=0

# heartbeat writes the next beat, from this shell.
heartbeat() {
    beat=$((beat + 1))
    printf '%s\n' "$beat" > "$AGCTL_FAKE_SECURITY_PIDFILE.heartbeat"
}

# delay_ms sleeps $1 milliseconds in the foreground. With a pidfile it sleeps
# in 10 ms steps and then the remainder, writing the heartbeat before each
# sleep, from this shell.
delay_ms() {
    if [ -n "${AGCTL_FAKE_SECURITY_PIDFILE:-}" ]; then
        steps=$(($1 / 10))
        rest=$(($1 % 10))
        while [ "$steps" -gt 0 ]; do
            heartbeat
            sleep 0.01
            steps=$((steps - 1))
        done
        if [ "$rest" -gt 0 ]; then
            heartbeat
            sleep "0.00$rest"
        fi
    else
        sleep "$(printf '%d.%03d' $(($1 / 1000)) $(($1 % 1000)))"
    fi
}

# store_item is the one mutating path. It reads account, service, hex and
# hexlen, and never returns.
store_item() {
    if [ -n "${AGCTL_FAKE_SECURITY_WRITE_EXIT:-}" ]; then
        if [ -n "${AGCTL_FAKE_SECURITY_STDERR:-}" ]; then
            printf '%s\n' "$AGCTL_FAKE_SECURITY_STDERR" >&2
        fi
        exit "$AGCTL_FAKE_SECURITY_WRITE_EXIT"
    fi
    delay_before=''
    delay_after=''
    hold=''
    if [ -n "${AGCTL_FAKE_WRITE_DELAY_MS:-}" ]; then
        delay_before=$(decimal_count AGCTL_FAKE_WRITE_DELAY_MS "$AGCTL_FAKE_WRITE_DELAY_MS") || exit 1
    fi
    if [ -n "${AGCTL_FAKE_WRITE_DELAY_AFTER_MS:-}" ]; then
        delay_after=$(decimal_count AGCTL_FAKE_WRITE_DELAY_AFTER_MS "$AGCTL_FAKE_WRITE_DELAY_AFTER_MS") || exit 1
    fi
    if [ -n "${AGCTL_FAKE_SECURITY_HOLD_PIPES:-}" ]; then
        hold=$(decimal_count AGCTL_FAKE_SECURITY_HOLD_PIPES "$AGCTL_FAKE_SECURITY_HOLD_PIPES") || exit 1
    fi

    items=${AGCTL_FAKE_SECURITY_ITEMS:-/nonexistent}
    if ! grep -q -F -x -- "$service" "$items/.allowed-services" 2>/dev/null; then
        printf 'security: SecKeychainItemModifyAttributesAndData: %s\n' \
            'that service is not registered with this stand-in as a write target' >&2
        exit 1
    fi

    case "$hex" in
    '' | *[!0-9a-f]*)
        printf 'security: the -X value is not lowercase hexadecimal\n' >&2
        exit 1
        ;;
    esac
    if [ $((hexlen % 2)) -ne 0 ]; then
        printf 'security: the -X value has an odd number of hexadecimal digits\n' >&2
        exit 1
    fi

    # The pid is written whole and then renamed into place, so a reader never
    # sees a partial number.
    if [ -n "${AGCTL_FAKE_SECURITY_PIDFILE:-}" ]; then
        printf '%s\n' "$$" > "$AGCTL_FAKE_SECURITY_PIDFILE.tmp"
        mv -f "$AGCTL_FAKE_SECURITY_PIDFILE.tmp" "$AGCTL_FAKE_SECURITY_PIDFILE"
    fi
    if [ -n "$delay_before" ]; then
        delay_ms "$delay_before"
    fi

    # `-U` updates the item matching **both** names and creates one when there
    # is none, so a write whose `-a` is not the account the reader matches on
    # lands in a sibling item that no read will ever serve. Keying the file on
    # the account as well as the service is what lets a test see that.
    key=$(printf '%s' "$service" | tr -c 'A-Za-z0-9._-' '_')
    who=$(printf '%s' "$account" | tr -c 'A-Za-z0-9._-' '_')
    mkdir -p "$items/$who"
    if command -v xxd >/dev/null 2>&1; then
        printf '%s' "$hex" | xxd -r -p > "$items/$who/$key"
    elif command -v perl >/dev/null 2>&1; then
        printf '%s' "$hex" |
            perl -e 'my $h = do { local $/; <STDIN> }; print pack("H*", $h);' > "$items/$who/$key"
    else
        printf 'security: this stand-in needs xxd or perl to decode the -X value\n' >&2
        exit 1
    fi

    if [ -n "$delay_after" ]; then
        delay_ms "$delay_after"
    fi
    if [ -n "$hold" ]; then
        sleep "$hold" &
    fi
    exit 0
}

subcommand="$1"
[ $# -gt 0 ] && shift

case "$subcommand" in
show-keychain-info)
    if [ -n "${AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR:-}" ]; then
        printf '%s\n' "$AGCTL_FAKE_SECURITY_PREFLIGHT_STDERR" >&2
    fi
    exit "${AGCTL_FAKE_SECURITY_PREFLIGHT_EXIT:-0}"
    ;;
dump-keychain)
    # Fail this dump only once a marker exists, so a test can let the first
    # listing through and fail the second one (the fake `codex` creates the
    # marker in between).
    if [ -n "${AGCTL_FAKE_SECURITY_DUMP_EXIT_IF:-}" ] && [ -e "$AGCTL_FAKE_SECURITY_DUMP_EXIT_IF" ]; then
        exit "${AGCTL_FAKE_SECURITY_DUMP_EXIT_IF_CODE:-36}"
    fi
    if [ -n "${AGCTL_FAKE_SECURITY_DUMP:-}" ] && [ -f "$AGCTL_FAKE_SECURITY_DUMP" ]; then
        cat "$AGCTL_FAKE_SECURITY_DUMP"
    fi
    exit "${AGCTL_FAKE_SECURITY_DUMP_EXIT:-0}"
    ;;
find-generic-password)
    service=''
    account=''
    while [ $# -gt 0 ]; do
        case "$1" in
        -s)
            service="$2"
            shift 2
            ;;
        -a)
            account="$2"
            shift 2
            ;;
        *)
            shift
            ;;
        esac
    done
    if [ -n "${AGCTL_FAKE_SECURITY_FIND_EXIT_IF:-}" ] && [ -e "$AGCTL_FAKE_SECURITY_FIND_EXIT_IF" ]; then
        exit "${AGCTL_FAKE_SECURITY_FIND_EXIT_IF_CODE:-36}"
    fi
    if [ -n "${AGCTL_FAKE_SECURITY_FIND_EXIT:-}" ]; then
        if [ -n "${AGCTL_FAKE_SECURITY_STDERR:-}" ]; then
            printf '%s\n' "$AGCTL_FAKE_SECURITY_STDERR" >&2
        fi
        exit "$AGCTL_FAKE_SECURITY_FIND_EXIT"
    fi
    # A generic password is identified by its account **and** its service, the
    # way the shipped `security(1)` documents `-a` ("Match \"account\" string").
    # So the item lives under a per-account directory and an `-a` naming a
    # different account finds nothing, however familiar the service.
    key=$(printf '%s' "$service" | tr -c 'A-Za-z0-9._-' '_')
    who=$(printf '%s' "$account" | tr -c 'A-Za-z0-9._-' '_')
    file="${AGCTL_FAKE_SECURITY_ITEMS:-/nonexistent}/$who/$key"
    if [ -f "$file" ]; then
        cat "$file"
        exit 0
    fi
    printf 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n' >&2
    exit 44
    ;;
add-generic-password)
    # The write transport: exactly `-U -a <account> -s <service> -X <hex>`,
    # with no keychain argument and nothing read from stdin. A shape it does
    # not recognise runs nothing.
    if [ -z "$argv_write" ]; then
        printf 'security: add-generic-password expects exactly -U -a <account> -s <service> -X <hex>\n' >&2
        exit 1
    fi
    store_item
    ;;
delete-generic-password)
    # Refused rather than unimplemented, and refused loudly: agctl issues
    # no delete anywhere, so this arriving means a code path exists
    # that should not. The invocation is already in the log by the time this
    # runs, which is what the write-log test counts.
    printf 'security: this stand-in refuses to delete an item; agctl has no delete path\n' >&2
    exit 1
    ;;
*)
    printf 'security: this stand-in implements the read subcommands and the write only, not %s\n' \
        "$subcommand" >&2
    exit 1
    ;;
esac
