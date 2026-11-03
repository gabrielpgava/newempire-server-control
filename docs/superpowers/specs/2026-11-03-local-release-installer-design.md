# Local releases and short Linux installer

## Intent and owner decisions

The owner requested a short GitHub-hosted `curl | bash` installation command and
an entirely local build, verification, signing, and GitHub Release publication.
GitHub stores source and completed artifacts; no workflow or cloud runner runs.
The owner's `execute` instruction approves this approach and inline execution.
The owner subsequently supplied a local signing directory and explicitly
authorized replacing the official public key with its matching public key.
Only the public key is committed; the private key and generation utility stay
outside the repository. Existing old-key installations require manual migration.

## Approaches

1. Keep the checksum-heavy, explicitly versioned command: rejected by the owner
   because it is too long and requires unnecessary installation arguments.
2. Reuse the verified Python deployment behind a small root `install.sh`, detect
   existing Java, and select the latest stable signed release: selected. This
   preserves host/data safeguards while accepting trust in the initial mutable
   GitHub script. It does not authenticate that initial script independently.
3. Build/publish through the existing manual workflow: explicitly rejected.
   Reuse the local Go release builder and draft/read-back publisher instead.

## Boundaries

- Work sequentially on `dev` in the existing checkout; no additional checkout,
  worktree, branch, or concurrent editing agent. Preserve existing workflows and
  GitHub settings, and inspect triggers before any remote mutation.
- English-only code, errors, documentation, and release notes; no new dependency.
- Supported deployment targets remain Linux/systemd amd64 and arm64, not every
  platform supported by Go. Final native archives include the React frontend.
- The short installer requests sudo, checks prerequisites, detects an installed
  trusted Java executable, and reuses the existing installer. Explicit versions,
  Java paths, and offline archives remain available. It never installs Java,
  accepts the Minecraft EULA, starts instances, or exposes a public listener.
- Select latest through bounded official GitHub metadata; validate stable
  SemVer, then fetch a fixed version. Verify the pinned Ed25519 signature,
   architecture, and checksum before host changes. The owner-authorized initial
   trust root has SHA256 d7ab48a9fa4414e04027255f140f6677a451e868470e14ef3c64770420d81a7f.
- Keep deployment companions authenticated by fixed source SHA and file hashes.
  The new initial script remains a mutable trust boundary documented honestly.
- Local publication retains exact clean protected-main identity, official remote
  identity, version policy, no overwrite, explicit draft retry, bounded asset
  read-back, signature/hash verification, tag identity, and latest discovery.
- Local provenance must be labeled local, bind the exact source SHA and every
   payload including checksums, and be signed with the owner-selected pinned key. It
  must not pretend to be GitHub OIDC provenance. Preserve the unused workflow.
- Missing local signing credentials or protected-main integration blocks official
  publication. Keep unsigned candidates and report the blocker; never extract a
   cloud secret, forge context, make an unauthorized key change, or publish unsigned artifacts.

## Verification and non-goals

Use behavioral tests for pipe execution, sudo, failed downloads, Java discovery,
latest-version validation, local guard/provenance, and corrupt/retried drafts.
Run local repository gates and cross-build both Linux architectures in bounded,
network-isolated containers with available dependencies. Report missing tools,
dependencies, advisories, and unrun real-systemd/Minecraft acceptance accurately.
No application API, world-data behavior, licensing, or runtime provider changes.
