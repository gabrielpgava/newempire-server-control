# Players and access control

Player controls manage Minecraft access, not panel accounts. Panel users and instance grants are owner-managed through `/api/v1/users`.

## Read player state

Select an instance and open **Players**. `GET /api/v1/instances/{instanceID}/players` returns:

```json
{
  "online": [],
  "whitelist": [],
  "operators": [],
  "bans": []
}
```

`online` is the last observed player-name list, not a historical census. Access-list records come from Minecraft's files and are not guaranteed to contain enrichment such as playtime, skins, UUID lookups, or last-seen timestamps. Whitelist membership does not imply an active connection. `ip_bans` is included only for administrator-level callers; treat IP addresses as sensitive.

## Permissions

- **Viewer:** read permitted instances and their non-IP player records.
- **Operator:** on explicitly granted instances, whitelist add/remove, kick, ban/pardon players, and broadcast messages.
- **Admin/owner:** the above plus grant/revoke Minecraft OP, set an existing player's OP level, and manage IP bans.

An instance outside a user's grants is not disclosed. Hiding a button is not authorization; the API enforces the role and instance checks.

## Queue an action

Player actions are jobs. For example, send this authenticated, CSRF-protected request to `POST /api/v1/instances/{instanceID}/players`:

```json
{"action":"whitelist_add","player":"ExamplePlayer"}
```

| Action | Target and value | Minimum role |
| --- | --- | --- |
| `whitelist_add`, `whitelist_remove` | `player`: Minecraft name | Operator |
| `kick`, `ban` | `player`: name; optional `value`: reason | Operator |
| `pardon` | `player`: name | Operator |
| `say` | `value`: message; no player required | Operator |
| `op`, `deop` | `player`: name | Admin |
| `op_level` | `player`: existing OP name; `value`: `"1"`–`"4"` | Admin |
| `ban_ip` | `player`: IP address; optional `value`: reason | Admin |
| `pardon_ip` | `player`: IP address | Admin |

Use the exact action names above, not shell syntax. Names/addresses and one-line values are validated; a newline is not a way to send a second command. A successful HTTP 202 contains a job. Inspect its final state and refresh the player list before claiming the action succeeded. The server may need to be ready with working local RCON; an unavailable runtime is a real failure, not an offline-success simulation.

Ordinary actions require a ready server. `op_level` instead requires Minecraft to be stopped: it updates an existing `ops.json` record only after a verified backup. Grant OP through Minecraft first; this operation does not fabricate a new player identity. Start Minecraft afterward to load the saved level.

Review OP grants and bans carefully. Revoking a panel user's access does not automatically remove that person's Minecraft OP, whitelist membership, or game login.

## Console boundary

`POST /api/v1/instances/{instanceID}/console` accepts `{"command":"list"}` and returns a job. On success, the command reply is in **`job.result.output`**, not in the initial HTTP response. This is a Minecraft command interface, never a host shell.

Operators are restricted to `list`, `say`, `kick`, `whitelist add/remove/list`, `ban`, `pardon`, and `save-all`. Other commands require admin. Direct `stop`, `save-off`, and `save-on` are rejected even for admins: use lifecycle and coordinated-backup controls. Network/protocol limits can reject a command smaller than the API's outer input limit; keep console commands short and single-line.

## Failure and privacy checks

If a job fails, read its diagnostic before retrying. An RCON timeout is not proof that a command had no effect; inspect current records first. Do not paste IP-ban lists, credentials, complete logs, or private player data into public issues. Real Minecraft moderation and provider-specific behavior remain part of [host acceptance](acceptance.md), not evidence supplied by UI screenshots.
