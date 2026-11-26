#!/usr/bin/env bash
# Scenario tests for scripts/version.sh.
#
# @purpose Proves every local-observable scenario (S2, S3, S4, S4b, S5, S7, S8, S9, plus the
#          notes-verbatim, missing-anchor and missing-source-of-truth cases) of the release
#          version contract without touching the working tree or git state.
# @rationale A throwaway fixture tree is the cheapest way to exercise rewrite + drift detection
#            deterministically, and unlike a mock it runs the real script end to end.
# @invariants Operates only inside a mktemp directory; never mutates the repository.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  ok  $*"; }

# Build a minimal fixture that mirrors every managed location's exact shape.
fixture() {
  local d="$1" ver="$2"
  mkdir -p "$d/scripts" "$d/internal/version" "$d/cmd/agy-gateway" "$d/docs" "$d/deploy"
  cp "$REPO/scripts/version.sh" "$d/scripts/version.sh"
  printf 'package version\n\nvar Version = "%s"\n' "$ver" > "$d/internal/version/version.go"
  printf '// @version         %s\n// @title x\n' "$ver" > "$d/cmd/agy-gateway/main.go"
  printf 'package docs\n\nvar SwaggerInfo = &swag.Spec{\n\tVersion:          "%s",\n\tHost:             "h",\n}\n' "$ver" > "$d/docs/docs.go"
  printf 'info:\n  title: t\n  version: %s\npaths:\n' "$ver" > "$d/docs/swagger.yaml"
  printf '{\n    "info": {\n        "title": "t",\n        "version": "%s"\n    },\n    "paths": {}\n}\n' "$ver" > "$d/docs/swagger.json"
  printf 'FROM x\nARG VERSION=%s\nRUN go build -X v=%s\n' "$ver" "$ver" > "$d/deploy/Dockerfile"
  printf 'services:\n  agy-gateway:\n    image: agy-gateway:%s\n' "$ver" > "$d/docker-compose.yml"
  cp "$d/docker-compose.yml" "$d/deploy/docker-compose.yml"
  printf '[![r](https://img.shields.io/badge/Release-v%s-purple.svg)](x)\n' "$ver" > "$d/README.md"
  printf '# Changelog\n\nintro\n\n---\n\n## [0.9.0] - 2020-01-01\n\nold\n' > "$d/CHANGELOG.md"
}

echo "S2: bump to the current version is a no-op"
fixture "$WORK/s2" 1.2.3
before="$(cd "$WORK/s2" && find . -type f -exec md5sum {} + | sort)"
out="$(cd "$WORK/s2" && ./scripts/version.sh bump 1.2.3)"
after="$(cd "$WORK/s2" && find . -type f -exec md5sum {} + | sort)"
[[ "$before" == "$after" ]] || fail "S2 bump mutated files"
[[ "$out" == "bumped 1.2.3 -> 1.2.3 in 9 files" ]] || fail "S2 unexpected output: $out"
pass "no-op bump leaves the tree byte-identical"

echo "S3: malformed versions are rejected before any write"
fixture "$WORK/s3" 1.2.3
for bad in "1.2" "v1.2.3" "" "1.2.3.4" "x.y.z"; do
  before="$(cd "$WORK/s3" && find . -type f -exec md5sum {} + | sort)"
  if (cd "$WORK/s3" && ./scripts/version.sh bump "$bad") >/dev/null 2>&1; then
    fail "S3 accepted invalid version '$bad'"
  fi
  after="$(cd "$WORK/s3" && find . -type f -exec md5sum {} + | sort)"
  [[ "$before" == "$after" ]] || fail "S3 mutated files for '$bad'"
done
pass "rejects $(echo 1.2 v1.2.3 empty 4-part non-semver) with no mutation"

echo "S4: drift on one managed line fails verify and blocks bump"
fixture "$WORK/s4" 1.2.3
sed -i 's/^var Version = "1.2.3"/var Version = "1.2.2"/' "$WORK/s4/internal/version/version.go"
if (cd "$WORK/s4" && ./scripts/version.sh verify) >/dev/null 2>&1; then
  fail "S4 verify passed on a stale tree"
fi
if (cd "$WORK/s4" && ./scripts/version.sh bump 2.0.0) >/dev/null 2>&1; then
  fail "S4 bump ran on a stale tree"
fi
grep -q '^ARG VERSION=1.2.3$' "$WORK/s4/deploy/Dockerfile" || fail "S4 bump partially mutated the tree"
pass "verify exits 1 and bump refuses to write"

echo "S4b: a duplicated managed line fails closed"
fixture "$WORK/s4b" 1.2.3
printf 'badge/Release-v1.2.3-dup\n' >> "$WORK/s4b/README.md"
if (cd "$WORK/s4b" && ./scripts/version.sh verify) >/dev/null 2>&1; then
  fail "S4b verify passed with 2 matching lines"
fi
pass "ambiguous match count is rejected"

echo "S5-bump: any version rewrite reaches all nine locations"
fixture "$WORK/s5" 1.2.3
(cd "$WORK/s5" && ./scripts/version.sh bump 4.5.6) >/dev/null
(cd "$WORK/s5" && ./scripts/version.sh verify) >/dev/null || fail "S5 verify failed after bump"
grep -q '^var Version = "4.5.6"$' "$WORK/s5/internal/version/version.go" || fail "S5 source of truth not rewritten"
grep -q '^ARG VERSION=4.5.6$' "$WORK/s5/deploy/Dockerfile" || fail "S5 Dockerfile not rewritten"
grep -q '^        "version": "4.5.6"$' "$WORK/s5/docs/swagger.json" || fail "S5 swagger.json not rewritten"
grep -q 'image: agy-gateway:4.5.6' "$WORK/s5/deploy/docker-compose.yml" || fail "S5 nested compose not rewritten"
pass "all nine locations rewritten and verified"

echo "S8: changelog prepend is idempotent and ordered"
fixture "$WORK/s8" 1.2.3
(cd "$WORK/s8" && printf -- '- feat: first\n' | ./scripts/version.sh changelog 4.5.6) >/dev/null
grep -q '^## \[4\.5\.6\] - ' "$WORK/s8/CHANGELOG.md" || fail "S8 heading missing"
headings="$(grep -E '^## \[' "$WORK/s8/CHANGELOG.md")"
# Exactly two headings, newest first, nothing interleaved.
[[ "$(printf '%s\n' "$headings" | wc -l)" == "2" ]] || fail "S8 unexpected heading count: $headings"
first="$(printf '%s\n' "$headings" | sed -n '1p')"
second="$(printf '%s\n' "$headings" | sed -n '2p')"
[[ "$first" == "## [4.5.6] - "* ]] || fail "S8 first heading not newest: $first"
[[ "$second" == "## [0.9.0] - "* ]] || fail "S8 second heading not oldest: $second"
before="$(md5sum "$WORK/s8/CHANGELOG.md")"
out="$(cd "$WORK/s8" && printf -- '- feat: again\n' | ./scripts/version.sh changelog 4.5.6)"
after="$(md5sum "$WORK/s8/CHANGELOG.md")"
[[ "$before" == "$after" ]] || fail "S8 second prepend mutated the file"
[[ "$out" == *"already present, skipping"* ]] || fail "S8 no skip notice: $out"
[[ "$(grep -cE '^## \[4\.5\.6\]' "$WORK/s8/CHANGELOG.md")" == "1" ]] || fail "S8 duplicated heading"
pass "prepend once, skip on repeat, newest first"

echo "S7: empty release notes still insert a heading"
fixture "$WORK/s7" 1.2.3
(cd "$WORK/s7" && printf '' | ./scripts/version.sh changelog 4.5.6) >/dev/null
grep -q '^## \[4\.5\.6\] - ' "$WORK/s7/CHANGELOG.md" || fail "S7 heading missing for empty notes"
pass "empty notes produce a heading with no body"

echo "L1: backslash-containing notes survive verbatim"
fixture "$WORK/l1" 1.2.3
notes='- fix(regex): support \d+ patterns
- fix(paths): handle C:\temp on Windows
- fix(json): strip trailing \n'
(cd "$WORK/l1" && printf '%s' "$notes" | ./scripts/version.sh changelog 4.5.6) >/dev/null
grep -qF -- '- fix(regex): support \d+ patterns' "$WORK/l1/CHANGELOG.md" || fail "L1 \\d+ corrupted"
grep -qF -- '- fix(paths): handle C:\temp on Windows' "$WORK/l1/CHANGELOG.md" || fail "L1 C:\\temp corrupted"
grep -qF -- '- fix(json): strip trailing \n' "$WORK/l1/CHANGELOG.md" || fail "L1 literal \\n corrupted"
pass "notes inserted byte-for-byte, no escape interpretation"

echo "L2: a missing '---' anchor fails closed"
fixture "$WORK/l2" 1.2.3
printf '# Changelog\n\n## [0.1.0]\n\nold\n' > "$WORK/l2/CHANGELOG.md"
before="$(md5sum "$WORK/l2/CHANGELOG.md")"
mkdir -p "$WORK/tmpdir"
before_tmp="$(find "$WORK/tmpdir" -name 'agy-changelog.*' | wc -l)"
if (cd "$WORK/l2" && printf -- '- x\n' | TMPDIR="$WORK/tmpdir" ./scripts/version.sh changelog 4.5.6) >/dev/null 2>&1; then
  fail "L2 changelog succeeded without an anchor"
fi
after="$(md5sum "$WORK/l2/CHANGELOG.md")"
[[ "$before" == "$after" ]] || fail "L2 mutated CHANGELOG without an anchor"
# The transient lives in TMPDIR, not the fixture, so assert on the real location.
after_tmp="$(find "$WORK/tmpdir" -name 'agy-changelog.*' | wc -l)"
[[ "$before_tmp" == "0" && "$after_tmp" == "0" ]] || fail "L2 left $after_tmp temp file(s) in TMPDIR"
pass "no anchor -> exit 1, file untouched, no leftover temp in TMPDIR"

echo "L5: a missing source-of-truth file is reported by the script, not sed"
fixture "$WORK/l5" 1.2.3
rm -f "$WORK/l5/internal/version/version.go"
if (cd "$WORK/l5" && ./scripts/version.sh current) >/dev/null 2>&1; then
  fail "L5 current succeeded without the source of truth"
fi
pass "missing source of truth exits 1"

echo "S9: verify proves the tree matches the declared version"
fixture "$WORK/s9" 7.8.9
out="$(cd "$WORK/s9" && ./scripts/version.sh current)"
[[ "$out" == "7.8.9" ]] || fail "S9 current returned '$out'"
(cd "$WORK/s9" && ./scripts/version.sh verify) >/dev/null || fail "S9 verify failed"
pass "current/verify agree on the fixture version"

echo "S10: a notes block that already carries its own heading is not nested twice"
for h in '##' '###'; do
  fixture "$WORK/s10$h" 1.2.3
  printf '%s [4.5.6](https://example/compare) (2026-01-01)\n\n### Features\n\n* landed\n' "$h" \
    | (cd "$WORK/s10$h" && ./scripts/version.sh changelog 4.5.6) >/dev/null
  headings="$(grep -c '^## \[4\.5\.6\]' "$WORK/s10$h/CHANGELOG.md")"
  [[ "$headings" == "1" ]] || fail "S10 ($h notes) emitted $headings headings for 4.5.6"
  grep -q '^### Features$' "$WORK/s10$h/CHANGELOG.md" || fail "S10 ($h notes) dropped the notes body"
done
pass "self-headed notes keep exactly one heading"

echo
echo "all version.sh scenarios passed"
