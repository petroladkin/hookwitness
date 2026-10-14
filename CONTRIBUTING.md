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
- CI runs `make check` and the container and API smoke tests on every pull
  request; all must pass.

Enable the repository's git hooks once per clone:

```sh
make hooks
```

They refuse commits on `dev`/`main` and compiled binaries at the repository
root.

## Commits

- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`,
  `fix:`, `docs:`, `chore:`, `refactor:`, `test:`. The body says why, not
  what.
- Code, comments, identifiers, commit messages and docs are in English.
- Tests go with the change, and `make check` passes (Go 1.27+).

## Documentation

User documentation lives in `docs/`, with a Ukrainian twin of every page in
`docs/uk/` (the API reference `docs/api.md` is English only). A change that
changes what a user sees changes the page that describes it, and its twin, in
the same pull request.

- The shell examples (```` ```sh ```` blocks) are run by `make docs-check`
  against a fresh build (needs curl, jq, openssl): a command ending in
  `# pass`, `# fail: <reason>` or `# 200` asserts the verdict or status of the
  request it sent, and comment lines right after a command are its expected
  output. `scripts/docscheck/main.go` documents the rest. Twins keep the same
  `sh` blocks; a repository test checks that, the relative links and anchors,
  and that every image is used.
- Screenshots in `docs/images/` come from `make media` (Node 22+ and Chrome):
  a seeded instance shot at 1280 px in the light theme. Regenerate them when
  the UI visibly changes; the times on the pages differ on every run, so
  commit the images whose content changed, not every file git reports.

## Reporting a problem

Bugs go to the issue tracker: what you did, what you saw, what you expected.
Please do not report security problems in public issues; report them
privately as [SECURITY.md](SECURITY.md) describes.
