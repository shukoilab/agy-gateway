#!/usr/bin/env bash
# Single source of truth for the agy-gateway release version.
#
# @purpose Makes every in-repo version literal derive from internal/version/version.go,
#          so a release can never leave the tree stale.
# @rationale One table + four subcommands beats scattered sed calls inside the
#            workflow: the same code gates CI, drives the release commit, and proves local state.
# @invariants
#   - Version is read from `var Version = "X.Y.Z"` in internal/version/version.go; never hardcoded here.
#   - Every managed location must match its pattern exactly once; 0 or 2 matches is a hard error.
#   - bump is idempotent: bumping to the current version leaves a byte-empty diff.
#   - Touches no file outside TARGETS plus CHANGELOG.md; the only transient is a mktemp
#     file outside the repo, removed on every failure path.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SOUT="internal/version/version.go"
CHANGELOG="CHANGELOG.md"
VER_RE='[0-9]+\.[0-9]+\.[0-9]+'

# file|pattern|replacement — "(V)" is substituted with the version in both fields.
# Patterns are anchored on purpose so documentation examples cannot be rewritten by accident.
TARGETS=(
  "$SOUT|^var Version = \"(V)\"\$|var Version = \"(V)\""
  "cmd/agy-gateway/main.go|^// @version[[:space:]]+(V)\$|// @version         (V)"
  "docs/docs.go|^	Version:[[:space:]]+\"(V)\",\$|	Version:          \"(V)\","
  "docs/swagger.yaml|^  version: (V)\$|  version: (V)"
  "docs/swagger.json|^        \"version\": \"(V)\"\$|        \"version\": \"(V)\""
  "deploy/Dockerfile|^ARG VERSION=(V)\$|ARG VERSION=(V)"
  "docker-compose.yml|^    image: agy-gateway:(V)\$|    image: agy-gateway:(V)"
  "deploy/docker-compose.yml|^    image: agy-gateway:(V)\$|    image: agy-gateway:(V)"
  "README.md|badge/Release-v(V)-|badge/Release-v(V)-"
)

# @purpose Emits the current version from the single source of truth.
# @returns "X.Y.Z" on stdout; exactly one line.
# @throws Exit 1 if the declaration is missing or duplicated.
current() {
  local v count
  if [[ ! -f "$SOUT" ]]; then
    echo "error: $SOUT is missing" >&2
    exit 1
  fi
  v="$(sed -nE 's/^var Version = "(.*)"$/\1/p' "$SOUT")"
  count="$(printf '%s\n' "$v" | grep -cE "^${VER_RE}\$" || true)"
  if [[ "$count" != "1" ]]; then
    echo "error: expected exactly one version declaration in $SOUT, found $count" >&2
    exit 1
  fi
  printf '%s\n' "$v"
}

# @purpose Expands the (V) token in a target row.
# @param 1 Raw field template.
# @param 2 Literal version substituted for every (V).
# @param 3 "pattern" escapes dots for regex; "repl" leaves the version literal.
expand() {
  local tpl="$1" ver="$2" mode="$3" esc="$2"
  [[ "$mode" == "pattern" ]] && esc="${ver//./\\.}"
  printf '%s' "${tpl//(V)/$esc}"
}

# @purpose Asserts a target matches its version pattern exactly once.
# @param 1 Target row.
# @param 2 Literal version to build the pattern from.
# @throws Exit 1 naming file and match count.
require_single() {
  local row="$1" ver="$2" f pat n generic
  f="${row%%|*}"
  pat="$(expand "${row#*|}" "$ver" pattern)"
  pat="${pat%%|*}"
  if [[ ! -f "$f" ]]; then
    echo "error: $f is missing" >&2
    exit 1
  fi
  n="$(grep -cE -- "$pat" "$f" || true)"
  if [[ "$n" != "1" ]]; then
    echo "error: $f: expected exactly 1 match for '$pat', got $n" >&2
    exit 1
  fi
  # A second match carrying a different version would mean unmanaged drift on the same line shape.
  generic="$(expand "${row#*|}" "$VER_RE" pattern)"
  generic="${generic%%|*}"
  n="$(grep -cE -- "$generic" "$f" || true)"
  if [[ "$n" != "1" ]]; then
    echo "error: $f: expected exactly 1 version-bearing line, got $n" >&2
    exit 1
  fi
}

# @purpose Read-only proof that every managed location carries the current version.
# @throws Exit 1 listing the offending file.
verify() {
  local ver row
  ver="$(current)"
  for row in "${TARGETS[@]}"; do
    require_single "$row" "$ver"
  done
  echo "verified $ver in ${#TARGETS[@]} locations"
}

# @purpose Rewrites every managed location to a new version, then re-verifies.
# @param 1 Target version, strict X.Y.Z.
# @throws Exit 1 on malformed version or on any pattern that does not match exactly once.
bump() {
  local new="${1:-}" old row
  if ! [[ "$new" =~ ^${VER_RE}$ ]]; then
    echo "error: invalid version '$new' (expected X.Y.Z)" >&2
    exit 1
  fi
  old="$(current)"
  # Validate every target before mutating anything, so a partial bump is impossible.
  for row in "${TARGETS[@]}"; do
    require_single "$row" "$old"
  done
  for row in "${TARGETS[@]}"; do
    local f pat repl
    f="${row%%|*}"
    pat="$(expand "${row#*|}" "$old" pattern)"
    pat="${pat%%|*}"
    repl="$(expand "${row#*|}" "$new" repl)"
    repl="${repl##*|}"
    sed -i -E "s|${pat}|${repl}|" "$f"
  done
  verify >/dev/null
  echo "bumped $old -> $new in ${#TARGETS[@]} files"
}

# @purpose Prepends a release section to CHANGELOG.md from notes on stdin.
# @param 1 Released version.
# @throws Exit 1 on malformed version, missing CHANGELOG.md, or absent "---" anchor.
# @invariants Idempotent: an existing "## [X.Y.Z]" heading short-circuits with no write.
#             Notes are inserted byte-for-byte (no shell or awk escape interpretation).
#             No temporary file is left behind on any failure path.
changelog() {
  local ver="${1:-}" notes tmp heading mode
  if ! [[ "$ver" =~ ^${VER_RE}$ ]]; then
    echo "error: invalid version '$ver' (expected X.Y.Z)" >&2
    exit 1
  fi
  if [[ ! -f "$CHANGELOG" ]]; then
    echo "error: $CHANGELOG is missing" >&2
    exit 1
  fi
  notes="$(cat)"
  # Drop a single trailing newline so the notes block is not followed by a stray blank line.
  notes="${notes%$'\n'}"
  # github-tag-action already prefixes its notes with a "[x.y.z]" heading (## for minor/major,
  # ### for patch, usually linked). Strip that leading heading and the blank lines around it,
  # otherwise the section nests twice.
  notes="$(printf '%s\n' "$notes" | VER="$ver" awk '
    BEGIN { pat = "^#{1,3} \\[" ENVIRON["VER"] "\\]" }
    !started {
      if ($0 ~ pat) next
      if ($0 ~ /^[[:space:]]*$/) next
      started = 1
    }
    { print }
  ')"
  if grep -qE "^## \[${ver//./\\.}\]" "$CHANGELOG"; then
    echo "changelog $ver already present, skipping"
    return 0
  fi
  heading="## [$ver] - $(date +%F)"
  mode="$(stat -c '%a' "$CHANGELOG" 2>/dev/null || echo 644)"
  tmp="$(mktemp "${TMPDIR:-/tmp}/agy-changelog.XXXXXX")"
  # Notes reach awk through the environment (ENVIRON), not -v: awk -v re-escapes
  # backslash sequences, corrupting `\d+`, `C:\temp`, and `\n` in commit subjects.
  if ! NOTES="$notes" awk -v heading="$heading" '
    BEGIN { n = ENVIRON["NOTES"] }
    { print }
    !done && $0 == "---" {
      print ""
      print heading
      print ""
      if (n != "") print n
      done = 1
    }
    END { exit(done ? 0 : 3) }
  ' "$CHANGELOG" > "$tmp"; then
    rm -f "$tmp"
    echo "error: $CHANGELOG: no '---' separator found; cannot insert $ver" >&2
    exit 1
  fi
  # Any failure between temp creation and the rename must not leave the transient behind.
  if ! mv "$tmp" "$CHANGELOG"; then
    rm -f "$tmp"
    echo "error: failed to replace $CHANGELOG" >&2
    exit 1
  fi
  # mktemp creates 0600; restore the file's original mode so a local run does not narrow it.
  chmod "$mode" "$CHANGELOG"
  echo "changelog $ver prepended"
}

usage() {
  cat >&2 <<'EOF'
usage: scripts/version.sh <command> [version]

  current              print the single-source-of-truth version
  verify               fail if any managed location is stale
  bump X.Y.Z           rewrite every managed location, then verify
  changelog X.Y.Z      prepend a release section (notes on stdin)
EOF
  exit 1
}

case "${1:-}" in
  current)   current ;;
  verify)    verify ;;
  bump)      bump "${2:-}" ;;
  changelog) changelog "${2:-}" ;;
  *)         usage ;;
esac
