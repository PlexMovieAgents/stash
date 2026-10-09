#!/usr/bin/env bash
# Rebases the current branch's patches from one upstream base onto another.
#
# usage: rebase-custom.sh <old base> <new base>
#
# Conflicts in go.mod and go.sum alone, which happen whenever upstream updates
# a dependency the patches also had to update, are resolved by taking
# upstream's files and adding back the modules the patch introduced. Go's
# minimal version selection then raises any shared dependencies to what those
# modules need.
#
# Any other conflict aborts the rebase, writes the conflicting files to
# $CONFLICTS_FILE (if set) and exits with status 2.

set -euo pipefail

old_base=$1
new_base=$2

# module paths required by a go.mod, one per line
requirements() {
	go mod edit -json "$1" | jq -r '.Require[]?.Path' | sort -u
}

resolve_gomod() {
	local ours theirs patch_mod
	ours=$(mktemp)
	theirs=$(mktemp)
	patch_mod=$(mktemp)

	git show REBASE_HEAD:go.mod > "$patch_mod"
	git checkout --ours -- go.mod go.sum

	requirements go.mod > "$ours"
	requirements "$patch_mod" > "$theirs"

	# modules the patch added
	comm -13 "$ours" "$theirs" | while read -r path; do
		version=$(go mod edit -json "$patch_mod" | jq -r --arg p "$path" '.Require[] | select(.Path == $p) | .Version')
		echo "Re-adding $path@$version"
		go get "$path@$version"
	done

	# go get marks the added modules as indirect, but the patch imports some
	# of them directly. Edit the lines in place, keeping the diff to upstream
	# small.
	go mod edit -json "$patch_mod" | jq -r '.Require[] | select(.Indirect | not) | .Path' | while read -r path; do
		awk -v p="$path" '$1 == p && $3 == "//" && $4 == "indirect" { sub(/ \/\/ indirect$/, "") } { print }' go.mod > go.mod.tmp
		mv go.mod.tmp go.mod
	done

	# record checksums for the raised versions of modules imported elsewhere.
	# go mod tidy is not used, since without generated code it would drop
	# modules that only generated code imports.
	go get ./...

	git add go.mod go.sum
	rm -f "$ours" "$theirs" "$patch_mod"
}

if git rebase --onto "$new_base" "$old_base"; then
	exit 0
fi

while true; do
	conflicts=$(git diff --name-only --diff-filter=U)
	if [ -z "$conflicts" ]; then
		# e.g. a patch that became empty
		echo "Rebase stopped without conflicts" >&2
		git status >&2
		git rebase --abort
		exit 1
	fi

	if [ "$(grep -cvxE 'go\.(mod|sum)' <<< "$conflicts")" != 0 ]; then
		echo "Conflicts applying $(git log -1 --format='%h %s' REBASE_HEAD):" >&2
		echo "$conflicts" >&2
		if [ -n "${CONFLICTS_FILE:-}" ]; then
			{
				echo "Applying \`$(git log -1 --format='%h %s' REBASE_HEAD)\`:"
				echo
				sed 's/^/- /' <<< "$conflicts"
			} > "$CONFLICTS_FILE"
		fi
		git rebase --abort
		exit 2
	fi

	echo "Resolving go.mod/go.sum conflict in $(git log -1 --format='%h %s' REBASE_HEAD)"
	resolve_gomod

	if GIT_EDITOR=true git rebase --continue; then
		exit 0
	fi
done
