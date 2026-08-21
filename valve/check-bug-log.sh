#!/usr/bin/env bash
# Check the structure of valve/upstream-bug-log.md before a commit.
#
# Three merges in one session left conflict markers in this file, and twice a
# reviewer found them rather than the author. Generic marker detection lives in
# the shared check-merge-conflict hook. This script checks what is specific to
# this document: entry ids must be unique, and every entry must carry exactly
# one status from the key at the top of the file.
#
# A merge that keeps both sides of a hunk is the failure this catches. It leaves
# two status paragraphs under one heading, and the two can disagree — one side
# said "open" where the other said "FIXED". A reader who greps for "open" then
# gets an answer that depends on which line the grep hit first.
#
# Patterns use [*] rather than \*. A backslash crosses the shell, then awk's
# string parser, before it reaches the regex engine, and each layer eats one.
set -euo pipefail

LOG="${1:-valve/upstream-bug-log.md}"

# Every entry heading, whatever its id. Ids are numeric (1, 118), lettered
# (A, F1, H1), and sparse, because agents write into reserved ranges. This
# check must not assume the numbering scheme.
ENTRY='## '
# Unanchored, so they can be anchored at each use site. An embedded '^'
# inside a larger pattern can never match, which is a silent always-pass.
STATUS='[*][*]Status'
VOCAB='[*][*]Status(: FIXED|:[*][*] (open|not a bug|unverifiable)| key[.])'

fail=0
note() {
	echo "check-bug-log: $1" >&2
	fail=1
}

duplicates=$(grep -E "^$ENTRY" "$LOG" | sed -E 's/^## ([^.]*)\..*/\1/' | sort | uniq -d)
if [ -n "$duplicates" ]; then
	note "duplicate entry ids: $(echo "$duplicates" | tr '\n' ' ')"
fi

# Report the heading, so a failure names the entry to repair rather than a
# line number that moves with the next edit.
offenders=$(awk -v entry="$ENTRY" -v status="$STATUS" '
	$0 ~ ("^" entry) { if (h != "") printf "%d\t%s\n", n, h; h = $0; n = 0; next }
	$0 ~ ("^" status) && h != "" { n++ }
	END { if (h != "") printf "%d\t%s\n", n, h }
' "$LOG" | awk -F'\t' '$1 != 1')
if [ -n "$offenders" ]; then
	note "entries whose status-line count is not exactly 1:"
	echo "$offenders" | awk '{print "  " $0}' >&2
fi

# The key names four statuses. A fifth spelling reads fine to a person and
# breaks every grep that counts the open ones.
outside=$(grep -nE "^$STATUS" "$LOG" | grep -vE "^[0-9]+:$VOCAB" || true)
if [ -n "$outside" ]; then
	note "status lines outside the vocabulary in the key:"
	echo "$outside" | awk '{print "  " $0}' >&2
fi

# A section header that names an id range must hold only ids in that range.
# "# Polyglot live run — entries 90-94" grew to 36 entries numbered 76 to 128,
# so the header claimed a live chain had reproduced defects found by reading
# code. A header is prose and drifts in silence; the ids under it do not.
strays=$(awk '
	# Three-argument match() is a gawk extension and this must run on the
	# BSD awk that ships with macOS. Extract with RSTART/RLENGTH instead.
	/^# / {
		sec = ""
		if (match($0, /entries[^0-9]*[0-9]+[^0-9]+[0-9]+/)) {
			t = substr($0, RSTART, RLENGTH)
			gsub(/[^0-9]+/, " ", t)
			if (split(t, a, " ") >= 2) { sec = $0; lo = a[1] + 0; hi = a[2] + 0 }
		}
		next
	}
	sec != "" && /^## [0-9]+\./ {
		id = $2; sub(/\..*/, "", id)
		if (id + 0 < lo || id + 0 > hi)
			printf "  %s\n    is under: %s\n", $0, sec
	}
' "$LOG")
if [ -n "$strays" ]; then
	note "entries outside the id range their section header claims:"
	echo "$strays" >&2
fi

if [ "$fail" -eq 0 ]; then
	echo "check-bug-log: $(grep -cE "^$ENTRY" "$LOG") entries, unique ids, one vocabulary"
fi
exit "$fail"
