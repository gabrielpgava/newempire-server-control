# Threat model and security boundaries

## Assets and actors

Protect Minecraft world progress, unknown/manual content, backup integrity, local host files, credentials, session identities, player privacy, and panel update identity. Lower-trust inputs include unauthenticated browser requests, viewer/operator users, uploaded archives and names, third-party metadata/downloads, and Minecraft-controlled instance files. The deployment administrator, configured Java executable allowlist, protected panel data/backup roots, and root-owned service-helper configuration are trusted boundaries.

The controls below describe the current application integration and its deployment obligations, not certification or proof of a live deployment. Domain packages do not independently provide HTTP authorization, runtime shutdown, durable jobs, or complete recovery workflows.

## Controls

| Boundary | Current mechanism and deployment obligation |
| --- | --- |
| First-run takeover | Random protected setup token, one-owner bootstrap transaction, no public registration after bootstrap |
| Authentication | Argon2id, bounded hashing concurrency/password size, process-local throttling, optional TOTP |
| Browser requests | Same-origin mutations, CSRF tokens, HTTP-only SameSite=Strict cookies, HTTPS off loopback, CSP |
| Instance isolation | Explicit instance IDs, role/grant checks, isolated ports/allowlisted roots, serialized mutations |
| File traversal/races | Descriptor-rooted operations, reject unsafe names and archive links/devices, bounded expansion |
| Data loss | Reviewed plans, verified restore points, stopped-runtime checks for mutations, commit barriers and retained recovery data |
| Third-party downloads | Permitted origins, public-address DNS checks at dial time, HTTPS redirect policy, artifact hashes where authoritative metadata supplies them |
| Host privilege | Unprivileged HTTP process; root-owned allowlisted helper, never arbitrary shell commands |
| RCON exposure | Local client plus host firewall isolation before server startup |
| Credential disclosure | Purpose-bound AES-GCM storage, protected master key, redacted API/logs; full backups remain sensitive |
| Supply-chain replacement | Official GitHub release identity, pinned Ed25519 manifest signature, SHA-256, architecture/version policy |
| Fork pull requests | Read-only checks, no publishing/signing secrets, no `pull_request_target` executing fork code |

## Accounts, sessions, and permissions

The `nec_session` cookie is HTTP-only, SameSite=Strict, valid for 12 hours, and Secure with an HTTPS public URL. The API returns the user and CSRF token, not the raw session token. Authenticated mutations require both a matching `Origin` and `X-CSRF-Token`; setup/login also require the matching origin. Browser access is same-origin, not a bearer-token administration API. Host-owned `public_url` cannot be changed through Settings or by supplying an extra request field; change protected host configuration and restart the panel.

Account security is self-service for **every role**, including viewers:

- `POST /api/v1/auth/password` accepts only `current_password` and `new_password`. It verifies the current password and requires a new password of **12–1024 UTF-8 bytes**, not characters. It does not reset another account or disable its existing TOTP requirement.
- `POST /api/v1/auth/totp` accepts `{}` to generate a new enrollment secret, then the current password, that secret, and a valid six-digit code to enroll. It cannot replace an existing enrollment without first disabling it with a valid current code.
- `DELETE /api/v1/auth/totp` requires the current password and a valid code from the existing authenticator. There is no target-user field, administrator override, or HTTP recovery-code bypass.
- Successful password changes, TOTP enrollment, and TOTP disablement revoke **all sessions for that account**, including the current browser session. Sign in again. These routes share a per-account limit of ten requests per 15-minute window, including secret generation; the limiter is in memory, not durable across panel restarts.

Only owners may list/create panel users or `DELETE /api/v1/users/{userID}`. Revocation deletes a non-owner account and cascades its sessions and instance grants. It refuses the current account and every owner account; it is not a temporary suspension or a way to reset someone else's TOTP. Previously accepted jobs are not cancelled by deleting an account or revoking sessions; inspect maintenance separately.

Roles are cumulative: owner > admin > operator > viewer. Owners/admins have global instance access; viewers/operators require explicit instance grants. Invisible instance/job lookups return 404. Ordinary operator lifecycle/console/moderation/backup actions do not grant file-manager access, backup download access, or update administration. Backup downloads, offsite fetches, restore previews, and restores require administrator access. Only owners may write outbound storage/notification configuration or check, stage, apply, roll back, postpone, or change policy for panel updates. Administrators may change backup retention but may not redirect outbound credentials.

## Backup and content trust

Backup archives can contain `server.properties`, RCON credentials, player data, and mod-private files. They are not client-side encrypted. Keep local backup roots and remote buckets private; use scoped provider credentials and independent storage retention/access policies. The encrypted settings store protects credentials, not the archive's contents.

Catalog `verified`, `local`, and `offsite` fields are recorded state, not fresh proof of available/correct bytes. Offsite upload verification checks remote size/hash metadata. A separate administrator fetch downloads and hashes actual bytes against the trusted database record, then verifies the archive/manifest before local use. Losing the trusted record is not repaired by accepting a hash solely from remote user metadata. The archive download alone does not include the catalog or `.instance.json` sidecars.

Restore preview and the restore job require valid `BACKUP_ID.instance.json`. Before stopping Minecraft or replacing data, the job verifies the archive and rejects absent/malformed metadata, a different instance ID, root, game/RCON ports, or adoption mode, or unsafe configuration such as an unapproved Java path, unsafe launcher, or invalid memory limit. If archived Java/launcher/memory settings differ, the restricted helper reconfigures the existing service after the data swap. A later helper/database error is reported with available recovery details, not silently converted into a world rollback. The instance remains stopped for review. See [backups and restores](backups-and-restores.md).

Mod search requires an exact selected Minecraft version and filters Fabric server candidates; discovery is not installation approval. Identified external mods are still unmanaged until an administrator explicitly adopts their exact official hash/compatible metadata under a stopped-runtime job and verified backup. Unknown files remain protected. `.mrpack` preview/apply reconstructs exact server mod artifacts and supported config overrides, not arbitrary launchers/world files or an implicit runtime migration. Content/runtime readiness recovery never implicitly restores world progress.

## Residual risks

Executing a downloaded mod/JAR remains code execution as the Minecraft user. Root, the deployment administrator, a compromised allowed Java binary, or an attacker able to rewrite trusted panel storage can bypass panel guarantees. Local backup/catalog checks and the separate instance sidecar are not authenticated signatures against that attacker. Files outside selected backup scope and externally changing data may not be recoverable. Online backups coordinate Minecraft saves but are not general transactional snapshots of every mod's private storage.

Disk exhaustion, host crashes, failed filesystem sync, interrupted swaps, and migrations need deployment-specific recovery checks. Marking a job interrupted does not automatically reconcile every retained backup journal; preserve the original/recovery trees and inspect them before retrying. Lost authenticator/key material has no browser administrator override; follow protected [disaster recovery](disaster-recovery.md) procedures rather than weakening authentication.

A successful unit test is not evidence of Linux firewall, systemd, real-world Minecraft, Proxmox, or offsite-provider acceptance. Consult [testing](testing.md) for actual evidence and gaps. Describing authenticated release verification does not assert that a release was published or that a live update succeeded.
