# Integration contract

The integration owner maintains migrations, HTTP handlers, CLI, release automation, and AGENTS.md. Domain packages do not import `internal/api` or own database migrations.

- Module: `github.com/gabrielpgava/newempire-server-control`; Go 1.27 (verified from go.dev at implementation time).
- `internal/model`: shared Instance, RuntimeStatus, LogLine and random 32-character hexadecimal IDs.
- All API paths start `/api/v1`. JSON uses snake_case. Lists are JSON arrays, errors are `{ "error": "English explanation" }`. Accepted jobs return a job object with ID.
- Sessions are same-origin HTTP-only cookies. Mutations require `X-CSRF-Token` from `/auth/session`. Authentication/setup endpoints enforce Origin separately.
- `GET /setup` returns `{required:boolean}`; `POST /setup` accepts token, username, password. `/auth/login`, `/auth/logout`, `/auth/session` (user and csrf_token).
- `/instances`: GET list, POST creation; `/instances/{id}` GET. POST `/instances/{id}/actions/{start|stop|restart|backup}` returns job. GET `/status`, `/logs`, POST `/console` with `{command}`.
- GET `/instances/{id}/files?path=` list; GET `/file?path=` download/text; POST `/files` with `{operation,path,destination?,content?,confirm?:true}` returns job. Operations mkdir, write, move, trash. GET `/export?path=` streams ZIP.
- tus upload routes `/uploads/` (POST, HEAD, PATCH, DELETE). Upload metadata must include filename. POST `/instances/{id}/imports/preview` `{upload_id}`; POST `/imports/apply` `{upload_id,confirm:true}` returns job. Adoption/creation use explicit setup form.
- GET `/instances/{id}/backups`; POST `/backups` `{note}`; GET `/backups/{backupID}/download`; POST `/backups/{backupID}/verify`; POST `/backups/{backupID}/restore` `{confirm:true}`.
- GET `/instances/{id}/properties`, PUT same with `{properties:Record<string,string>,confirm:true}`; GET `/worlds`; POST `/worlds` `{operation,name,confirm:true}`.
- GET `/instances/{id}/players`; POST `/players` `{action,player?,value?}`.
- GET `/instances/{id}/mods`; GET `/mods/search?q=`; POST `/instances/{id}/mods/plan` `{projects?:string[]}`; POST `/mods/apply` `{projects?:string[],confirm:true}`; PUT `/mods/policy` `{mode,interval_hours,window_start,window_end,timezone,postpone_players}`.
- GET `/fabric/versions`; GET `/java`; POST `/instances/{id}/runtime/plan` and `/runtime/apply` explicit versions and confirmation.
- GET `/jobs`, `/jobs/{id}`; POST `/jobs/{id}/cancel`; GET `/events` authenticated SSE invalidation stream.
- GET/POST `/schedules`; DELETE `/schedules/{id}`. Schedule fields instance_id, kind (backup/restart/mod_check), hour, minute, weekdays (0–6), timezone, enabled.
- GET `/audit`; GET/POST `/users` (owner only); GET/PUT `/settings`; GET `/updates`; POST `/updates/{check|stage|apply|rollback}`; GET `/healthz` is minimal public liveness, `/api/v1/health` authenticated.

All filesystem mutations and lifecycle changes run through durable jobs and exclusive instance locks. Domain operations return errors; the API never claims successful mutation on a failed operation. Unknown/external content is preserved. No world rollback is implicit in a content update.
