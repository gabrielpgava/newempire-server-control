# Security policy

## Support and reporting

This is a new project. No independent audit or production-readiness certification is claimed. Security fixes target the latest published stable release; older releases should be updated after backup and release-note review.

Use the repository's **Security → Report a vulnerability** private reporting form if enabled. If unavailable, contact the repository owner privately through their GitHub profile to arrange a secure channel. Do not put vulnerability details, credentials, or player data in public issues.

Include affected version/commit, deployment method, trust boundary, a minimal synthetic reproduction, impact, and a suggested regression test. Never probe another person's panel or world.

## Deployment boundaries

- HTTPS is required for non-loopback browser access. Keep the panel behind a trusted reverse proxy or private network.
- The web process must not run as root. Restrict service helpers to validated instance operations.
- Only configured, administrator-allowlisted Java binaries and import roots are permitted.
- Keep RCON off public interfaces using the installed host firewall controls; a localhost-only client does not itself secure a listening server.
- Backups and the credential master key must be protected outside Minecraft instance roots. Recover the key with the database; a database alone cannot decrypt credentials.
- Third-party JARs execute as the Minecraft account and are inherently executable content. Hash verification establishes provenance/integrity, not safety.
- Only signed, pinned-identity panel releases are accepted. Missing signatures must cause failure, not fallback.

See [the threat model](docs/security.md), [disaster recovery](docs/disaster-recovery.md), and the tested/untested matrix in [testing](docs/testing.md).
