#!/bin/sh
set -eu

result_file=${GOOBERS_INPUT_RESULTFILE:-todo-check-result.json}
listing_file=${TMPDIR:-/tmp}/goobers-todos.$$
trap 'rm -f "$listing_file"' EXIT HUP INT TERM

if git grep -nI -e '[T]ODO' -- . >"$listing_file"; then
	:
else
	status=$?
	if [ "$status" -ne 1 ]; then
		exit "$status"
	fi
fi

todo_count=$(wc -l <"$listing_file" | tr -d '[:space:]')
cat "$listing_file"
printf '{"todoCount":%s}\n' "$todo_count" >"$result_file"
