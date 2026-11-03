# Generic Linux Installer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Document a verified one-line Linux installation and report dashboard addresses and the first-owner code safely.

**Architecture:** Extend the existing Python installer only at its post-install reporting boundary. A small pinned-source bootstrap stages its existing deployment dependencies before invoking it; native release verification remains unchanged.

**Tech Stack:** POSIX shell, Python standard library, existing curl/OpenSSL/systemd tooling, existing Go and React build gates.

**Spec:** `docs/superpowers/specs/2026-11-03-linux-installer-design.md`

## Global Constraints

- All edits and commits stay on `dev` in the existing checkout; preserve other branches and worktrees.
- Keep the production public key, explicit stable-version selection and approved Java-path policy unchanged.
- No new dependencies, Actions, unverified installation fallback, network exposure or Minecraft execution.
- All repository copy is English; no real setup token is retained in output artifacts or tests.
- No official release or production-readiness claim without corresponding evidence.

## Review Focus

- Redirected output must never contain a setup code; test before opening the token file.
- Missing/delayed tokens must not cause regeneration or a false already-configured claim.
- Symlinked, oversized or insufficiently protected tokens must not be displayed.
- Private host addresses must not be presented as reachable HTTP listeners; preserve custom public URLs.
- Tampered, redirected or private-destination deployment downloads must fail before installer execution.

---

### Task 1: Report local access and first-owner setup safely

**Files:** Modify `deploy/install.py`, `deploy/test_deploy.py`, `docs/install/linux-systemd.md`, `CHANGELOG.md`.

**Interfaces:** Add `local_addresses()` returning validated private address strings and `print_installation_access()` called after the existing successful `install(...)`. Neither changes panel state or authentication behavior.

- [x] Write tests for private IPv4/IPv6 discovery, invalid/public address rejection and unavailable discovery, preserving the configured dashboard URL.
- [x] Write tests for interactive setup-code display, redirected-output suppression, delayed/missing tokens, unsafe token files and bounded waiting; use dummy tokens and mocked host/service discovery only.
- [x] Run `python3 -B -m unittest discover -s deploy -p 'test_*.py'`; confirm the new behavior tests fail before implementation.
- [x] Implement the two reporting helpers using bounded subprocess/file operations and existing filesystem safety patterns. Display host addresses as informational only; keep the loopback binding unchanged.
- [x] Re-run deployment tests, inspect output for accidental secret disclosure, and update installation/bootstrap-token documentation and changelog.
- [ ] Commit the tested installer changes on `dev`; use this immutable commit as the deployment source for Task 2.

### Task 2: Add the verified one-line bootstrap and README entry

**Files:** Create `deploy/bootstrap.py`, `deploy/test_bootstrap.py`; modify `README.md`, `docs/install/linux-systemd.md`, `CHANGELOG.md`.

**Interfaces:** `stage_deployment(stage)` downloads the fixed required deployment files using a full `SOURCE_COMMIT` and expected SHA-256 map. `main()` runs the existing `install.py` with the caller's explicit `--version` and `--java` arguments only after every file verifies.

- [ ] Write failing tests for the complete verified stage, tampered/truncated/oversized files, HTTP redirects/errors, private DNS results and no installer execution after any failed check.
- [ ] Implement the bounded fixed-origin HTTPS bootstrap with private temporary staging and standard-library cleanup; preserve the installer's existing signature/key checks.
- [ ] Pin the nine required installer/verifier/key/wrapper/unit/sudoers files to Task 1's commit and verify their hashes against that source; do not download a replacement trust root from release assets.
- [ ] Add a single physical shell line in the README: stage the bootstrap, verify its documented SHA-256, then invoke it with an explicit stable version and approved Java path. Explain prerequisites, the localhost/private-IP distinction and interactive-only setup-code display.
- [ ] Add a regression check matching the README bootstrap checksum to `deploy/bootstrap.py`; document when source pins must be refreshed.
- [ ] Run the deployment/bootstrap tests and inspect that failures never execute installer code or print dummy secrets to captured output.

### Task 3: Verify and propose integration

**Files:** Update the two checklist documents with actual results only; no application/API changes.

- [ ] Run `make check` and `make security` with the verified local toolchain; record exact revision, warnings and skips.
- [ ] Build Linux amd64 and arm64 candidates locally with the embedded UI and unchanged production public key before any push; do not publish unsigned candidates.
- [ ] Review the complete diff, validate README command syntax and relative links, and confirm `dev`, clean staging scope and unchanged disabled Actions policy.
- [ ] Push `dev` and propose a conventional feature PR to `main`; do not reuse PR #1's protection exception or bypass mandatory review.
- [ ] Report implementation, executed checks, unrun real-host installation and the exact signed-publication blocker. Do not restart the retained VM or publish a release.
