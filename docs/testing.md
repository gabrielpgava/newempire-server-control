# Testing and acceptance evidence

Run `make check` followed by `make security` on the complete integrated source.
These are local gates, not claims that every product acceptance criterion has
already been verified. Report actual commands, source revision, failures and
skipped checks in each PR. Automatic GitHub CI is disabled by the owner's budget
policy. The manual-only release workflow builds and verifies publication assets;
it does not replace the locally reviewed gates and needs explicit per-run approval.

## Automated gates

- Repository contracts: mandatory files, Node version authority, frontend scripts
  and exactly one OpenAPI document.
- `gofmt`, `go mod verify`, `go vet ./...` and a native `./cmd/control` build.
- `npm ci`, TypeScript checking, non-watch frontend tests and production build
  before compiling the Go-embedded UI.
- Pinned Redocly OpenAPI validation, including reference resolution.
- `go test -race -count=1 -timeout=5m ./...`: all packages, including SQLite
  migrations, downgrade/backup behavior and the domain security tests actually
  present in the source. This is not a substitute for missing regression tests.
- Python deployment/installer tests in both `make check` and `make test`, plus release-policy/SemVer/draft-publication tests and Go canonical-manifest,
  signature, archive/ELF, corrupt-asset and deterministic-packaging tests.
- Pinned `govulncheck` and `npm audit --omit=dev --audit-level=low`. Known production
  vulnerabilities fail the gate; unavailability of an audit service is a failure,
  not a clean security result.

Run only the automation suite with `make release-test`. The GitHub API is faked
in publication tests: they check write ordering, explicit draft retries, source
identity checks, read-back corruption detection and non-overwrite behavior. Makefile
tests also check missing-version refusal and unsigned distro/SBOM commands; workflow
contracts and pinned actionlint check manual triggers, permissions and secret scope. They
do not create releases. Cryptographic and archive tests use dummy fixtures and
test keys, never production signing material.

`make release-dry-run` exercises cross-compilation, the two binary-only archives,
the shared typed manifest, SBOM generation, signing and verification without a
release API write or private-key file. The ephemeral public key differs from the
production trust root, so these files are intentionally unusable for production
updates. A clean, complete committed source tree is required.

## Isolation

`make test-sandbox` delegates to `scripts/test-sandbox.sh`; read that script for
the current copied paths and exact test scope. It uses a read-only source and
module cache, an empty allowlisted environment, no external network, scratch-only
writes and container CPU/memory/process/file limits. Fetch/build required
dependencies outside the sandbox first. Missing cached dependencies are a blocker,
not permission to enable network access inside untrusted tests. The sandbox target
is separate from the normal local gates and may not include frontend/browser tests.

`scripts/test-web-sandbox.py --dependencies PRIVATE_LINUX_DEPENDENCY_DIRECTORY`
separately runs frontend tests/build without network or credentials. Its two
workers share a two-CPU quota, with a ten-minute watchdog for a cold constrained
run. Only validated bounded distribution files are copied back; a timeout is a
failed check, not evidence that the tests passed.
Frontend interaction tests allow five seconds for an asynchronous assertion and
thirty seconds for a complete multi-step scenario under that CPU quota. These
are correctness deadlines, not measured browser performance targets.

The deployment ownership regression is skipped for ordinary users. In an isolated
root container, give it a private root-owned `TMPDIR` with trusted ancestors (for
example, a 0700 tmpfs mounted at `/fixtures`). The installer deliberately rejects
world-writable ancestors such as `/tmp`; weakening that validation to accommodate
a test fixture would invalidate the ownership test.

Use a new temporary data directory, localhost-only test servers and disposable
instances for all manual checks. Never use a real world, production panel
database, signing secret or broadly privileged systemd account as a test fixture.

## Separately required Linux acceptance

Real systemd integration is not implied by unit tests and is not automatically
run on a developer's macOS host. In a dedicated disposable Linux VM/LXC, using
the documented least-privilege install:

1. Exercise start/readiness/stop/restart and restrictive runtime-helper permissions
   against a test Minecraft instance; distinguish a process from healthy readiness.
2. Verify the file/import/mod/runtime/backup mutation boundaries and failure
   recovery with disposable world data. Confirm backups stay outside instance roots.
3. Install an official signed release. Check the authenticated updater discovers
   the next stable release, rejects corrupt/wrong-source assets, and stages the
   expected architecture. Beta is explicit opt-in only.
4. Apply through the independent restricted panel helper; force a failed panel
   readiness check and verify panel binary/database/config recovery. Confirm that
   Minecraft is neither restarted nor modified. Schema mismatches require a
   separate administrator-planned migration.
5. Record the release URL, exact host architecture, service units, tested versions,
   commands and observed failures/recovery. Do not claim success without that
   evidence. No optional systemd job is wired to an unimplemented test command.

Publication also requires trusted source/permissions, a matching signing key,
uploaded/read-back assets and verified provenance from the protected-main manual
workflow. Enable Actions only for an explicitly authorized run and disable it
afterward. Mocked GitHub tests do not prove any of these external facts. See
[the release runbook](releases.md) for the current boundary.
