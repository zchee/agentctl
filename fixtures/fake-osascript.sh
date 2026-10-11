#!/bin/sh
# A stand-in for osascript(1) that answers the desktop app's quit request.
# It edits the testing process table through the harness's proctable
# command; it never talks to a real application.
#   AGCTL_FAKE_OSASCRIPT  quit (default): the app's mains and their process
#                         trees exit, and so do processes run from its data
#                         directory.
#                         mains: only the mains exit; their children are
#                         reparented to pid 1 and linger.
#                         keep-data: the trees exit but processes run from
#                         the data directory linger, reparented to pid 1.
#                         keep: nothing exits.
#                         refuse: the request fails.
#   AGCTL_FAKE_DESKTOP_LOG  file that receives one line per invocation.
if [ -n "${AGCTL_FAKE_DESKTOP_LOG:-}" ]; then
    printf 'osascript %s\n' "$*" >> "$AGCTL_FAKE_DESKTOP_LOG"
fi
case "${AGCTL_FAKE_OSASCRIPT:-quit}" in
refuse)
    echo "execution error: Claude got an error: User canceled. (-128)" >&2
    exit 1
    ;;
keep)
    exit 0
    ;;
*)
    exec proctable quit "$AGENTCTL_TEST_PROCESS_TABLE" "$HOME/Library/Application Support/Claude" "${AGCTL_FAKE_OSASCRIPT:-quit}"
    ;;
esac
