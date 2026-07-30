#!/usr/bin/env sh

set -eu

threshold=${COVERAGE_THRESHOLD:-90}
failed=0
profile=$(mktemp)
filtered_profile=$(mktemp)
trap 'rm -f "$profile" "$filtered_profile"' EXIT HUP INT TERM

for package in $(go list ./...); do
	# Examples and generator/benchmark commands are compiled and exercised by
	# go test, but their process/exit wiring cannot be meaningfully covered
	# in-process. Keep the 90% gate focused on importable CRDT and utility
	# packages.
	case "$package" in
		*/examples/*|*/cmd/crdt-compare|*/internal/cmd/*) continue ;;
	esac
	output=$(go test -coverprofile="$profile" "$package")
	printf '%s\n' "$output"
	# Protobuf and gRPC stubs are generated directly from checked-in schemas;
	# exercising their every accessor is not evidence about our relay code. Keep
	# the 90% gate focused on handwritten CRDT and transport behaviour.
	grep -v '\.pb\.go:' "$profile" >"$filtered_profile" || true
	coverage=$(go tool cover -func="$filtered_profile" | awk '/^total:/ { sub(/%$/, "", $3); print $3 }')
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
