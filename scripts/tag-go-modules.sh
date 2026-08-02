#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 && "$#" -ne 3 ]]; then
	echo "usage: $0 vMAJOR.MINOR.PATCH commit [--dry-run]" >&2
	exit 2
fi

version=$1
commit=$2
root_module='github.com/DarkInno/crdt'
dry_run=false
if [[ "$#" -eq 3 ]]; then
	if [[ "$3" != '--dry-run' ]]; then
		echo "unknown option: $3" >&2
		exit 2
	fi
	dry_run=true
fi

if [[ ! "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "expected stable vMAJOR.MINOR.PATCH, got: ${version}" >&2
	exit 2
fi

git rev-parse --verify --quiet "${commit}^{commit}" >/dev/null
module_version=${version}
declare -a new_tags=()
declare -a all_tags=()

while IFS= read -r mod_file; do
	module_path=$(awk '$1 == "module" { print $2; exit }' "${mod_file}")
	if [[ -z "${module_path}" ]]; then
		echo "missing module path in ${mod_file}" >&2
		exit 1
	fi

	case "${module_path}" in
	"${root_module}")
		tag=${version}
		;;
	"${root_module}"/*)
		relative_module=${module_path#"${root_module}/"}
		tag="${relative_module}/${version}"
		;;
	*)
		echo "unsupported module path ${module_path} in ${mod_file}" >&2
		exit 1
		;;
	esac

	if [[ "${module_path}" != "${root_module}" ]]; then
		while read -r dependency required_version _; do
			if [[ "${dependency}" == "${root_module}" || "${dependency}" == "${root_module}/"* ]]; then
				if [[ "${required_version}" != "${module_version}" ]]; then
					echo "${mod_file} requires ${dependency} ${required_version}; expected ${module_version} for ${version}" >&2
					exit 1
				fi
			fi
		done < <(awk '$1 ~ /^github\.com\/DarkInno\/crdt(\/|$)/ { print $1, $2 }' "${mod_file}")
	fi

	if git rev-parse --verify --quiet "refs/tags/${tag}" >/dev/null; then
		tagged_commit=$(git rev-list -n 1 "${tag}")
		if [[ "${tagged_commit}" != "${commit}" ]]; then
			echo "refusing to replace ${tag}, which points to ${tagged_commit}" >&2
			exit 1
		fi
	else
		if [[ "${dry_run}" == false ]]; then
			git tag --annotate "${tag}" "${commit}" --message "Release ${tag}"
		fi
		new_tags+=("${tag}")
	fi
	all_tags+=("${tag}")
done < <(find . -path './.git' -prune -o -path './vendor' -prune -o -name go.mod -type f -print | LC_ALL=C sort)

if [[ "${#all_tags[@]}" -eq 0 ]]; then
	echo "no go.mod files found" >&2
	exit 1
fi

if [[ "${dry_run}" == false && "${#new_tags[@]}" -gt 0 ]]; then
	git push origin "${new_tags[@]}"
fi

printf '%s\n' "${all_tags[@]}"
