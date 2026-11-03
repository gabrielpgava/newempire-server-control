# Releases and verified application updates

This repository is source-available under PolyForm Shield 1.0.0. A public source
repository, passing unit tests, or a draft is not evidence of a published release
or complete product acceptance. Check the actual release links.

## Current status: GitHub Actions disabled

The owner requested removal of GitHub Actions to avoid runner usage and possible
charges. The CI and release workflows and their Dependabot update entry have
been removed. Checks must run locally; merges do not publish releases. Do not
restore workflows or introduce paid CI/release services without owner approval.

Removing source files does not disable Actions repository-wide, cancel existing
runs, or modify remote settings. The earlier branch configuration required
**Pull request policy** and **Validate**. If those checks remain required, a
maintainer must review and remove the obsolete check requirements separately,
while retaining PR, code-owner and approval protections. Changes on other
branches can still trigger their own workflows until separately addressed.

## Local validation and release preparation

Use `.go-version`, `go.mod` and `.node-version` as the toolchain authorities.

```sh
make check
make security
make release-test
# Read-only SemVer/notes plan; no GitHub writes or signing key access.
python3 -B scripts/release.py plan
# Builds test packages; does not publish or use the production signing key.
make release-dry-run
```

The build dry-run requires clean tracked source and refuses an existing
`dist/dry-run`; choose a new output directory with the Go command for repeat runs.
It creates an ephemeral key **in memory**, builds both architectures, generates
the SBOM, signs and verifies locally, and never creates a tag or contacts the
release API. Dry-run assets are not official update assets and cannot be
verified with the production trust root. No private key is retained.

## Publication blocker and trust requirements

There is currently no supported end-to-end official publication path. The
retained `scripts/release.py` live planning/publication commands require the
removed protected-main workflow context and GitHub OIDC-backed provenance for
its exact source SHA. They are not a supported manual publishing alternative;
do not spoof that context or bypass their checks. Mocked publisher tests and
local dry-runs remain useful, but do not establish release authenticity.

A future publication process requires owner approval and must preserve trusted
source identity, least-privilege credentials, both architecture packages,
signature/provenance verification, draft asset read-back, non-overwrite behavior,
exact tag/SHA matching and stable discovery before updater acceptance. Failed or
partial drafts require explicit investigation; never overwrite a published
release or delete a tag to conceal a failure. Do not invent provenance or
publish unsigned packages merely to satisfy the release policy.

The committed base64 32-byte Ed25519 public key in
`deploy/release-public-key.txt` remains the trust root; the CLI linker field is
`main.releasePublicKey`. The signer expects `RELEASE_SIGNING_KEY` to be standard
padded base64 of the complete **64-byte Go Ed25519 private key** (seed followed
by public key), not PEM or a 32-byte seed. It reconstructs the key and requires
its public key to match the trust root. Keep recovery copies outside the
repository; never log, upload or include a private key in an archive or command
argument. Missing, malformed or mismatched keys remain publication blockers.

The panel's signed-update checks are unchanged. No unsigned update fallback or
substitute hosted service is introduced by removing Actions.

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
do not publish anything. Workflow dispatch and draft retry are not available.

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

## Retired workflow provenance reference

For assets produced by the retired workflow, the verification identity remains
the old workflow path. The following is a historical verification reference,
not an active publication procedure or a claim that an official release exists.
Replace `SHA` with the expected main commit:

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
