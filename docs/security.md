# Threat model and security boundaries

## Assets and actors

Protect Minecraft world progress, unknown/manual content, backup integrity, local host files, credentials, session identities, player privacy, and panel update identity. Lower-trust inputs include unauthenticated browser requests, viewer/operator users, uploaded archives and names, third-party metadata/downloads, and Minecraft-controlled instance files. The administrator, configured Java executable allowlist, protected data root, and root-owned service helper configuration are trusted boundaries.

## Controls

| Boundary | Required control |
| --- | --- |
| First-run takeover | Random protected setup token, one owner transaction, no public registration after bootstrap |
| Authentication | Argon2id, bounded hashing concurrency/password size, throttling, optional TOTP |
| Browser requests | Same-origin mutations, CSRF tokens, HTTP-only SameSite cookies, HTTPS off loopback, CSP |
| Instance isolation | Explicit instance IDs, role/grant checks, isolated ports/roots, serialized mutations |
| File traversal/races | Descriptor-rooted operations, reject unsafe names and archive links/devices, bounded expansion |
| Data loss | Dry-run preview, verified restore point, offline mutation, commit barrier and retained recovery data |
| Third-party downloads | Exact permitted origins, public-address DNS checks at dial time, HTTPS redirect policy, authoritative hashes |
| Host privilege | Unprivileged HTTP process; root-owned allowlisted helper, never arbitrary shell commands |
| RCON exposure | Local client plus host firewall isolation before server startup |
| Credential disclosure | Purpose-bound AES-GCM storage, protected master key, redacted API/logs |
| Supply-chain replacement | Official GitHub release identity, pinned Ed25519 manifest signature, SHA-256, architecture/version policy |
| Fork pull requests | Read-only checks, no publishing/signing secrets, no `pull_request_target` executing fork code |

## Residual risks

Executing a downloaded mod/JAR remains code execution as the Minecraft user. Root, the deployment administrator, or a compromised allowed Java binary can bypass panel guarantees. Files outside selected backup scope and externally changing data may not be recoverable. Online backups coordinate Minecraft saves but are not general transactional snapshots of every mod's private storage.

Disk exhaustion, host crashes, failed filesystem sync, and interrupted migrations need tested recovery. A successful unit test is not evidence of Linux firewall, systemd, real-world Minecraft, Proxmox, or offsite-provider acceptance. Consult [testing](testing.md) for actual evidence and gaps.
