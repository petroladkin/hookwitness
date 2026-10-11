# Contributing to Hookwitness

Thanks for looking. The project is in early development; the shape of the
code is still settling, so **open an issue before a pull request** and say what
problem the change solves. That costs less than a pull request that cannot be
merged.

## Sign your commits (DCO)

Every commit needs a `Signed-off-by` line, which `git commit -s` adds:

```
Signed-off-by: Your Name <you@example.com>
```

It means you agree to the [Developer Certificate of
Origin](https://developercertificate.org/): you wrote the change, or have the
right to submit it, and you submit it under this project's license
([Apache-2.0](LICENSE)). The name and email must match the commit's author.
To fix a missing sign-off: `git commit --amend -s`, or
`git rebase --signoff <base>` for several commits.

## Branches

- Branch from `dev`, target `dev`. `main` only receives release branches.
- Branch names: `feature/<name>`, `fix/<name>`; `release/x.y` and
  `hotfix/<name>` are for maintainers.
- One branch, one concern. A fix and a refactor are two pull requests.

Enable the repository's git hooks once per clone:

```sh
git config core.hooksPath scripts/hooks
```

They refuse commits on `dev`/`main` and compiled binaries at the repository
root.

## Commits

- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`,
  `fix:`, `docs:`, `chore:`, `refactor:`, `test:`. The body says why, not
  what.
- Code, comments, identifiers, commit messages and docs are in English.
- Tests go with the change.

## Reporting a problem

Bugs go to the issue tracker: what you did, what you saw, what you expected.
Please do not report security problems in public issues; contact the
maintainer privately (a SECURITY.md with the channel will be added before the
first release).
