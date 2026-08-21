#!/usr/bin/env bash
# Report the tests that valve/upstream-bug-log.md names but the tree does not
# have.
#
# The log's whole value is that a claim points at a test you can run. A rebase
# that renames a test breaks that link in silence: the prose still reads as
# evidence, and the evidence is gone.
#
# This is a REPORT, not a commit hook, and the first run is why. It found six
# names, and all six were correct prose — each sat in a sentence that said the
# test was deleted or replaced, next to the name that replaced it. A hook that
# fails six times on the day it lands gets --no-verify, and the bypass takes
# the conflict-marker check with it. So this prints the line each name sits on
# and lets a person read it. Run it after a rebase, where a rename actually
# happens.
#
# It exits 1 when a name is missing, so a later caller can act on that. Wire it
# into the hooks only if a citation is ever found genuinely rotted.
set -euo pipefail

LOG="${1:-valve/upstream-bug-log.md}"

# A cited name is a bare identifier. Excluding a leading '.' drops Go selectors
# such as the `zerolog.TestWriter` in a quoted stack trace, which is a type in
# a dependency and never a test in this tree. A leading ':' must survive: the
# log cites as `path/to/file_test.go:TestName`.
#
# Four trailing characters, not six: both bounds extract the same 173 names
# today, so six is a commitment the data does not pay for.
names=$(grep -ohE '(^|[^A-Za-z0-9_.])Test[A-Za-z0-9_]{4,}' "$LOG" | sed -E 's/^[^T]*//' | sort -u)

# Every test in this tree is a plain `func TestX`; there are no testify suite
# methods. Match only what exists — a suite method would be reported as missing
# and read as a bug in this check, which is the loud failure, not the silent one.
#
# Match by PREFIX, with no trailing word boundary. The log cites whole families
# (`TestWebSocket_*`, `TestQueryLogs_*`) and -run patterns
# (`go test -run TestSuggestFinalizedBlock`), and an exact match calls all six
# of those missing. A prefix still catches the case worth catching: a renamed
# test leaves its old name matching nothing.
missing=0
while read -r name; do
	[ -n "$name" ] || continue
	if grep -rqE "^func ${name}" --include='*_test.go' .; then
		continue
	fi
	if [ "$missing" -eq 0 ]; then
		echo "check-test-citations: named in the log, absent from the tree:" >&2
		echo "  Read each line. A sentence that says the test is gone is correct;" >&2
		echo "  a sentence that cites it as a live pin is rot, and needs the new name." >&2
	fi
	missing=$((missing + 1))
	echo >&2
	grep -nF "$name" "$LOG" | sed 's/^/    /' >&2
done <<< "$names"

total=$(echo "$names" | grep -c . || true)
if [ "$missing" -eq 0 ]; then
	echo "check-test-citations: $total tests named, all present in the tree"
	exit 0
fi
echo >&2
echo "check-test-citations: $total tests named, $missing absent" >&2
exit 1
