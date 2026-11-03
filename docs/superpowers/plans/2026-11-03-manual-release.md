# Local distribution and manual release implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan sequentially. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Consolidate the code, simplify local distribution and attempt the one explicitly authorized authenticated release.

**Architecture:** Extend existing build/release tooling and restore only a manual protected-main release workflow. Keep local validation mandatory and restore remote controls after their authorized temporary exceptions.

**Tech Stack:** Go, GNU Make, Python standard-library unittest, React/TypeScript/Vite, GitHub Actions and GitHub CLI.

**Spec:** `docs/superpowers/specs/2026-11-03-manual-release-design.md`

## Global Constraints

- Existing `dev` checkout only; sequential edits, no new branches or worktrees.
- One manual release run only; no automatic CI/publication, private-key extraction or trust weakening.
- Restore approval controls immediately and disable Actions after success or failure.
- Full local gates and both Linux package builds before any push.
- No Minecraft/VM starts or claims of complete real-host acceptance.

## Review Focus

- Missing version or existing output must fail without overwriting distribution files.
- Remaining dependency branches must typecheck and satisfy peers without suppression.
- Forks, non-main refs and unprotected/stale source must not access signing/publication.
- Secrets must stay confined to the signing step; provenance must cover the exact uploaded payloads.
- Failed publication must leave an unpublished diagnostic draft and restored external controls.

### Task 1: Consolidate compatible frontend toolchain updates

**Files:** `web/package.json`, `web/package-lock.json`, `web/tsconfig.json`.
**Interfaces:** Existing npm `typecheck`, `test` and `build` scripts remain unchanged.

- [x] Merge the two remaining remote dependency commits on `dev`, preserving owner changes.
- [x] Observe TypeScript's missing CSS declaration and the plugin/Vite peer failure before fixes.
- [x] Add `vite/client` types and a registry-verified compatible Vite 8 version; regenerate the lock without forced peer resolution.
- [x] Run typecheck, all frontend tests and production build; commit the integrated dependency changes.

### Task 2: Add local distro and manual-only publication controls

**Files:** `Makefile`, `.github/workflows/release.yml`, `scripts/release_test.py`, `scripts/release.py`, `AGENTS.md`, README and development/release/testing/acceptance documentation, `CHANGELOG.md`.
**Interfaces:** `make dist VERSION=<version> OUT=<new-directory>` calls existing Go build and Python SBOM modes; `make release-plan` calls the read-only planner. Workflow uses the existing guarded planner/signer/publisher without changing its trust identity.

- [x] Add failing tests for missing-version refusal, distro command composition, manual-only workflow permissions/secret confinement and truthful release-note limitations.
- [x] Implement the minimal Makefile targets and adapt the reviewed historical workflow without its automatic triggers or reusable CI job.
- [x] Include known acceptance limitations in generated English release notes and update policy/docs for the explicitly authorized manual exception.
- [x] Run script/deployment tests and pinned actionlint; commit only the owned changes.

Task 2 verification: isolated script tests 26/26, isolated root deployment tests 38/38, pinned actionlint v1.7.12, whitespace checks and 49 relative documentation links passed. No remote execution or publication has occurred.

### Task 3: Verify and conduct the bounded release transaction

**Files:** Plan/evidence documentation and existing remote PR/protection/environment/release records; no new publisher.
**Interfaces:** Exact tested `dev` tree -> PR #7 squash -> protected `main` SHA -> one manual workflow run -> verified official release or explicit failure.

- [ ] Obtain one fresh read-only whole-branch review; fix important findings test-first.
- [ ] Pass local `make check security`, isolated deployment tests, workflow lint and clean-source `make dist`; inspect ELF/UI/SBOM/source identities.
- [ ] Push `dev`, update PR #7 without overwriting concurrent edits, snapshot controls and merge only the tested head using the authorized temporary review exception; restore reviews in `finally`.
- [ ] Verify the merged tree and local main-identity build, enable Actions only for the single dispatch, approve only this run with the authorized temporary environment exception, restore it immediately and disable Actions in `finally`.
- [ ] Verify published assets/signature/provenance/tag/discovery or record the exact failure; report remaining real-host acceptance gaps and do not retry without authorization.
