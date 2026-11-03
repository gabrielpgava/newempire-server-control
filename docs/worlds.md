# Worlds and server properties

World controls select directories; they do not manufacture valid Minecraft saves or prove that a save is compatible with a different Minecraft/mod version.

## Inventory and activation

`GET /api/v1/instances/{instanceID}/worlds` returns:

```json
{"active":"world","worlds":[{"name":"world","path":"world","active":true,"dimensions":[]}]}
```

Dimensions are reported from detected data. An empty list is not a promise that Minecraft has no other dimensions. Custom mod data and unknown files must be preserved.

Admins/owners can prepare a new world directory:

```http
POST /api/v1/instances/{instanceID}/worlds
Content-Type: application/json
```

```json
{"operation":"create","name":"new-world","confirm":true}
```

Use a single directory name of 1–64 characters: start with a letter/number, then use letters, numbers, `_`, `.`, or `-`. Never use an absolute path or traversal. The operation prepares an empty directory. **Minecraft generates the world on a subsequent start.** It does not replace an existing world or claim an empty directory is a valid generated save.

To change the selected world, use `{"operation":"activate","name":"new-world","confirm":true}` at the same endpoint. Review the selected name and the job's safety checks. Stop Minecraft before world/property edits; maintenance jobs may reject a running instance rather than stop it automatically. They must not bypass backup, instance locking, or stopped-runtime requirements. Existing worlds remain separate. Check the job result and runtime status before starting or resuming play.

## Import and export

Use **Files → Import** to stage a world archive or browser-zipped folder. Complete the upload, inspect its paths/conflicts, and apply the exact `preview_hash` with explicit confirmation. Upload completion alone does not install a world. Place the world under the intended relative directory and select it through Worlds after the import succeeds.

`GET /api/v1/instances/{instanceID}/export?path=world` exports a directory as ZIP for an administrator. A general export is not automatically a coordinated online backup; use the backup workflow when consistency matters. Keep a verified backup outside the instance before importing, activating, downgrading, or migrating valuable world data.

Deleting a path through Files moves it into external file recovery. `GET /api/v1/instances/{instanceID}/recovery` lists `{token,path,created_at}` entries; recover through `POST /files` with `{"operation":"recover","token":"RETAINED_TOKEN","confirm":true}`. Recovery must not overwrite an existing destination. File recovery is not a substitute for a verified whole-instance backup.

## Properties

`GET /api/v1/instances/{instanceID}/properties` returns:

```json
{"properties":{},"restart_required":true}
```

Property values are strings, including booleans and numbers. For a supported safe key, an admin can send:

```http
PUT /api/v1/instances/{instanceID}/properties
Content-Type: application/json
```

```json
{"properties":{"motd":"Our Minecraft server"},"confirm":true}
```

The API rejects unsupported/sensitive keys; it is not an arbitrary configuration-file writer. RCON passwords are not returned. Unknown keys/comments in the underlying document are preserved rather than silently reset. Treat `restart_required` and job completion separately from runtime readiness. The panel's response is not evidence that Minecraft has already loaded a changed property.

Supported edits are `difficulty`, `gamemode`, `pvp`, `white-list`, `enforce-whitelist`, `view-distance`, `simulation-distance`, `max-players`, `level-seed`, `online-mode`, `motd`, `allow-flight`, `spawn-protection`, `enable-command-block`, `generate-structures`, `level-type`, `max-tick-time`, and `op-permission-level`. `level-name` is readable but changed through Worlds, not arbitrary property edits. Changing a seed cannot regenerate an existing save and is rejected when that active world already has `level.dat`.

## Recovery rules

- Do not copy or edit a running world's files manually while a maintenance job is pending or running.
- If a migration or content update fails, inspect the retained restore point and diagnostics. **World rollback is never an implicit content rollback.** Restoring world data is a separate, administrator-confirmed choice.
- Restore previews show affected data; verify the backup before confirming a restore and preserve the failed state for diagnosis where practical.
- World generation, custom dimensions, real Minecraft compatibility, and restart behavior still require the [acceptance drills](acceptance.md). No conversion, pruning, chunk editing, map rendering, or automatic downgrade support is claimed.
