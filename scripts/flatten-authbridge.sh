#!/bin/sh
# One-shot migration: move authbridge/* to the repo root and drop the segment
# from every module path and path reference. Committed so review is "re-run it
# and diff" rather than reading 1,006 renames. Deleted in the final commit.
set -eu

cd "$(git rev-parse --show-toplevel)"

# 1. Move everything up a level. docs/ and scripts/ merge with their root
#    namesakes; neither has a filename collision. README.md and CLAUDE.md are
#    left in place for Tasks 3 and 4 — they DO collide.
for entry in authbridge/*; do
	name=${entry#authbridge/}
	case "${name}" in
		README.md|CLAUDE.md) continue ;;
		docs|scripts)
			# Merge, do not replace.
			for inner in "${entry}"/*; do
				git mv "${inner}" "${name}/$(basename "${inner}")"
			done
			rmdir "${entry}"
			;;
		*) git mv "${entry}" "${name}" ;;
	esac
done
# Dotfiles are not matched by authbridge/* above.
for entry in authbridge/.*; do
	case "${entry}" in authbridge/.|authbridge/..) continue ;; esac
	[ -e "${entry}" ] || continue
	git mv "${entry}" "${entry#authbridge/}"
done

# 2. Module paths.
for mod in $(find . -name go.mod -not -path './.git/*'); do
	dir=$(dirname "${mod}")
	old=$(awk '/^module /{print $2; exit}' "${mod}")
	new=$(printf '%s' "${old}" | sed 's|/cortex/authbridge/|/cortex/|')
	[ "${old}" = "${new}" ] || (cd "${dir}" && go mod edit -module "${new}")
done

# 3. Import paths. A one-segment prefix strip. Safe against the install URL,
#    which reads cortex/main/authbridge/ and cannot match cortex/authbridge/.
find . -name '*.go' -not -path './.git/*' -print0 \
	| xargs -0 sed -i '' 's|github.com/rossoctl/cortex/authbridge/|github.com/rossoctl/cortex/|g'

# 4. replace directives point INTO the subtree, so their relative paths are
#    unchanged — but the module path on the left side is not.
find . -name go.mod -not -path './.git/*' -print0 \
	| xargs -0 sed -i '' 's|github.com/rossoctl/cortex/authbridge/|github.com/rossoctl/cortex/|g'

# 5. Path references outside Go. Two exclusions, both deliberate:
#    - docs/superpowers/: dated records, per spec §12.
#    - install.sh: its --ref fallback deliberately PROBES the old location
#      (${ref}/authbridge/install.sh). Rewriting that makes both lookup paths
#      identical and silently breaks --ref for every pre-flatten tag. Its two
#      usage comments are hand-edited in step 5b instead.
git ls-files -z -- '*.yaml' '*.yml' '*.md' '*.sh' 'Makefile' '*.toml' '*.json' \
	| grep -zv '^docs/superpowers/' \
	| grep -zv '^install\.sh$' \
	| xargs -0 sed -i '' \
		-e 's|authbridge/authlib|authlib|g' \
		-e 's|authbridge/cmd/|cmd/|g' \
		-e 's|authbridge/storage/|storage/|g' \
		-e 's|authbridge/scripts/|scripts/|g' \
		-e 's|authbridge/proxy-init|proxy-init|g' \
		-e 's|authbridge/sparc-service|sparc-service|g' \
		-e 's|authbridge/lineage-attach|lineage-attach|g' \
		-e 's|authbridge/demos/|demos/|g' \
		-e 's|authbridge/docs/|docs/|g' \
		-e 's|authbridge/install|install|g' \
		-e 's|authbridge/keycloak_sync.py|keycloak_sync.py|g' \
		-e 's|authbridge/requirements.txt|requirements.txt|g' \
		-e 's|context: ./authbridge$|context: .|' \
		-e 's|directory: /authbridge/|directory: /|g'

# 6. Tidy every module.
for mod in $(find . -name go.mod -not -path './.git/*'); do
	(cd "$(dirname "${mod}")" && GOWORK=off go mod tidy)
done
