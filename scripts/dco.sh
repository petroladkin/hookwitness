#!/bin/bash
# Checks the Developer Certificate of Origin sign-off of every commit in
# <base>..<head>: each one (merges aside) must carry a
# "Signed-off-by: Name <email>" trailer naming its author exactly, which
# `git commit -s` adds. CI runs it on pull requests (see CONTRIBUTING.md).
#
# Usage: scripts/dco.sh <base> <head>
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: scripts/dco.sh <base> <head>" >&2
  exit 2
fi

bad=0
n=0
while IFS=$'\x1f' read -r -d $'\x1e' sha author subject trailers; do
  sha=${sha#$'\n'}
  n=$((n + 1))
  signed=no
  while IFS= read -r line; do
    if [ "$line" = "$author" ]; then
      signed=yes
    fi
  done <<<"$trailers"
  if [ $signed = no ]; then
    echo "missing: ${sha:0:12} $subject"
    echo "  wants: Signed-off-by: $author"
    bad=$((bad + 1))
  fi
done < <(git log --no-merges --format='%H%x1f%an <%ae>%x1f%s%x1f%(trailers:key=Signed-off-by,valueonly,unfold)%x1e' "$1..$2")

if [ $bad -gt 0 ]; then
  echo "$bad of $n commits lack the author's sign-off. Add it with 'git commit --amend -s'," >&2
  echo "or 'git rebase --signoff $1' for several, then push again (see CONTRIBUTING.md)." >&2
  exit 1
fi
echo "all $n commits signed off by their authors"
