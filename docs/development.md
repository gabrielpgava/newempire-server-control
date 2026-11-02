# Development

Use the exact Go version in `.go-version` and the Node version in root `.node-version`.
`go.mod` declares module
compatibility. Do not replace these authorities with a floating CI runtime.

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
make release-test    # Python automation tests and Go package/signature tests
make release-dry-run # complete local package exercise with an in-memory test key
```

`make security` needs the generated frontend (run `make check` first) because Go
embeds `web/dist`. The frontend must be built **before** Go compilation. Installed
native releases contain the UI and require no Node.js runtime. Build output and
data directories are ignored by Git. Do not commit generated private material.

`make check` fails on missing mandatory files, missing frontend scripts or an
ambiguous OpenAPI file. Keep exactly one of `docs/openapi.json`,
`docs/openapi.yaml`, or `docs/openapi.yml`; Redocly performs structural and
reference validation. Go tests include versioned migrations and the security
regressions present in each domain. Lockfiles must not drift during checks.

The normal commands run application tests on your machine. For isolation use
the repository's separately maintained `scripts/test-sandbox.sh` via
`make test-sandbox`, with its required cached dependencies/container image; see
[testing](testing.md). Never point tests at production instances, databases,
credentials or service units.

## Review and integration

Use small complete vertical slices, Conventional Commits and squash PR titles.
Feature/breaking PRs require `release:feature`; a label cannot turn a `chore:` title
into a releasing change. Include real test evidence, English documentation,
failure handling, compatibility, recovery and migration effects. Release code,
workflows and the trust root require owner review. Do not bypass checks to make
a release look complete. [Releases](releases.md) describes signing setup and the
exact publication/retry protocol.

Linux/systemd integration is a separate acceptance activity; a macOS build or
mocked service test cannot prove it. Docker is not silently substituted for an
implemented runtime provider. Keep update operations restricted to the panel;
release tooling must never restart Minecraft or modify worlds/content.
