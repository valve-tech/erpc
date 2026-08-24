#!/usr/bin/env bash
# Pick the base commit for the CI pre-commit run.
#
# The local hooks judge what you are about to commit. This picks the CI
# equivalent of that range, so the job enforces the same thing. Running the
# hooks over the whole tree instead fails on 63 upstream-owned files that
# already violate end-of-file-fixer and trailing-whitespace, which makes the
# job permanently red and teaches people to ignore it.
#
# A pull request names its base. A push supplies `github.event.before`, which
# is all zeros when the branch is created and names a discarded commit after a
# force push. Both bad cases fall back to HEAD~1 — the commit the push added,
# for the ordinary fast-forward case this fork actually uses.
#
# Writes `base=<sha>` on stdout, for $GITHUB_OUTPUT.
#
# Covered by valve/precommit-range-test.sh.
set -euo pipefail

ZEROS=0000000000000000000000000000000000000000

base="${PR_BASE:-}"
if [ -z "$base" ]; then
	base="${PUSH_BEFORE:-}"
fi

# Reject anything git cannot resolve to a commit. That covers the empty case,
# the all-zeros case, and the force-push case where the old tip is gone —
# one path instead of three, so the fallback is the one thing to test.
if [ "$base" = "$ZEROS" ] || [ -z "$base" ] || ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
	if git rev-parse --verify --quiet HEAD~1 >/dev/null; then
		base="$(git rev-parse HEAD~1)"
	else
		# A repository with one commit has no earlier range. Checking HEAD
		# against itself is empty and passes, which is the honest answer.
		base="$(git rev-parse HEAD)"
	fi
fi

echo "base=$base"
