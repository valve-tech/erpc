#!/usr/bin/env bash
# Exercises valve/precommit-range.sh, including the fallback. The fallback is
# the whole reason the script exists rather than an inline expression: a
# fallback that nothing runs is a guess, not an answer.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/precommit-range.sh"
repo="$(mktemp -d "${TMPDIR:-/tmp}/precommit-range.XXXXXX")"
trap 'rm -rf "$repo"' EXIT

git -C "$repo" init -q
git -C "$repo" config user.email t@example.com
git -C "$repo" config user.name t
for n in 1 2 3; do
	echo "$n" > "$repo/f$n"
	git -C "$repo" add "f$n"
	git -C "$repo" commit -qm "c$n"
done

first="$(git -C "$repo" rev-parse HEAD~2)"
parent="$(git -C "$repo" rev-parse HEAD~1)"
fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [ "$got" = "base=$want" ]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s\n       want base=%s\n       got  %s\n' "$name" "$want" "$got"
		fails=$((fails + 1))
	fi
}

run() { (cd "$repo" && env PR_BASE="${1-}" PUSH_BEFORE="${2-}" bash "$script"); }

check "a pull request base wins"            "$first"  "$(run "$first" "$parent")"
check "a push uses event.before"            "$parent" "$(run "" "$parent")"
check "all-zeros before falls back"         "$parent" "$(run "" 0000000000000000000000000000000000000000)"
check "an empty before falls back"          "$parent" "$(run "" "")"
check "a discarded commit falls back"       "$parent" "$(run "" 1111111111111111111111111111111111111111)"
check "a discarded PR base falls back"      "$parent" "$(run 1111111111111111111111111111111111111111 "")"

# A single-commit repository has no earlier range; HEAD against itself is empty.
solo="$(mktemp -d "${TMPDIR:-/tmp}/precommit-solo.XXXXXX")"
git -C "$solo" init -q
git -C "$solo" config user.email t@example.com
git -C "$solo" config user.name t
echo x > "$solo/f"
git -C "$solo" add f
git -C "$solo" commit -qm only
solo_head="$(git -C "$solo" rev-parse HEAD)"
check "a root commit checks nothing" "$solo_head" "$(cd "$solo" && env PR_BASE="" PUSH_BEFORE="" bash "$script")"
rm -rf "$solo"

if [ "$fails" -ne 0 ]; then
	printf '\n%d check(s) failed\n' "$fails"
	exit 1
fi
printf '\nall checks passed\n'
