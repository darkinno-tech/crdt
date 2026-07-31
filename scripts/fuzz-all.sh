#!/usr/bin/env sh

# Run every repository fuzz target independently. Go permits only one fuzz
# target per invocation, so derive package/target pairs from the current module
# graph instead of maintaining a duplicate Makefile list. TestGoFiles and
# XTestGoFiles come from go list, so platform/build-tag excluded files do not
# accidentally become fuzz targets.
set -eu

fuzz_time=${FUZZ_TIME:-20s}
fuzz_parallel=${FUZZ_PARALLEL:-1}
found=0

while IFS='|' read -r package directory test_files xtest_files; do
	targets=$(printf '%s,%s\n' "$test_files" "$xtest_files" |
		tr ',' '\n' |
		while IFS= read -r test_file; do
			if [ -n "$test_file" ]; then
				# The CI image intentionally contains only POSIX userland tools; keep
				# fuzz discovery independent of developer-only search utilities.
				grep -h -o -E '^func Fuzz[[:alnum:]_]+\(' "$directory/$test_file" || true
			fi
		done |
		sed -e 's/^func //' -e 's/(//' |
		sort -u)
	for target in $targets; do
		found=1
		if [ "${FUZZ_LIST_ONLY:-}" = "1" ]; then
			printf '%s %s\n' "$package" "$target"
			continue
		fi
		printf '%s\n' "fuzz: $package $target"
		go test -run='^$' -fuzz="^${target}$" -fuzztime="$fuzz_time" -parallel="$fuzz_parallel" "$package"
	done
done <<EOF
$(go list -f '{{.ImportPath}}|{{.Dir}}|{{join .TestGoFiles ","}}|{{join .XTestGoFiles ","}}' ./...)
EOF

if [ "$found" -eq 0 ]; then
	echo 'fuzz: no Fuzz* targets found in module graph' >&2
	exit 1
fi
