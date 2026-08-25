#!/usr/bin/env bash
# Refuse an import of a fork-added package from a file that is not itself
# fork-added.
#
# The fork tracks erpc/erpc by REBASING. Every line the fork changes in a file
# upstream also owns is replayed on every rebase, forever, and can conflict —
# common/config.go is already +495/-215 and one of the worst recurring conflict
# sites. A file under a valve-named directory costs nothing, because a rebase
# never touches a file upstream does not have. That is the whole reason
# valvebilling/ reads its flag from the environment instead of adding a field
# to eRPC's config: see the header of valvebilling/config.go.
#
# The property that keeps it true is one import rule, and this is the check.
# An import edge from eRPC's own code into a fork package is what would spend
# the budget: it makes the fork packages non-deletable, it makes the flag-off
# path something other than stock eRPC, and it puts fork symbols in files that
# reconcile/ws-plus-main and archive/harvest-onto-main also edit.
#
# The rule is symmetric and needs no list of package names:
#
#   A Go file may import github.com/erpc/erpc/valve* only if the file's own
#   DIRECTORY path contains a path segment beginning with "valve".
#
# Deliberately no hardcoded set of fork directories. Every fork-added path
# today — valve/, valvebilling/, valverelay/, cmd/valve-relay/ — carries that
# segment, so the naming convention already in use is the whole specification.
# A new valvemetrics/ is covered on the day it lands, and a rename of
# valverelay/ needs no edit here. A list would need editing to stay correct,
# and a stale list under-protects in silence.
#
# The test is on the DIRECTORY, not the whole path, because the invariant is
# about directories being deletable. A file named common/valvehook.go is not
# fork-added in any useful sense — deleting valvebilling/ would still break it.
#
# Scope note: this is not a general "no upstream file differs from upstream"
# check, and must never become one. The fork has legitimately edited upstream
# files elsewhere and that pre-existing debt is out of scope here.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Read the module path rather than hardcoding it, so a module rename does not
# turn this check into one that silently matches nothing.
MODULE="$(awk '/^module /{print $2; exit}' go.mod)"

# A Go import spec, with or without an alias. Anchored on the closing quote so
# it cannot run past the end of the path.
IMPORT_RE="\"${MODULE}/valve[^\"]*\""

# True when a directory path has a segment that begins with "valve".
is_fork_dir() {
	case "/$1/" in
	*/valve*/) return 0 ;;
	esac
	return 1
}

# Self-test, because the failure mode of a grep-based gate is that its pattern
# rots, matches nothing, and passes in silence forever. valve/check-bug-log.sh
# carries the same warning for the same reason. These two probes are fixed
# inputs with known answers, so they cost nothing and cannot flake.
if ! printf '%s\n' "	alias \"${MODULE}/valvebilling\"" | grep -qE "$IMPORT_RE"; then
	echo "check-fork-isolation: BROKEN CHECK — the import pattern no longer" >&2
	echo "  matches a known fork import. Pattern: $IMPORT_RE" >&2
	echo "  Repair this script; it is passing everything." >&2
	exit 1
fi
if is_fork_dir "common" || ! is_fork_dir "cmd/valve-relay"; then
	echo "check-fork-isolation: BROKEN CHECK — is_fork_dir misclassifies a" >&2
	echo "  known path. Repair this script; it is passing everything." >&2
	exit 1
fi

# Judge the INDEX, which is what is about to be committed. git ls-files reads
# the index, so a newly staged file is included and a merely untracked one is
# not — an untracked file is not part of the commit, and failing on it would be
# a false alarm. pre-commit stashes unstaged changes before it runs a hook, so
# under the hook the worktree grep below sees exactly the staged content.
#
# Running this by hand on a dirty tree therefore reports on what you have
# staged, not on what you have merely written. Stage the file first.
#
# Scanning the index also means nothing walks .git/, node_modules/ or any
# untracked tree. -H forces the filename prefix: without it grep omits the name
# when xargs happens to hand it a single file, and the report loses the one
# thing it exists to tell you.
hits="$(git ls-files -z '*.go' | xargs -0 grep -HnE "$IMPORT_RE" || true)"

offenders=""
while IFS= read -r hit; do
	[ -n "$hit" ] || continue
	file="${hit%%:*}"
	# Parameter expansion, not dirname(1): this runs once per hit and a
	# subprocess each time is the one thing that could make it slow. A path
	# with no slash is a file at the repo root, whose directory is ".".
	dir="."
	case "$file" in
	*/*) dir="${file%/*}" ;;
	esac
	is_fork_dir "$dir" || offenders="${offenders}${hit}"$'\n'
done <<<"$hits"

if [ -z "$offenders" ]; then
	exit 0
fi

cat >&2 <<EOF
check-fork-isolation: a file outside the fork-added directories imports a
fork-added package.

$(printf '%s' "$offenders" | sed 's/^/  /')

Why this is refused:

  The fork tracks erpc/erpc by rebasing. Every line the fork changes in an
  upstream-owned file is replayed on every rebase, forever, and can conflict.
  common/config.go alone is already +495/-215. Files under valve-named
  directories cost nothing, because a rebase never touches a file upstream
  does not have.

  An import edge from eRPC's own code into a fork package spends that budget
  and takes three properties with it:

    - valvebilling/ and valverelay/ stop being deletable.
    - The flag-off path stops being stock eRPC.
    - reconcile/ws-plus-main and archive/harvest-onto-main stop merging
      cleanly, because fork symbols now sit in files those branches edit too.

What to do instead:

  Call the fork package from valverelay/ or cmd/valve-relay/, which is where
  the relay's own code lives. valvebilling/ imports nothing from eRPC on
  purpose — see valvebilling/doc.go.

  If eRPC's own request path genuinely must call it, that is a deliberate
  decision to start paying rebase debt, not an oversight. Record the decision
  and its cost in valve/billing-module.md, then change this check on purpose.
EOF
exit 1
