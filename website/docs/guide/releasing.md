# Releasing

Releases are fully automated. Merging a PR into `main` publishes a new version; nobody tags by hand.

## Flow

On every push to `main` (workflow `auto-release.yml`):

1. **version**: compute the next semver tag from the existing `v*` tags.
2. **test**: run the Dagger `test` function (`go test -race`).
3. **publish**: once the tests pass, run the Dagger `publish-image` (multi-arch `linux/amd64` + `linux/arm64`, to `ghcr.io/lerenn/llm-proxy-manifestd`) and `publish-chart` (OCI chart, to `oci://ghcr.io/lerenn/charts`) functions.
4. **tag**: cross-compile the `lpmctl` CLI with the Dagger `build-cli` function (`lpmctl-<os>-<arch>` for linux/darwin × amd64/arm64, plus `lpmctl-sha256sums.txt`), push the git tag on the merged commit and create a GitHub Release with generated notes and those binaries as assets.

The tag is created **last** on purpose: if publishing fails, no tag exists, so re-running the workflow recomputes the same version instead of skipping one. Runs are serialized (`concurrency`) so two merges never race for the same version, and the tag step fails if the tag already exists.

Pushes that only touch Markdown files or `website/` do not release.

## Version bump rule

Read from the merge commit message (with squash-merge: the PR title and body):

| Marker | Bump |
| --- | --- |
| `#major` anywhere | major |
| `#minor` anywhere, or subject starting with `feat:` / `feat(scope):` | minor |
| anything else | patch |

The first release, when no `v*` tag exists, is `v0.1.0`.

## Manual tags

Pushing a `v*` tag by hand still runs `release.yml`, which publishes the same image and chart and attaches the `lpmctl` binaries to the tag's GitHub Release (created if missing). Tags created by the automation use `GITHUB_TOKEN`, which by design does not trigger other workflows, so they are not published twice.
