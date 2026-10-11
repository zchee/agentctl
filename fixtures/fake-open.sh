#!/bin/sh
# A stand-in for open(1) that starts the desktop app: it creates the data
# directory the way the app does at its first start and records a new main
# with two helpers in the testing process table.
#   AGCTL_FAKE_OPEN  ready (default): the main is visible at once.
#                    late: the main becomes visible one listing after the
#                    readiness wait has ended.
#                    later: the main becomes visible two listings after the
#                    readiness wait has ended, so the late check misses it
#                    and the rollback's own listing is the first to see it.
#                    none: open succeeds and no process appears.
#                    nomain: open creates the data directory and no
#                    process appears.
#                    fail: open fails.
#                    fail-once: the first open fails, later ones are ready.
#                    blind: open starts the app as ready does, and from then
#                    on every listing of the process table fails with EIO,
#                    so nothing can be observed after the start.
#                    fail-once-blind: the first open fails, later ones are
#                    blind.
#                    fail-other: open fails while the main of another copy
#                    of the app appears in the table.
#                    fail-hide: open fails after moving the directory named
#                    by AGCTL_FAKE_OPEN_HIDE to the same path plus ".moved",
#                    the way someone else could while the switch runs.
#   AGCTL_FAKE_DESKTOP_LOG  file that receives one line per invocation.
if [ -n "${AGCTL_FAKE_DESKTOP_LOG:-}" ]; then
    printf 'open %s\n' "$*" >> "$AGCTL_FAKE_DESKTOP_LOG"
fi
mode="${AGCTL_FAKE_OPEN:-ready}"
if [ "$mode" = fail-once ] || [ "$mode" = fail-once-blind ]; then
    marker="$AGENTCTL_TEST_PROCESS_TABLE.open-failed"
    if [ -e "$marker" ]; then
        mode=${mode#fail-once}
        mode=${mode#-}
        mode=${mode:-ready}
    else
        : > "$marker"
        mode=fail
    fi
fi
case "$mode" in
fail-other)
    pid=$(proctable nextpid "$AGENTCTL_TEST_PROCESS_TABLE") || exit 1
    proctable add "$AGENTCTL_TEST_PROCESS_TABLE" "$pid" 1 /Users/Shared/Applications/Claude.app/Contents/MacOS/Claude || exit 1
    echo "The application cannot be opened for an unexpected reason (fake)." >&2
    exit 1
    ;;
fail-hide)
    mv "$AGCTL_FAKE_OPEN_HIDE" "$AGCTL_FAKE_OPEN_HIDE.moved" || exit 1
    echo "The application cannot be opened for an unexpected reason (fake)." >&2
    exit 1
    ;;
fail)
    echo "The application cannot be opened for an unexpected reason (fake)." >&2
    exit 1
    ;;
none)
    exit 0
    ;;
esac
mkdir -p "$HOME/Library/Application Support/Claude" || exit 1
[ "$mode" = nomain ] && exit 0
app=/Applications/Claude.app/Contents
pid=$(proctable nextpid "$AGENTCTL_TEST_PROCESS_TABLE") || exit 1
delay=0
if [ "$mode" = late ]; then
    # The launch wait takes one listing per poll; one more listing, the late
    # check, is the first to see this main.
    delay=61
elif [ "$mode" = later ]; then
    # One listing past the late check: the rollback's listing sees it.
    delay=62
fi
proctable add "$AGENTCTL_TEST_PROCESS_TABLE" "$pid" 1 "$app/MacOS/Claude" "delay=$delay" &&
    proctable add "$AGENTCTL_TEST_PROCESS_TABLE" $((pid + 1)) "$pid" "$app/Frameworks/Claude Helper (Renderer).app/Contents/MacOS/Claude Helper (Renderer)" "delay=$delay" &&
    proctable add "$AGENTCTL_TEST_PROCESS_TABLE" $((pid + 2)) "$pid" "$app/Frameworks/Claude Helper (GPU).app/Contents/MacOS/Claude Helper (GPU)" "delay=$delay" || exit 1
if [ "$mode" = blind ]; then
    # A process of the same user whose path read fails makes every later
    # listing fail, the way an unreadable process table would.
    proctable add "$AGENTCTL_TEST_PROCESS_TABLE" $((pid + 3)) 1 /usr/libexec/unlistable read_error=EIO || exit 1
fi
