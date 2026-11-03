# Local Release and Installer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provide a short latest-stable Linux installer and prepare/publish signed native releases entirely locally.

**Architecture:** Reuse the Python bootstrap/installer, local Go cross-builder and signer, and fail-closed Python draft publisher. Add explicitly local source-bound signed provenance without impersonating GitHub attestations or changing the updater manifest.

**Tech Stack:** Bash, Python standard library, Go, React/TypeScript, local Docker sandbox, local GitHub CLI.

**Spec:** `docs/superpowers/specs/2026-11-03-local-release-installer-design.md`

## Global Constraints

- Work sequentially on `dev` in the existing checkout; no additional checkout, worktree, branch, or concurrent editing agent.
- English-only code, errors, documentation, and release notes; no new dependency.
- Supported deployment targets remain Linux/systemd amd64 and arm64.
- No workflow or cloud runner runs. Preserve existing workflows and GitHub settings.
- Only the owner's explicit initial trust-root replacement is authorized; no unsigned publication, fake Actions context, or extraction of cloud secrets.
- Keep unsigned candidates and report any signing or protected-main integration blocker.

## Review Focus

- A pipe has no reusable script filename/stdin: sudo must receive the downloaded script and preserve terminal output without executing a partial transfer.
- Mutable latest may change during installation: fix a validated stable version before fetching signed assets.
- Java is absent, symlinked, or untrusted: resolve existing executables and retain root-ownership/path checks; never silently install a JRE.
- Local files or remote draft bytes are corrupt: reject before publication and never overwrite another asset.
- A local signer is unavailable or remote main moved: retain candidates and stop before official publication, without weakening verification.

### Task 1: Short installer

**Files:** Create `install.sh`; modify `deploy/install.py`, `deploy/test_bootstrap.py`, `deploy/test_deploy.py`, and subsequently refresh `deploy/bootstrap.py` source/hash pins.

**Interfaces:** `deploy/install.py` keeps explicit `--version`, repeated `--java`, and offline input flags; omitted version selects latest stable, omitted Java discovers an installed trusted runtime. `install.sh` fetches a complete bootstrap into private temporary storage before executing it.

- [x] Add behavioral tests for pipe execution/download failure/sudo, installed Java discovery and absence, stable latest selection and malformed/prerelease versions.
- [x] Run changed tests in the local sandbox and observe missing-behavior failures.
- [x] Implement the wrapper and minimal installer defaults, retaining all existing safety checks.
- [x] Run all deployment tests; commit with a `feat:` Conventional Commit and refresh immutable bootstrap pins against the committed deployment sources.

### Task 2: Local signing and publication

**Files:** Modify `scripts/release/main.go`, `scripts/release/main_test.go`, `scripts/release.py`, and `scripts/release_test.py`; synchronize the owner-selected public key across deployment files and the CLI, then refresh immutable bootstrap pins.

**Interfaces:** Preserve existing cloud-only commands for the unused workflow; add explicit local plan/publication and local signing/verification modes. Signed local provenance binds repository, full source commit, version, and all existing payloads including `checksums.txt`.

- [x] Add tests rejecting a fork/dirty/non-main/moved/unprotected local source, invalid local provenance, corrupt payloads, and mismatched retries before publication.
- [x] Observe these tests fail in the local sandbox.
- [x] Implement local guards and signed provenance using the pinned key and existing manifest verification; keep draft/read-back and latest checks.
- [x] Run all release tests, including legacy workflow safety tests; commit the verified changes.

### Task 3: Documentation, gates, and native candidates

**Files:** Modify `README.md`, `docs/install/linux-systemd.md`, `docs/releases.md`, `AGENTS.md`, `CHANGELOG.md`, and other current guidance only where contradictory.

**Interfaces:** Document `curl -fsSL https://raw.githubusercontent.com/gabrielpgava/newempire-server-control/main/install.sh | bash`; retain advanced/offline instructions and initial-script trust warning. Publication consumes the exact clean protected-main source and a locally available official key.

- [x] Update English documentation and local-only engineering policy; record outstanding real-host acceptance.
- [x] Run local `make check`, `make security`, frontend build/tests, deployment/release tests, migration/race tests, and release dry-run where sandbox/dependencies permit; record exact failures or blockers.
- [x] Commit and obtain a fresh read-only whole-change review; fix important findings with RED→GREEN evidence.
- [x] Compute the conventional version and build Linux amd64/arm64 candidates with embedded UI from clean sources; verify archive/SBOM/metadata.
- [x] If credentials and protected-main integration are available/authorized, sign, verify, publish locally, read back assets, and confirm stable latest/updater discovery. Otherwise preserve candidates and report exact remaining work without claiming delivery.

**Conditional outcome:** preparation passed; official release delivery remains blocked by required independent/code-owner/latest-push approval on protected main. Unsigned `v0.1.0` candidates and ephemeral-key dry-run evidence are preserved. See `docs/acceptance.md`; no protection was bypassed and no official signing/publication occurred.
