# Releases and verified application updates

This repository is source-available under PolyForm Shield 1.0.0. A public source
repository, passing unit tests, or a draft is not evidence of a published release
or complete product acceptance. Check the actual workflow and release links.

## Maintainer setup (required before the first release)

1. Protect `main`: require PRs, passing **Pull request policy** and **Validate**
   checks, code-owner review for release/security files, and no force pushes or
   deletions. Restrict bypass permissions. Use squash merges so the checked PR
   title becomes the commit subject. Protect `v*` tags from independent creation,
   movement and deletion; allow only the release automation/maintainers required
   to publish. Enable immutable releases where supported.
2. Create the GitHub environment **release**, restrict deployments to protected
   `main`, and configure its approval requirements. GitHub's branch protection
   and environment settings are external controls; workflow files cannot create
   or prove those settings themselves.
3. Commit the base64 32-byte Ed25519 public key in
   `deploy/release-public-key.txt`. The initial pinned trust root is
   `DVI1QlV02lloZudVLPo64ZEOjQiEQR5PKyMlA+eVdG8=`. This is public, not a secret.
   The CLI linker field is `main.releasePublicKey`; the build reads the committed
   key rather than selecting a new trust root.
4. Set **RELEASE_SIGNING_KEY** in the **release** environment: standard padded
   base64 of the complete **64-byte Go Ed25519 private key** (seed followed by
   public key), not a PEM file or 32-byte seed. Keep its protected recovery copy
   outside the repository. The signer reconstructs the key from its seed and
   requires the derived public key to equal the committed trust root. A missing,
   malformed or mismatched key blocks publication. Never print it, pass it as a
   command argument, upload it, or include it in an archive.
5. Permit the workflow's scoped `GITHUB_TOKEN` to write releases and artifact
   attestations and to request an OIDC token. No PAT is required. Ensure GitHub
   artifact attestations are available for the repository.
6. Provide all mandatory source/docs/deployment files, the npm lockfile and the
   root Node version authority (`.node-version`).
   Go comes from `.go-version`, with module compatibility in `go.mod`.

Create the `release:feature` label before opening feature PRs. Do not bypass
failing tests or a missing signing secret simply to produce a release.

## One orchestrated workflow

`.github/workflows/release.yml` runs on pushes to `main` and explicit manual runs.
There is no tag-trigger chain: tags created by `GITHUB_TOKEN` do not need to start
another workflow.

1. The reusable CI workflow validates formatting, frontend installation,
   typechecking, tests and build, OpenAPI, `go vet`, all race-enabled Go tests
   (including migrations and domain security tests), a native build, dependency
   integrity and production dependency vulnerabilities.
2. A read-only prepare job checks the official repository, protected-main
   workflow context, current main head, and exact checkout SHA. It computes a
   version from complete commit messages since the last reachable stable tag.
   Prerelease tags never become the stable baseline. Stale runs fail closed; the
   workflow at the new main head must release the accumulated changes.
3. With no release credentials or OIDC write permission, preparation builds the
   frontend and both Linux binaries, packages them deterministically, and writes
   the compact manifest and CycloneDX SBOM. Only this workflow run's short-lived,
   immutable artifact is transferred to the signing job. Shared build caches are
   disabled. The deployed binaries do not require Node.js.
4. The environment-gated publish job rechecks main before accessing the signing
   secret. It performs no npm installation or dependency build. The private key
   is present only in the signing step's environment. It signs the exact manifest
   bytes, verifies payloads, and produces GitHub OIDC-backed provenance.
5. After verifying provenance for the expected repository, workflow, protected
   ref and source SHA, automation creates a **draft** targeting that exact SHA.
   It uploads assets without `--clobber`, downloads every asset, checks inventory,
   size and SHA-256, then rechecks the Ed25519 signature, canonical manifest,
   archives, checksums, SBOM and provenance. Publication is the last mutation.
   Stable releases are explicitly made latest; prereleases are not.
6. It verifies the published tag's resolved SHA and stable `releases/latest`
   discovery. A maintainer must additionally exercise the panel's authenticated
   check/stage/apply/recovery paths on an isolated Linux installation. A live
   successful updater run is not implied by publication alone.

Release concurrency never cancels an active run. GitHub may replace an older
pending run with a newer pending run; commits are still included from the stable
baseline. No workflow checks out PR head code with a write token or release
secret. Fork PR checks use read-only permissions, without environment secrets.

## Version and PR policy

| Commit since last stable tag | Bump |
| --- | --- |
| `feat:` / `feat(scope):` | MINOR |
| `fix:`, `perf:`, `revert:` | PATCH |
| Conventional `!` or a `BREAKING CHANGE:` / `BREAKING-CHANGE:` footer | MAJOR |
| Only documentation, tests or internal maintenance | No automatic release |

The largest bump wins; an initial release starts from `0.0.0`. A breaking change
is a major bump even before `1.0.0`. English notes come from commit subjects and
bodies. Explain compatibility and recovery effects in the PR/docs too.

PR titles must be conventional. `feat` and breaking titles require the
`release:feature` label. That label is rejected for a non-release-producing title.
Put `!` in a breaking PR's title, not just its body, so squash merging preserves
the release intent. A significant capability is not delivered until its release
and applicable updater acceptance are verified.

For a manual release, dispatch **Release** on **main**:

- `bump=patch` (default), `minor`, `major`, or `auto` sets a minimum, never a lower
  bump than the commits require.
- `channel=stable` publishes stable; `beta` or `rc` produces `X.Y.Z-beta.N` or
  `X.Y.Z-rc.N` using the next existing tag sequence. For example, features after
  `v1.0.0` yield `1.1.0-beta.1`; a later stable run yields `1.1.0`, not `1.1.1`.
- Leave `retry_tag` empty unless resuming a known failed draft.

## Assets and updater contract

Every release contains:

- `new-empire-control_<version>_linux_amd64.tar.gz`
- `new-empire-control_<version>_linux_arm64.tar.gz`
- `documentation.tar.gz` (tracked LICENSE, README, docs and deployment examples)
- `sbom.cdx.json` (CycloneDX 1.6: actual Go build-info dependencies from **both**
  binaries, the Go standard library, and resolved production npm lock packages)
- `release.json`, `release.json.sig`, `checksums.txt`, `provenance.jsonl`

Each binary archive contains **exactly one regular file** named
`new-empire-control`, mode `0755`, with no directory prefix, links or extra files.
The verifier checks ELF64, little endian and the declared architecture. Both
archive and expanded-binary limits are 512 MiB. Docs are deliberately outside
these archives because the updater rejects extra entries.

Manifest versions omit the tag's `v`. Its exact field order is:

```json
{"version":"1.2.0","repository":"gabrielpgava/newempire-server-control","schema":1,"assets":[{"name":"new-empire-control_1.2.0_linux_amd64.tar.gz","os":"linux","arch":"amd64","size":12345,"sha256":"<lowercase SHA-256>"},{"name":"new-empire-control_1.2.0_linux_arm64.tar.gz","os":"linux","arch":"arm64","size":12345,"sha256":"<lowercase SHA-256>"}]}
```

`schema` is `database.SchemaVersion` (currently 1), not a JSON schema revision.
Only the two sorted binary archives appear in the manifest.
`updates.CanonicalManifest` is the authority: compact UTF-8, no BOM or trailing
newline. `release.json.sig` is standard padded base64 of the Ed25519 signature of
those exact bytes, also without a newline. Unknown/duplicate fields or alternate
serialization fail validation. Schema-changing releases need an explicit manual
migration; the updater refuses to silently cross a schema boundary.

Checksums cover archives, documentation, SBOM, manifest and signature. The
provenance bundle authenticates those payloads **and** the checksum file; it
cannot attest itself. Checksums alone are not an authenticity check. The SBOM is
a dependency inventory, not a claim that every component is vulnerability-free
or that a dependency graph/license audit has been performed.

## Recovery and local dry-run

A failed publish leaves an unpublished draft whenever a draft was already
created. Inspect its logs and assets. Dispatch the same main SHA with the same
bump/channel and the exact `retry_tag` (for example `v1.1.0`). Only a matching
draft, source SHA, version and channel can be resumed. Existing payload bytes
must match the fresh build; missing files can be uploaded. A previous provenance
bundle is reused only after cryptographically verifying the same payload and
source identity, since new attestations have new timestamps. Published releases
are never overwritten or reused.

Unexpected assets, mismatched bytes, moved tags, or a changed main head require
administrator investigation; the workflow does not delete or replace them.
Resolve the draft explicitly and rerun the appropriate current-main release.
Do not delete a published tag/release to hide a failure. If publication succeeded
but a later discovery check failed, first inspect the existing published release
rather than requesting another version blindly.

```sh
# Read-only SemVer/notes plan; no GitHub writes or signing key access.
python3 -B scripts/release.py plan
make release-test
# Requires the complete integrated source and frontend toolchain/dependencies.
make release-dry-run
```

The build dry-run refuses modified tracked files and an existing
`dist/dry-run`; choose a new output directory with the Go command for repeat runs.
It creates an ephemeral key **in memory**, builds both architectures, generates
the SBOM, signs and verifies locally, and never creates a tag or contacts the
release API. Dry-run assets are not official update assets and cannot be
verified with the production trust root. No private key is retained.

For a downloaded official release, independently verify provenance before
trusting its contents (replace `SHA` with the expected main commit):

```sh
gh attestation verify new-empire-control_VERSION_linux_amd64.tar.gz \
  --repo gabrielpgava/newempire-server-control --bundle provenance.jsonl \
  --signer-workflow gabrielpgava/newempire-server-control/.github/workflows/release.yml \
  --signer-digest SHA --source-digest SHA --source-ref refs/heads/main \
  --deny-self-hosted-runners
```

## Pinned tooling

Action refs were resolved from GitHub's upstream tag API; Dependabot proposes
reviewed updates, not automatic publication or permission changes.

| Action | Verified version | Immutable commit |
| --- | --- | --- |
| actions/checkout | v7.0.1 | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| actions/setup-go | v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` |
| actions/setup-node | v7.1.0 | `949feb2413d6458794dcd2491c4babbbce0c15c1` |
| actions/upload-artifact | v7.0.2 | `cf430e030ddbb5b0abf93d22962f4752f3646cd9` |
| actions/download-artifact | v8.0.2 | `9000827ccba6bdab643e8b6fd33ac0654aef8333` |
| actions/attest | v4.2.2 | `1e69f48acb82d1966a394da916b4c1698aa569d6` |

`govulncheck` is pinned to `golang.org/x/vuln/cmd/govulncheck@v1.1.4`;
workflow validation uses `github.com/rhysd/actionlint/cmd/actionlint@v1.7.12`;
the OpenAPI validator is `@redocly/cli@2.72.1`. npm's production audit and the Go
vulnerability database require network access. Source/toolchain consistency,
trimmed paths and fixed archive timestamps improve repeatability; independent
bit-for-bit rebuilds still need verification and are not asserted here.
