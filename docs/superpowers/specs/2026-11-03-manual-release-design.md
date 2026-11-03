# Local distribution and explicitly authorized release

## Intent and approved approach

Extend the existing Makefile, consolidate the remaining development branches,
and publish an authenticated Linux release for installation. Reuse the existing
Go packager, SBOM generator, signer and publish-last/read-back verifier. Do not
replace them with a local unsigned publisher or extract GitHub secrets.

`make dist VERSION=<version> OUT=<new-directory>` builds both Linux architectures
with the embedded frontend and a CycloneDX SBOM. `make release-plan` is read-only.
Local packages remain unsigned candidates, not official updater assets.

The remaining TypeScript 7 and React-plugin 6 branches need the existing Vite
client declarations and a compatible Vite 8 dependency. Resolve these narrowly,
without disabling type checking or forcing incompatible peer dependencies.

## Authorized publication boundary

The owner approved one manual GitHub Actions release run, possible runner costs,
and temporary approval exceptions only for consolidated PR #7 and this run's
`release` environment. Restore the original approval policies immediately after
the corresponding operation; disable repository Actions when the run ends,
including on failure. Automatic CI and push-triggered publication remain off.

Use `.github/workflows/release.yml` with only `workflow_dispatch`, protected
official `main`, full-SHA action pins, isolated read-only preparation and a
protected signing/publishing job. The production signing key is available only
to its signing step. Retain canonical metadata, both architectures, signatures,
GitHub OIDC provenance, non-overwrite behavior, draft read-back, exact tag/source
identity and stable-release discovery checks. A failure does not authorize a
second run, key rotation, unsigned fallback or fabricated workflow context.

## Safety and acceptance

All edits stay sequentially on the existing `dev` checkout. Preserve existing
workspaces and owner changes. Run local `make check`, `make security`, workflow
lint, deployment tests and both Linux package builds before pushing or merging.
Review the complete branch read-only before the remote transaction.

Do not restart Minecraft or the retained VM, expand runtime network access, or
claim completed real-host lifecycle/update acceptance. Release notes must state
the unresolved readiness discrepancy and unrun installation, restore and update
drills. A published release is confirmed only after the actual assets, signature,
provenance, source tag and discovery are verified; otherwise report the blocker.
