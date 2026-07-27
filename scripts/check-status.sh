#!/usr/bin/env bash
# Guards STATUS.md against turning back into a changelog.
#
# STATUS.md answers "what is actually wired up?" — one line per capability. It is
# deliberately not a history: `git log -p --follow STATUS.md` is the changelog, and it
# only stays readable while the entries stay short.
#
# If an entry needs a paragraph, the paragraph belongs in an implementation artifact
# under internal/_bmad-output/implementation-artifacts/ and the entry should link to it.
#
# See root AGENTS.md § STATUS.md discipline.

set -euo pipefail

STATUS_FILE="${1:-STATUS.md}"
MAX_CHARS="${MAX_STATUS_ENTRY_CHARS:-320}"

if [[ ! -f "$STATUS_FILE" ]]; then
  echo "check-status: $STATUS_FILE not found" >&2
  exit 1
fi

fail=0

report() {
  printf '%s:%s: %s\n' "$STATUS_FILE" "$1" "$2" >&2
  fail=1
}

entry_start=0
entry_text=""

flush() {
  [[ $entry_start -eq 0 ]] && return 0
  local n=${#entry_text}
  if (( n > MAX_CHARS )); then
    report "$entry_start" "entry is $n chars (max $MAX_CHARS)"
    printf '    %.90s...\n' "$entry_text" >&2
  fi
  entry_start=0
  entry_text=""
}

lineno=0
while IFS= read -r line || [[ -n "$line" ]]; do
  lineno=$((lineno + 1))

  # Nested bullets mean an entry grew sub-structure — the first step toward a changelog.
  if [[ "$line" =~ ^[[:space:]]{2,}[-*][[:space:]] ]]; then
    report "$lineno" "nested bullet — flatten it or move the detail to an implementation artifact"
    continue
  fi

  # A new top-level entry, a heading, or a blank line ends the previous entry.
  if [[ "$line" =~ ^-[[:space:]] ]]; then
    flush
    entry_start=$lineno
    entry_text="${line#- }"
  elif [[ -z "$line" || "$line" =~ ^# || "$line" =~ ^--- ]]; then
    flush
  elif [[ $entry_start -ne 0 && "$line" =~ ^[[:space:]]+ ]]; then
    # Wrapped continuation of the current entry.
    entry_text+=" ${line#"${line%%[![:space:]]*}"}"
  fi
done < "$STATUS_FILE"
flush

if [[ $fail -ne 0 ]]; then
  cat >&2 <<'EOF'

STATUS.md is state, not history. Keep each entry to one line naming the capability,
its issue, and its validation state. Depth goes in an implementation artifact under
internal/_bmad-output/implementation-artifacts/, linked from the entry.

To find when or why something changed:  git log -p --follow STATUS.md

See root AGENTS.md § STATUS.md discipline.
EOF
  exit 1
fi

echo "check-status: $STATUS_FILE ok"
