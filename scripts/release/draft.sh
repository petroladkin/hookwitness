#!/bin/bash
# draft.sh TAG FILE...: puts a release's files on the draft Release of TAG,
# for the binaries job of .github/workflows/ci.yml. No Release with the tag:
# a draft is created (a prerelease for vX.Y.Z-rc.N). One draft (an earlier
# run of the tag): the files are uploaded to it again. A published Release
# is never changed, and several Releases with the tag are refused: deleting
# the extra drafts is a person's call.
# Needs gh (GH_TOKEN) and GITHUB_REPOSITORY.
set -euo pipefail
export TAG=${1:?usage: draft.sh TAG FILE...}
shift
drafts=$(gh api "repos/$GITHUB_REPOSITORY/releases" --paginate --jq '.[] | select(.tag_name == env.TAG) | .draft')
n=$(grep -c . <<<"$drafts" || true)
d=$(grep -c '^true$' <<<"$drafts" || true)
if [ "$n" -gt 1 ]; then
  echo "::error::$n Releases carry tag $TAG (drafts: $d); delete the extra drafts by hand"
  exit 1
fi
case "$drafts" in
  false)
    echo "::error::the Release of $TAG is already published; its assets are left as they are"
    exit 1 ;;
  true)
    gh release upload "$TAG" "$@" --clobber -R "$GITHUB_REPOSITORY" ;;
  "")
    pre=
    case "$TAG" in *-rc.*) pre=--prerelease ;; esac
    gh release create "$TAG" --draft $pre --verify-tag --title "$TAG" \
      --notes "Draft made by CI: the release step writes the notes and publishes it." \
      -R "$GITHUB_REPOSITORY" "$@" ;;
  *)
    echo "::error::unexpected answer about the Releases of $TAG: $drafts"
    exit 1 ;;
esac
