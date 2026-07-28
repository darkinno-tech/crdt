#!/usr/bin/env sh

set -eu

threshold=${COVERAGE_THRESHOLD:-90}
failed=0

# Executable examples are run by go test but do not define library coverage
# obligations. The gate applies to importable packages and command tools.
for package in $(go list ./... | awk '$0 !~ /\/examples\//'); do
	output=$(go test -cover "$package")
	printf '%s\n' "$output"
	coverage=$(printf '%s\n' "$output" | sed -n 's/.*coverage: \([0-9.][0-9.]*\)%.*/\1/p')
	if [ -z "$coverage" ]; then
		echo "coverage: no result for $package" >&2
		failed=1
		continue
	fi
	if ! awk -v actual="$coverage" -v required="$threshold" 'BEGIN { exit !(actual + 0 >= required + 0) }'; then
		echo "coverage: $package is ${coverage}%, below required ${threshold}%" >&2
		failed=1
	fi
done

exit "$failed"
