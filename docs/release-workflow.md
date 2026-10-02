# Release workflow

One milestone = one release. Versions follow
[Semantic Versioning](https://semver.org/) and the changelog follows
[Keep a Changelog](https://keepachangelog.com/). Tags are `vX.Y.Z`.

Everything is written in **English**, including release notes.

## 1. Release gate

A release is ready when the milestone has **no open issues**:

```bash
gh api repos/{owner}/{repo}/milestones --jq '.[] | select(.title=="vX.Y.Z") | {title, open_issues, closed_issues}'
```

- Open issues that will not make it: move them to the next milestone
  (`gh issue edit <N> --milestone vX.Y.(Z+1)`).
- The default branch must have a green `ci-ok`.

## 2. Choose the version

| Change | Bump |
|---|---|
| Bug fixes only | patch (`1.2.3` → `1.2.4`) |
| New, backward compatible features | minor (`1.2.3` → `1.3.0`) |
| Breaking changes | major (`1.2.3` → `2.0.0`) |

Before `1.0.0`, breaking changes bump the minor version. The milestone title
already holds the planned version. Change the milestone title if the plan changed.

## 3. Prepare the CHANGELOG (pull request)

```bash
git switch -c chore/release-vX.Y.Z
```

In `CHANGELOG.md`:

- Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`.
- Add a fresh empty `## [Unreleased]` above it.
- Group entries under Added, Changed, Deprecated, Removed, Fixed, Security.
- Update the compare links at the bottom, if the file has them.
- Bump `serverInfo.version` in `mcp.go` (the version the MCP server reports) to `X.Y.Z`.

Open a PR titled `chore: release vX.Y.Z`, wait for `ci-ok`, squash merge
(`gh pr merge --squash --delete-branch`). Push the branch with an explicit ref:

```bash
git push -u origin refs/heads/chore/release-vX.Y.Z
```

## 4. Tag

Tag the merge commit on the default branch with an **annotated** tag:

```bash
git switch main && git pull --ff-only
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin refs/tags/vX.Y.Z
```

The tag is also the Go module version of `github.com/crazy-goat/xdbg`, so
`go install github.com/crazy-goat/xdbg@vX.Y.Z` works after tagging.

## 5. GitHub Release

Pushing the tag starts `.github/workflows/release.yaml`. It builds the `xdbg` binary for
linux and darwin on amd64 and arm64, creates the GitHub Release with the notes from the
matching `CHANGELOG.md` section, and attaches the binaries and a `checksums.txt` file.
It fails when the section is missing. The workflow reads `CHANGELOG.md` from the tagged
commit, so the release PR with the `## [X.Y.Z]` section must be **merged before** you
tag. Tags with a `-` (for example `v0.1.0-rc.1`) become pre-releases.

This is an intentional difference from the shared `release.yml` in `crazy-goat/.github`,
which only creates the release and cannot attach build artifacts.

```bash
gh run watch
gh release view vX.Y.Z
```

## 6. Close the milestone

```bash
gh api -X PATCH repos/{owner}/{repo}/milestones/<number> -f state=closed
```

Make sure the next milestone `vX.Y.(Z+1)` (or the next minor) exists.

## 7. After the release

- Check that install instructions work with the new version (`go install`, release binaries, `install.md`).
- If something is wrong, do not move the tag. Fix forward with a patch release.

## Checklist

- [ ] Milestone has no open issues, CI is green
- [ ] CHANGELOG section `[X.Y.Z] - date` written, `[Unreleased]` is empty
- [ ] `serverInfo.version` in `mcp.go` bumped to `X.Y.Z`
- [ ] Release PR merged
- [ ] Annotated tag `vX.Y.Z` pushed
- [ ] GitHub Release exists with the CHANGELOG notes and the four binaries
- [ ] Milestone closed, next milestone exists
