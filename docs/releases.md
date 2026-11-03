# Releases and verified application updates

This repository is source-available under PolyForm Shield 1.0.0. A public source
repository, passing unit tests, or a draft is not evidence of a published release
or complete product acceptance. Check the actual release links.

## Normally disabled; manual publication only

Automatic CI and release publication remain disabled to avoid runner charges.
The owner approved `.github/workflows/release.yml` with **workflow_dispatch only**.
Each run needs explicit owner authorization, including its possible runner cost;
merges and pushes do not start it. Run validation/security locally before merging.
No reusable CI job, push/PR trigger or paid publication service is introduced.

A maintainer must enable Actions only for the authorized release run, monitor it
and disable Actions afterward, including on failure. The workflow has no
administrative token to change repository settings or protection rules itself.
Preserve PR/code-owner/latest-push approval and protected-branch release rules.
Any separately owner-authorized one-merge or one-run approval exception must be
backed up, narrowly applied and immediately restored; it is not reusable.

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

## Authorized manual publication

1. Review `make check`, `make security`, script/deployment tests and the local
   distro build for the exact consolidated source. Integrate `dev` into protected
   `main`; verify the resulting tree and commit before publication.
2. Record the original Actions permissions and release environment rules. Confirm
   no other run is active, only the manual release workflow will be enabled, and
   the `release` environment restricts deployment to protected branches.
3. Enable Actions and the release workflow only for this explicitly authorized
   attempt, then dispatch **once** at current `main`:

   ```sh
   gh workflow run release.yml --ref main -f bump=auto -f channel=stable -f retry_tag=
   ```

4. Preparation checks official protected-main identity, computes the version and
   builds both packages/SBOM with pinned tools. It has read-only permissions and
   no signing secret. Only this run's plan and unsigned assets pass to publication;
   no executable signer is transferred.
5. Approve the exact run's protected `release` deployment. Publication has only
   contents/attestations/OIDC write permissions. It builds the signer from its fresh
   exact-SHA checkout without running npm. That signer receives `RELEASE_SIGNING_KEY`
   in its own step, requires the pinned public identity,
   and creates the signature/checksums. GitHub attests every payload.
6. The existing publisher verifies signatures/provenance before creating a
   draft, uploads without overwriting, downloads all assets and re-verifies
   them, checks current main/tag identity, then publishes **last** and verifies
   stable latest discovery. Record the actual release URL and exact source SHA.
7. Restore any approved temporary environment exception immediately after
   approval. Disable Actions when the run completes or fails, read back those
   settings, and retain non-secret evidence. Use a `finally`/trap-controlled
   administrative session; do not leave controls weakened after an interruption.

`scripts/release.py` still requires real protected-main workflow context and
GitHub OIDC-backed provenance for the exact SHA. Do not spoof that context or
call its live commands as a local publishing shortcut. A failed run consumes
its authorization: investigate the precise failure before asking for another
run. Inspect the actual release and tag state: a failure before draft creation
leaves no draft, while a lost final response can leave an already published
release. `retry_tag` can resume only the exact matching unpublished draft, needs
separate authorization, and never permits overwriting an asset or published tag.
Never invent provenance, rotate the trust root, delete a tag to conceal a
failure, or publish unsigned packages to satisfy the release policy.

The committed base64 32-byte Ed25519 public key in
`deploy/release-public-key.txt` remains the trust root; the CLI linker field is
`main.releasePublicKey`. The signer expects `RELEASE_SIGNING_KEY` to be standard
padded base64 of the complete **64-byte Go Ed25519 private key** (seed followed
by public key), not PEM or a 32-byte seed. It reconstructs the key and requires
its public key to match the trust root. Keep recovery copies outside the
repository; never log, upload or include a private key in an archive or command
argument. Missing, malformed or mismatched keys remain publication blockers.

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
do not publish anything locally. The manual workflow exposes the same choices;
an explicit draft retry still requires separate owner run authorization.

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

## Verify workflow provenance

The manual workflow retains the original trusted workflow path. Replace `SHA`
with the expected protected-main commit and verify each payload against the
downloaded bundle; this command is not evidence that a release already exists:

```sh
gh attestation verify new-empire-control_VERSION_linux_amd64.tar.gz \
  --repo gabrielpgava/newempire-server-control --bundle provenance.jsonl \
  --signer-workflow gabrielpgava/newempire-server-control/.github/workflows/release.yml \
  --signer-digest SHA --source-digest SHA --source-ref refs/heads/main \
  --deny-self-hosted-runners
```

## Pinned local tooling

`govulncheck` is pinned to `golang.org/x/vuln/cmd/govulncheck@v1.9.0`;
the OpenAPI validator is `@redocly/cli@2.72.1`. npm's production audit and the Go
vulnerability database require network access. Source/toolchain consistency,
trimmed paths and fixed archive timestamps improve repeatability; independent
bit-for-bit rebuilds still need verification and are not asserted here.
