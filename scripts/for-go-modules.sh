#!/usr/bin/env sh

set -eu

if [ "$#" -eq 0 ]; then
	printf '%s\n' "usage: $0 command [argument ...]" >&2
	exit 2
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_root=$(CDPATH= cd -- "${script_dir}/.." && pwd)

for module in \
	. \
	durable \
	examples \
	extensions \
	persistence \
	telemetry \
	providers/internal/sqlrelay \
	providers/mysql \
	providers/postgres \
	providers/redis \
	providers/sqlite \
	providers/webrtc
do
	printf '%s\n' "==> ${module}"
	(
		cd "${project_root}/${module}"
		"$@"
	)
done
