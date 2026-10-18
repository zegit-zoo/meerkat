#!/usr/bin/env bash
# release-tag.sh — create and push a signed vX.Y.Z release tag, with the
# checks release.yml and the tag ruleset would otherwise fail on later.
#
#   bash scripts/release-tag.sh v1.2.3                 # tag origin/master's head
#   bash scripts/release-tag.sh v1.2.3 <commit>        # tag a commit on master
#   bash scripts/release-tag.sh --yes --force-ci v1.2.3 <commit>
#
# Only Markus and Jonas can create v* tags, and the other one approves the
# release environment (docs/RELEASE.md). The push goes over HTTPS: an SSH
# key can map to a GitHub account the tag ruleset refuses.
set -euo pipefail

REPO="zegit-zoo/meerkat"
REPO_URL="https://github.com/${REPO}.git"

usage() {
  cat <<EOF
usage: $(basename "$0") [--yes] [--force-ci] vX.Y.Z [commit]

  commit      defaults to origin/master's head; must be on master
  --yes       do not ask for confirmation
  --force-ci  tag even if the commit's checks failed or are pending
EOF
}

die() { echo "release-tag: $*" >&2; exit 1; }
warn() { echo "release-tag: warning: $*" >&2; }

yes=false
force_ci=false
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --yes) yes=true ;;
    --force-ci) force_ci=true ;;
    -*) usage >&2; die "unknown option: $1" ;;
    *) args+=("$1") ;;
  esac
  shift
done
[[ ${#args[@]} -ge 1 && ${#args[@]} -le 2 ]] || { usage >&2; exit 2; }

tag="${args[0]}"
# The same pattern as the ruleset and internal/update/cosign.go's
# CertIdentityRegexp: anything else would publish an unverifiable release.
[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
  || die "$tag is not vMAJOR.MINOR.PATCH"

cd "$(git rev-parse --show-toplevel)"

git fetch --quiet --tags "$REPO_URL" +refs/heads/master:refs/remotes/origin/master

commit="$(git rev-parse --verify --quiet "${args[1]:-refs/remotes/origin/master}^{commit}")" \
  || die "${args[1]:-origin/master} is not a commit"
# release.yml's verify job refuses the same thing, after the tag is pushed.
if ! git merge-base --is-ancestor "$commit" refs/remotes/origin/master; then
  die "${tag} would point at ${commit}, which is not on master. Releases are cut from master only."
fi

if git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null; then
  die "$tag already exists locally"
fi
if [[ -n "$(git ls-remote --tags --refs "$REPO_URL" "refs/tags/$tag")" ]]; then
  die "$tag already exists on $REPO"
fi

highest="$(git tag -l 'v*.*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)"
if [[ -n "$highest" && "$(printf '%s\n%s\n' "$highest" "$tag" | sort -V | tail -n 1)" != "$tag" ]]; then
  warn "$tag is not higher than $highest (fine for an older-line patch)"
fi

if command -v gh >/dev/null 2>&1; then
  echo "Checks on ${commit:0:12}:"
  runs="$(gh api --paginate "repos/${REPO}/commits/${commit}/check-runs" \
    --jq '.check_runs[] | [.name, .status, (.conclusion // "")] | @tsv')"
  statuses="$(gh api "repos/${REPO}/commits/${commit}/status" \
    --jq '.statuses[] | [.context, (if .state == "pending" then "pending" else "completed" end), .state] | @tsv')"
  bad=0
  while IFS=$'\t' read -r name status conclusion; do
    [[ -n "$name" ]] || continue
    case "$status/$conclusion" in
      completed/success|completed/neutral|completed/skipped) mark="ok" ;;
      completed/*) mark="FAILED ($conclusion)"; bad=1 ;;
      *) mark="PENDING ($status)"; bad=1 ;;
    esac
    printf '  %-50s %s\n' "$name" "$mark"
  done <<<"$runs"$'\n'"$statuses"
  if [[ -z "${runs}${statuses}" ]]; then
    echo "  (none)"
  fi
  if [[ $bad -ne 0 ]]; then
    "$force_ci" || die "checks on ${commit:0:12} failed or are pending; wait, or pass --force-ci"
    warn "tagging despite failed or pending checks (--force-ci)"
  fi
else
  warn "gh not found; not checking CI status of ${commit:0:12}"
fi

prev="$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' "$commit" 2>/dev/null || true)"
echo
echo "Tag:     $tag"
echo "Commit:  $commit"
echo "         $(git log -1 --format=%s "$commit")"
if [[ -n "$prev" ]]; then
  echo "Since $prev: $(git rev-list --count "${prev}..${commit}") commit(s)"
  git log --format='  %h %s' "${prev}..${commit}"
else
  echo "No earlier release tag reaches this commit."
fi
echo

if ! "$yes"; then
  read -r -p "Create and push signed tag $tag? [y/N] " answer
  [[ "$answer" == [yY] || "$answer" == [yY][eE][sS] ]] || die "aborted"
fi

git tag -s "$tag" "$commit" -m "meerkat $tag"
git tag -v "$tag"

# umask 022: the pre-push go-test hook fails under umask 077 (meerkat#203).
if ! (umask 022; git push "$REPO_URL" "refs/tags/$tag"); then
  git tag -d "$tag" >/dev/null
  die "push failed; deleted the local tag $tag so a retry starts clean"
fi

cat <<EOF

Pushed $tag. Release runs:
  https://github.com/${REPO}/actions/workflows/release.yml
The other release manager (not whoever pushed the tag) must approve the
\`release\` environment before goreleaser and docker run.
EOF
