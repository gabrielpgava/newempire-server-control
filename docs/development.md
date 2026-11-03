# Development

Use the exact Go version in `.go-version` and the Node version in root `.node-version`.
`go.mod` declares module
compatibility. Do not replace these authorities with a floating toolchain.

The module is `github.com/gabrielpgava/newempire-server-control`. Domain types
live in `internal/model`; domain packages do not import HTTP handlers. Read
`AGENTS.md` and [the integration contract](integration-contract.md) before changes.
The source-available license is PolyForm Shield 1.0.0.

## Common commands

```sh
make check           # frontend, formatting, OpenAPI, vet, race tests, native build
make security        # Go vulnerability analysis + production npm audit
make frontend        # npm ci, typecheck, non-watch tests, build
make test            # frontend build and all Go/release tests
make build           # frontend + bin/new-empire-control
make dist VERSION=0.0.0-dev.1 OUT=dist/local-evaluation # unsigned Linux packages + SBOM
make release-plan    # read-only conventional version/notes preview
make release-test    # Python automation tests and Go package/signature tests
make release-dry-run # complete local package exercise with an in-memory test key
```

`make security` needs the generated frontend (run `make check` first) because Go
embeds `web/dist`. The frontend must be built **before** Go compilation. Installed
native releases contain the UI and require no Node.js runtime. Build output and
data directories are ignored by Git. Do not commit generated private material.

`make check` fails on missing mandatory files, missing frontend scripts or an
ambiguous OpenAPI file. Keep exactly one of `docs/api/openapi.json`,
`docs/api/openapi.yaml`, or `docs/api/openapi.yml`; Redocly performs structural and
reference validation. Go tests include versioned migrations and the security
regressions present in each domain. Lockfiles must not drift during checks.

All checks, builds, signing and publication run locally under the owner's policy.
Attach results to the PR; do not enable or dispatch the preserved unused manual
workflow. No automatic validation workflow or trigger is restored.
`make dist` requires a version, clean committed
source and a new output directory outside the checkout or Git-ignored (such as
`dist/local-evaluation`). Existing outputs, including dangling symlinks, are
rejected before frontend work; it never signs or publishes its candidates.

The normal commands run application tests on your machine. For isolation use
the repository's separately maintained `scripts/test-sandbox.sh` via
`make test-sandbox`, with its required cached dependencies/container image; see
[testing](testing.md). Never point tests at production instances, databases,
credentials or service units.

## Review and integration

Use small complete vertical slices, Conventional Commits and squash PR titles.
Feature/breaking PRs require `release:feature`; a label cannot turn a `chore:` title
into a releasing change. Include real test evidence, English documentation,
failure handling, compatibility, recovery and migration effects. Release code
and the trust root require owner review. Do not bypass checks to make a release
look complete. [Releases](releases.md) describes the local tooling, protected-main
publication and failure/retry boundaries.

Linux/systemd integration is a separate acceptance activity; a macOS build or
mocked service test cannot prove it. Docker is not silently substituted for an
implemented runtime provider. Keep update operations restricted to the panel;
release tooling must never restart Minecraft or modify worlds/content.

## Offline recovery CLI changes

The offline path in `cmd/control/recovery*.go` holds the same exclusive
`panel.lock` as `serve`. It opens existing private panel state without migrations
or initialization. Never call `App.Start` or start job workers from recovery:
`Engine.RunOne(ctx, jobID)` must execute only the named queued job. The zero-worker
engine startup is used solely to mark orphaned running jobs interrupted under the
exclusive process lock. Backup and restore use the existing domain/API jobs;
recovery must not grow a second archive or restore implementation.

Run the CLI regressions in the isolated Linux sandbox after building the embedded
frontend:

```sh
make frontend
bash scripts/test-sandbox.sh ./cmd/control ./internal/jobs ./internal/api ./internal/backups ./internal/auth ./internal/database ./internal/secrets ./internal/files ./internal/modrinth
```

Recovery tests create disposable initialized state, use a fake runtime rather
than host service units, and execute the unprivileged CLI cases as UID/GID 65534
when the sandbox itself runs as root. They cover missing confirmation/state/key,
unsafe links, the exclusive panel lock, TOTP login, private CLI-session handling,
nonredirecting requests, explicit-job isolation, verified backup/restore with no
panel HTTP server, retained-tree recovery and content rollback without world
rollback. These tests are not evidence of real systemd/ACL/Minecraft acceptance.
See [disaster recovery](disaster-recovery.md) for exact administrative commands
and the conservative refusal cases. Do not use production state for recovery
tests.
