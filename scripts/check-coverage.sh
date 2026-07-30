#!/usr/bin/env sh

set -eu

threshold=${COVERAGE_THRESHOLD:-90}
failed=0

for package in $(go list ./...); do
	# Example and controlled benchmark commands are compiled and exercised by go
	# test, but are not library packages and intentionally contain process/exit
	# wiring that cannot be meaningfully covered in-process. Keep the 90% gate
	# focused on importable CRDT and utility packages.
	case "$package" in
		*/examples/*|*/cmd/crdt-compare) continue ;;
	esac
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
