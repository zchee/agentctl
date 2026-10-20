#!/bin/sh
# A stand-in for security(1): the read subcommands, plus the write as
# `add-generic-password` with the payload in argv and as a `-i` command line
# on stdin. See src/secret/fake_security.rs.
#
# Write knobs, applied to both write forms:
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
#                                     every 10 ms. The beat comes from this
#                                     shell's own foreground loop, never from a
#                                     background job, so it stops the moment
#                                     this shell is killed.

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

# require_count refuses a knob value that is not a plain decimal count, so a
# typo fails the write loudly instead of silently skipping the delay.
require_count() {
    case "$2" in
    '' | *[!0-9]*)
        printf 'security: %s must be a decimal count, and is %s\n' "$1" "$2" >&2
        exit 1
        ;;
    esac
}

beat=0

# delay_ms sleeps $1 milliseconds in the foreground. With a pidfile it sleeps
# in 10 ms steps and writes the heartbeat before each one, from this shell.
delay_ms() {
    if [ -n "${AGCTL_FAKE_SECURITY_PIDFILE:-}" ]; then
        steps=$(($1 / 10))
        while [ "$steps" -gt 0 ]; do
            beat=$((beat + 1))
            printf '%s\n' "$beat" > "$AGCTL_FAKE_SECURITY_PIDFILE.heartbeat"
            sleep 0.01
            steps=$((steps - 1))
        done
    else
        sleep "$(printf '%d.%03d' $(($1 / 1000)) $(($1 % 1000)))"
    fi
}

# store_item is the one mutating path, shared by both write forms. It reads
# account, service, hex and hexlen, and never returns.
store_item() {
    if [ -n "${AGCTL_FAKE_SECURITY_WRITE_EXIT:-}" ]; then
        if [ -n "${AGCTL_FAKE_SECURITY_STDERR:-}" ]; then
            printf '%s\n' "$AGCTL_FAKE_SECURITY_STDERR" >&2
        fi
        exit "$AGCTL_FAKE_SECURITY_WRITE_EXIT"
    fi
    if [ -n "${AGCTL_FAKE_WRITE_DELAY_MS:-}" ]; then
        require_count AGCTL_FAKE_WRITE_DELAY_MS "$AGCTL_FAKE_WRITE_DELAY_MS"
    fi
    if [ -n "${AGCTL_FAKE_WRITE_DELAY_AFTER_MS:-}" ]; then
        require_count AGCTL_FAKE_WRITE_DELAY_AFTER_MS "$AGCTL_FAKE_WRITE_DELAY_AFTER_MS"
    fi
    if [ -n "${AGCTL_FAKE_SECURITY_HOLD_PIPES:-}" ]; then
        require_count AGCTL_FAKE_SECURITY_HOLD_PIPES "$AGCTL_FAKE_SECURITY_HOLD_PIPES"
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
    if [ -n "${AGCTL_FAKE_WRITE_DELAY_MS:-}" ]; then
        delay_ms "$AGCTL_FAKE_WRITE_DELAY_MS"
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

    if [ -n "${AGCTL_FAKE_WRITE_DELAY_AFTER_MS:-}" ]; then
        delay_ms "$AGCTL_FAKE_WRITE_DELAY_AFTER_MS"
    fi
    if [ -n "${AGCTL_FAKE_SECURITY_HOLD_PIPES:-}" ]; then
        sleep "$AGCTL_FAKE_SECURITY_HOLD_PIPES" &
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
-i)
    # The stdin form stays until the writer passes the payload in argv.
    # One command line on stdin, nothing secret in argv, parsed as strictly
    # as the argv form: a line it does not recognise runs nothing.
    input=$(cat)
    if [ -z "$input" ]; then
        printf 'security: -i expects one command line on standard input, and got none\n' >&2
        exit 1
    fi
    lines=$(printf '%s\n' "$input" | wc -l | tr -d ' ')
    if [ "$lines" != 1 ]; then
        printf 'security: -i expects exactly one command line, and got %s\n' "$lines" >&2
        exit 1
    fi
    case "$input" in
    'add-generic-password -U -a "'*'" -s "'*'" -X "'*'"') ;;
    *)
        printf 'security: -i does not recognise that command line\n' >&2
        exit 1
        ;;
    esac
    rest=${input#'add-generic-password -U -a "'}
    account=${rest%%'" -s "'*}
    rest=${rest#*'" -s "'}
    service=${rest%%'" -X "'*}
    hex=${rest#*'" -X "'}
    hex=${hex%'"'}
    hexlen=$(printf '%s' "$hex" | wc -c | tr -d ' ')

    # Logged redacted at the source, before anything else can go wrong with
    # the line: the log is what the write-log tests read, and a stand-in that
    # wrote the hex would put a credential in the test output.
    log_line "add-generic-password -U -a \"$account\" -s \"$service\" -X <REDACTED:$hexlen>"
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
