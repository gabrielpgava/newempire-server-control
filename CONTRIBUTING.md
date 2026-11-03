# Contributing

Read [AGENTS.md](AGENTS.md) and [the development guide](docs/development.md) before changing code. This repository is **source-available under PolyForm Shield 1.0.0**, not OSI-approved open source. Contributions are provided under the existing license; do not change it without the owner's approval.

Use English for code comments, documentation, messages, and UI copy. Keep changes focused and preserve unknown server files. Never test imports, upgrades, restores, or third-party JARs against a real player's world.

1. Branch from `main`; preserve existing work.
2. Implement a complete vertical slice, including failure paths, permissions, API contracts, and documentation.
3. Add regression tests. Use synthetic worlds and local fake upstream services by default.
4. Run the commands in [testing](docs/testing.md), record results and untested environments honestly.
5. Use a Conventional Commit PR title. Significant features use `feat:` and the `release:feature` label; breaking changes use `!` and include migration instructions.
6. Run `make check` and `make security` locally and record their results before merge. GitHub Actions and automatic releases are disabled by the owner's budget policy; do not re-enable them without approval. A significant feature is not delivered until its authenticated release is published and consumable by the self-updater; publication is currently blocked as described in [releases](docs/releases.md).

Report vulnerabilities privately according to [SECURITY.md](SECURITY.md). Do not attach master keys, database files, session cookies, RCON passwords, or real backup archives to issues.
