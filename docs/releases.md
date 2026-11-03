# Releases and verified application updates

This repository is source-available under PolyForm Shield 1.0.0. A public source
repository, passing unit tests, or a draft is not evidence of a published release
or complete product acceptance. Check the actual release links.

## Strictly local publication

Validation, security checks, compilation, signing and publication run locally.
GitHub stores source and finished artifacts; do not enable, dispatch or otherwise
trigger Actions or cloud pipelines. The preserved `.github/workflows/release.yml`
is manual-only and unused. Legacy workflow verification remains compatible but
is not the local publication path.

Before any push, merge, tag or release, inspect the tracked workflow triggers and
stop if cloud execution could occur or cannot be ruled out. Preserve independent
PR/code-owner/latest-push approval and protected-main rules. A blocked merge is
not permission to bypass a safeguard or publish a different source revision.

## Local validation and release preparation

Use `.go-version`, `go.mod` and `.node-version` as the toolchain authorities.

```sh
make check
make security
make release-test
# Read-only SemVer/notes plan; no GitHub writes or signing key access.
make release-plan
# Unsigned packages + SBOM; VERSION is required and OUT must not exist.
make dist VERSION=0.0.0-dev.1 OUT=dist/local-evaluation
# Builds test packages; does not publish or use the production signing key.
make release-dry-run
```

`make dist` refuses existing outputs (including dangling symlinks) before any
frontend work. A new `OUT` must be outside the checkout or Git-ignored, such as
the default `dist/release-<version>`; unignored files would invalidate Go's clean
source stamp and the SBOM. It then builds the frontend and reuses the existing
Go packager for Linux amd64/arm64 and the Python SBOM generator. It requires clean
committed source, never reads the production signing key and never publishes.
An unsigned candidate is not a supported native installation or update asset.

The build dry-run also requires clean tracked source and refuses an existing
`dist/dry-run`; choose a new output directory with the Go command for repeat runs.
It creates an ephemeral key **in memory**, builds both architectures, generates
the SBOM, signs and verifies locally, and never creates a tag or contacts the
release API. Dry-run assets are not official update assets and cannot be
verified with the production trust root. No private key is retained.

## Authorized local publication

1. Review `make check`, `make security`, script/deployment tests and the local
   distro build for the exact consolidated source. Integrate `dev` into protected
   `main`; verify the resulting tree and commit before publication.
2. In the existing checkout, select the clean integrated `main`, verify official
   origin and protected remote-main identity, and create a read-only live plan:

    ```sh
    python3 -B scripts/release.py plan --live --local > dist/local-plan.json
    ```

   Create the ignored `dist/` parent first if absent. `plan --live --local` rejects
   dirty/non-main/forked sources, unprotected or moved remote main and workflow
    triggers other than the explicitly recognized manual-only form. It does not
    impersonate Actions or require `GITHUB_*` environment variables.
    Unrecognized/quoted/duplicate event-map syntax is rejected rather than
    guessed; preserve the existing plain block-style manual-only workflow.
3. Read `version` and `commit` from the plan as `VERSION` and `SHA`. Build unsigned
   candidates without the signing key, then build the signer from this same
   clean verified source. Use a new output directory for every attempt:

   ```sh
   make dist VERSION="$VERSION" OUT="dist/release-$VERSION"
   go build -mod=readonly -trimpath -o bin/release-tool ./scripts/release
   ```

4. Verify the private key file's ownership/permissions and the executable/source
   identity before exposing the key. Scope `RELEASE_SIGNING_KEY` to the signer
   process only; never export it into npm, tests or the publisher environment.
   `SIGNING_KEY_FILE` is an administrator-selected protected local file, not a
   downloaded secret. In a trusted local signing session:

   ```sh
   RELEASE_SIGNING_KEY="$(cat "$SIGNING_KEY_FILE")" bin/release-tool sign-local \
     -dir "dist/release-$VERSION" -version "$VERSION" -commit "$SHA"
   bin/release-tool verify-local \
     -dir "dist/release-$VERSION" -version "$VERSION" -commit "$SHA"
   ```

   The signer validates the pinned public identity, signs the updater manifest
   and deterministic source-bound local provenance, and verifies every payload.
   Keep build/test execution isolated and network-disabled; only the reviewed
   publication client needs credentials/network access for the GitHub API.
5. Publish using the explicit local path:

   ```sh
   python3 -B scripts/release.py publish --local --plan dist/local-plan.json \
     --assets "dist/release-$VERSION" --tool bin/release-tool
   ```

   The publisher verifies signatures before creating a draft, uploads without
    overwriting, downloads every asset with size bounds and a 60-second total
    deadline and re-verifies it, rechecks
    main/tag identity, publishes **last**, then verifies stable latest discovery.
    API/download operations and uploads explicitly select `github.com`, ignoring
    an ambient `GH_HOST`. Record the actual release URL and exact source SHA.
    Do not enable Actions.

Inspect actual release/tag state after any failure or lost response: publication
may have succeeded even if the client response was lost. An explicit
`RELEASE_RETRY_TAG=vX.Y.Z` live local plan can resume only its exact matching
unpublished draft. Every local asset, including deterministic provenance, must
match; retries cannot overwrite assets or a published tag. Never invent
provenance, replace a key without owner authorization, delete a tag to conceal a
failure, or substitute unsigned packages.

The committed base64 32-byte Ed25519 public key in
`deploy/release-public-key.txt` remains the trust root; the CLI linker field is
`main.releasePublicKey`. The signer expects `RELEASE_SIGNING_KEY` to be standard
padded base64 of the complete **64-byte Go Ed25519 private key** (seed followed
by public key), not PEM or a 32-byte seed. It reconstructs the key and requires
its public key to match the trust root. Keep recovery copies outside the
repository; never log, upload or include a private key in an archive or command
argument. Missing, malformed or mismatched keys remain publication blockers.

The owner explicitly replaced the initial unreleased key on 2026-11-03. Current
raw-public-key SHA-256 is
`d7ab48a9fa4414e04027255f140f6677a451e868470e14ef3c64770420d81a7f`.
Old-key evaluation installations require a reviewed manual migration; this is
not automatic key rotation or authorization for future replacements.

The panel's signed-update checks are unchanged. A present secret is not proof
that it contains a matching key; signing failure must leave publication stopped.
Verified publication is separate from real-host installation/update/rollback,
Minecraft sustained readiness and world-safety acceptance. Generated English
release notes link the source-bound ledger and state these unresolved limits.

## Version and PR policy

| Commit since last stable tag | Bump |
| --- | --- |
| `feat:` / `feat(scope):` | MINOR |
| `fix:`, `perf:`, `revert:` | PATCH |
| Conventional `!` or a `BREAKING CHANGE:` / `BREAKING-CHANGE:` footer | MAJOR |
| Only documentation, tests or internal maintenance | No release-producing bump |

The largest bump wins; an initial release starts from `0.0.0`. A breaking change
is a major bump even before `1.0.0`. English notes come from commit subjects and
bodies. Explain compatibility and recovery effects in the PR/docs too.

PR titles must be conventional. Maintainers must review that `feat` and breaking
titles have the `release:feature` label and that the label is not applied to a
non-release-producing title; there is no automated PR policy check.
Put `!` in a breaking PR's title, not just its body, so squash merging preserves
the release intent. A significant capability is not delivered until its release
and applicable updater acceptance are verified.

For read-only planning, `RELEASE_BUMP=auto|patch|minor|major` sets a minimum bump,
never a lower one than the commits require. `RELEASE_CHANNEL=stable|beta|rc`
selects stable or the next `X.Y.Z-beta.N` / `X.Y.Z-rc.N` sequence. These settings
do not publish anything by themselves. A retry must explicitly select the exact
unpublished draft; it never authorizes a cloud run or changed assets.

## Assets and updater contract

Every release contains:

- `new-empire-control_<version>_linux_amd64.tar.gz`
- `new-empire-control_<version>_linux_arm64.tar.gz`
- `documentation.tar.gz` (tracked LICENSE, README, root installer, docs and deployment examples)
- `sbom.cdx.json` (CycloneDX 1.6: actual Go build-info dependencies from **both**
  binaries, the Go standard library, and resolved production npm lock packages)
- `release.json`, `release.json.sig`, `checksums.txt`
- `provenance.json`, `provenance.json.sig` (locally signed source/subject evidence)

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

## Verify local provenance

Download all nine assets into a new directory. With a signer/verifier built from
independently reviewed source and the pinned public key, supply the expected
version and protected-main SHA:

```sh
bin/release-tool verify-local -dir DOWNLOADED_ASSET_DIRECTORY \
  -version VERSION -commit SHA
```

`provenance.json` is deterministic compact JSON with schema `1`, the official
repository, builder `local`, full source commit, version, and seven sorted
subjects (name, size and SHA-256), including `checksums.txt`.
`provenance.json.sig` authenticates its exact bytes with the same Ed25519 trust
root. This binds the signed release to a maintainer's source claim; it is **not**
GitHub OIDC/runner attestation or independent proof of a reproducible build.
The updater still consumes the unchanged signed `release.json` contract.

## Pinned local tooling

`govulncheck` is pinned to `golang.org/x/vuln/cmd/govulncheck@v1.9.0`;
the OpenAPI validator is `@redocly/cli@2.72.1`. npm's production audit and the Go
vulnerability database require network access. Source/toolchain consistency,
trimmed paths and fixed archive timestamps improve repeatability; independent
bit-for-bit rebuilds still need verification and are not asserted here.
