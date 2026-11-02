# Administration interface

The `web/` package is a React/TypeScript/Vite application, compiled into the Go binary with `go:embed all:dist`. Node.js is needed to build it, not to run the installed panel. All operational data comes from the same-origin `/api/v1` API. Test fixtures are not part of the application bundle.

## Build and checks

From `web/`:

```sh
npm ci
npm run typecheck
npm test
npm run build
npm audit
```

Then run `go test ./web` from the repository root. `dist/` and `node_modules/` are ignored; build the frontend before compiling or testing Go. `web.FS()` exposes the built files with `index.html` at the filesystem root. The HTTP integration owner serves this filesystem; client navigation uses hashes and needs no server-side route rewriting.

For development, `npm run dev` listens on loopback and proxies `/api` to `http://127.0.0.1:8080`. Use the Vite origin consistently so session cookies and Origin validation stay same-origin. Do not expose the development server publicly.

## Workflows

- Setup reads the server's public setup-required flag. The protected host token is entered by the owner, never fetched from an HTTP endpoint. Login supports an optional TOTP code. Session bootstrap obtains the CSRF token; passwords and tokens are not persisted in browser storage.
- Add instance offers Fabric creation, browser archive/folder import, and allowlisted local adoption. Import and adoption require a server-generated inspection hash and a detected launcher. All three return a queued job, not an instantly completed instance. Follow the job and open its resulting instance.
- Dashboard shows observed lifecycle readiness, current status, recovery coverage, and real job history. A process is not treated as a ready server. Console commands are serialized jobs; output is shown from the completed job result.
- Files supports browsing, authenticated downloads/ZIP exports, bounded UTF-8 previews, etag-protected text editing, folder creation, moves, recovery retention, and imports with conflict previews. Secret or binary previews fail visibly. A queued edit does not say that the file is already saved.
- Worlds offers explicit activation and preparation of empty world directories. Minecraft generates new world data on its next start. Imports use the Files workflow.
- Mods provides hash inventory, external-content preservation, Modrinth search, reviewed dependency/update plans, plan-hash application, pin/unpin, disable/remove, and an explicit maintenance-window policy. The admin-only Server modpack section accepts `.mrpack` files through the same resumable upload workflow, displays the server preview, and requires confirmation before applying its exact plan hash. Export downloads an authenticated `.mrpack`; the server rejects unmanaged or altered JARs. Fabric mods are never labeled plugins.
- Backups provides default-offline or coordinated-online creation, verification jobs, authenticated downloads, and administrator-confirmed restore previews. The stored integrity badge says “Verified when created,” not that the current archive bytes have just been checked. Use Verify to queue a current check; verification is not described as a successful restore test.
- Players exposes reported online/access lists and supported Minecraft player actions. No invented UUIDs, playtime, skins, or history are shown. Operator grants and IP actions require admin. The admin-only `op_level` action sets level 1–4 for an existing operator while the server is already stopped; it does not grant operator. Grant/deop remain RCON-ready operations.
- Automation creates/deletes timezone-aware backup/restart/mod-check schedules. Jobs shows durable progress, step history, failure diagnostics, recovery results, and cancellation only when the API marks it cancellable.
- Settings covers safe properties, runtime plans, public URL/retention, owner-only explicit replacement of encrypted R2/S3 and notification credentials, owner-created users with instance grants, audit history, and personal TOTP enrollment.
- Application updates checks/stages/applies/rolls back panel releases through jobs, and saves a separate update policy. The panel must not restart Minecraft or change world data. The UI never fabricates verification when a signing key or helper is unavailable.

## Uploads and limits

`tus-js-client` uses 8 MiB chunks, saved upload fingerprints, retry/backoff, pause/resume, and authenticated cancellation. Every tus request receives the current CSRF token only after its destination is validated as the panel's own upload endpoint. No external resumable URL receives this token.

Browser-selected directories preserve nested paths and remove only their common selected root. They are packaged as deterministic, uncompressed ZIPs with `fflate` before upload. Browser packaging is limited to 512 MiB and 20,000 files; empty directories are omitted. Create a ZIP or tar.gz archive locally for larger folders and upload that archive directly through tus. Server upload quotas and inspection limits still apply. Upload completion means **staged**, not installed.

## API integration

The normative wire schemas are in `docs/integration-contract.md`, maintained by the integration owner. `web/src/api.ts` centralizes their TypeScript counterparts. Arrays must be `[]`, never null; errors must be `{error:string}` with a failing HTTP status. Accepted operations return a `Job`; `GET /jobs/{id}` returns `{job,steps}`. Numeric timestamps are Unix seconds, while instance creation and log timestamps use RFC3339.

The UI uses `/setup`, `/auth/login`, `/auth/logout`, `/auth/session`, `/auth/totp`, `/instances`, instance status/log/console/action/file/import/recovery/world/property/mod/backup/runtime/player routes, `/imports/preview`, `/adoptions` and its preview, `/fabric/versions`, `/java`, `/jobs`, `/schedules`, `/settings`, `/users`, `/audit`, `/updates` and its action/policy routes, plus `/events` SSE invalidation. Server modpacks use `POST /instances/{id}/modpacks/preview` with `{upload_id}`, `POST /instances/{id}/modpacks/apply` with `{upload_id,plan_hash,confirm:true}`, and authenticated `GET /instances/{id}/modpacks/export`. Files use `preview=true` for JSON text responses and include `etag` on writes. Import/runtime/mod applications send the exact reviewed hash. Fabric installer selection defaults to the first reported stable installer, never a hard-coded version. Storage/notification enable-only saves omit credential keys, preserving encrypted values; replacement forms send explicitly entered credentials.

Permissions in the UI are convenience only; the API remains the security boundary. Owners/admins manage instances. Operators have lifecycle/console/player/backup access only to server-authorized instance records; viewers are read-only. File/content/runtime/world/automation/update changes require admin, and users/credentials require owner. Sessions do not implicitly grant access to every instance.

## Design and verification boundary

The approved code-led survey-sheet system is recorded in `DESIGN.md`, `.impeccable/design.json`, and `.impeccable/surfaces/admin.md`. Dense desktop tables become contained horizontal-scroll regions on mobile. Forms use native labels, visible focus, reduced-motion support, and protected destructive confirmations.

Frontend tests use explicit HTTP fixtures to check failure and permission paths. They are not evidence that host services, third-party integrations, or Linux update recovery work. The parent integration session owns live API end-to-end checks, desktop/mobile screenshots, independent finish review, complete repository checks, and release publication. No release or whole-system completion is claimed by this UI branch.
