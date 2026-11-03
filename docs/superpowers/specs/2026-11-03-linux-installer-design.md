# Generic Linux installation and first-owner access

## Intent

Provide a reusable one-line `curl` installation command in the README. Install
the existing native systemd service and show local access information and the
code used to create the first owner. Nothing is specific to the owner's server.

## Approach

Reuse `deploy/install.py`, its pinned Ed25519 verification and existing service
units. Add a small bootstrap that stages deployment files from one reviewed,
immutable source revision, verifies their SHA-256 hashes, then calls that
installer. The README command verifies the bootstrap before executing it.

Executing a mutable `curl | sh` response is rejected: the existing `install.sh`
also needs companion files and cannot run correctly from a pipe. Building the
application on the destination is rejected: a native installation should not
need Go or Node.js.

## Required behavior

- Require the documented Linux/systemd prerequisites, an explicitly selected
  stable release and an administrator-approved Java executable. Do not install
  or hard-code a Java, Minecraft or Fabric version.
- Retain the existing dedicated panel account, systemd hardening, preservation
  of existing configuration and refusal to replace a different installation.
- Show the configured dashboard URL, the default localhost address and detected
  private interface addresses. Clearly distinguish host addresses from actual
  dashboard listeners; do not enable non-loopback HTTP or change firewall rules.
- On an interactive terminal, show the existing protected `setup-token` after a
  bounded startup wait. Never generate a replacement token, query credentials
  over HTTP or put its value in redirected output, logs or documentation.
- Without an interactive terminal, or when the token is unavailable, show a
  local administrator command to read it. Do not claim that an owner already
  exists merely because the file is missing.
- Reject unsafe token files and invalid network addresses. Missing address
  discovery must not undo an otherwise completed installation.
- Download only from fixed official HTTPS origins, reject private download
  destinations and redirects, bound downloads and verify all deployment files
  before executing any of them.

## Boundaries and delivery

Work sequentially on the existing `dev` checkout. No new dependencies, API
changes, workspaces, Actions, public proxy provisioning or Minecraft starts.
Run local tests and builds before pushing. The installer remains unavailable
for real release installation until an official signed release is published;
the existing publication blocker must remain explicit. A new merge needs its
own review or authorization; the exception used for PR #1 is not reusable.
