# Native operations

Linux/systemd integration is not yet live-host verified. These procedures describe
the intended deployment, not evidence that release or recovery acceptance passed.

## Boundaries

`new-empire-control.service` runs the unprivileged panel. Each instance has its
own `nec-mc-<32 hex>.service`, dedicated account/group, root registration, data
root, ports and nftables RCON guard. `new-empire-update.service` is an independent
root oneshot and may restart **only the panel**, never Minecraft or world data.
There is no arbitrary shell/API unit-execution permission.

```sh
sudo systemctl status new-empire-control.service
sudo journalctl --unit=new-empire-control.service --no-pager --lines=100
sudo journalctl --identifier=new-empire-runtime --no-pager --lines=100
```

Instance IDs are exactly 32 lowercase hex characters. Prefer panel lifecycle
jobs (durable locks, roles, audit) over independent root interventions. A job
acceptance is not proof of completion or readiness; check its result and RCON
readiness. Never edit/import/restore/content-update a running world.

## Registration and reconfiguration

The panel invokes `sudo -n /usr/local/libexec/new-empire-runtime provision`,
one bounded model JSON on stdin. The root policy approves exact Java paths,
managed/adoption roots and memory limits; units and manifests remain root-owned.
No root policy is editable by HTTP. A failed create/import can leave a stopped,
partially populated registered directory: preserve it for diagnostics/retry and
do not delete it as automatic cleanup.

Java/launcher/memory changes require preflight, verified backup, staging, a
graceful stop and `reconfigure <ID>` under the same job lock. ID/root/ports/user
cannot be changed through that operation. App-managed versions are metadata
choices, not proof that a root Java executable was approved. Add new exact
canonical Java paths to the root policy through normal administration first.

Reconfigure writes two root files, then daemon-reloads. An interruption between
replacements is detected as a unit/manifest mismatch and cannot start a JVM.
For offline recovery: keep the instance stopped, preserve both root files and
the failed job plan, verify the old/new Java/launcher paths and backup, then
reconcile the unit with the intended manifest using reviewed deployment code.
Do not disable comparison checks, hand-insert arbitrary ExecStart commands or
roll back worlds just to repair a control-plane registration. This mismatch
recovery is currently administrator-assisted, not automatic.

## Managed permissions and adopted data

Managed roots are panel-owned. Preparation normalizes per-entry ownership to the panel UID, assigns
only the instance GID, enforces directories 02770/files 0660 and replaces access/
default ACLs with the restricted named-panel policy. Only panel/this-instance
owners are accepted. It never traverses links, hardlinks, mounts or special
files; ACL/openat2 errors leave the instance stopped. In a race/error, some
permission metadata may already be repaired; retry offline after inspection.

After root-replacing restore or atomic 0600/0700 content changes, restricted
`prepare <ID>` can repair a stopped managed tree and refresh its root identity.
The root-only ExecStartPre guard does the same before every actual JVM start.
Successful helper stop proves MainPID zero, inactive/failed state and absence of
any remaining instance-UID processes before normalizing newly created files.
Offline atomic edits can then preserve panel ownership instead of losing the
Minecraft-created file's owner or invoking a privileged arbitrary write.
It also verifies the launcher and `server.properties`/registered port agreement.
Adopted trees are validated only: no recursive ownership, mode or ACL changes.
If an atomic edit cannot preserve the adopted owner/group it must fail untouched.
Prepare adopted access explicitly as in the installation guide. Unexpected
adopted root replacement always requires administrator review.

## Firewall and graceful stop

Each `inet nec_<ID>` table has a verified input hook dropping the registered
RCON TCP port on every non-loopback interface. Both IP families are covered.
Never treat a loopback RCON client or a public game bind as sufficient isolation.
An administrator flushing/replacing firewall rules while Java runs removes that
protection; coordinate firewall maintenance and reverify before a server start.
Other firewalls must coexist with, not erase, the per-instance guards.

Systemd sends SIGTERM, waits 120 seconds and never automatically sends SIGKILL.
If saving hangs, inspect journald, disk space and filesystem health. Do not
force-kill or mutate a still-saving world just because a job timed out. Confirm
the actual process has exited before applying file changes or retrying start.

## Backups and application updates

Keep verified backups/recovery files outside all instance roots. Preserve the
panel database, encryption/recovery material, root config/registrations, release
public key and independent verified world backups in disaster-recovery copies.
Test restoration in a disposable instance; filesystem/container snapshots alone
do not prove a consistent running-world backup.

Native release archives contain only the embedded-UI ELF binary. The installer
and updater verify official signed metadata, checksums, architecture and version
policy. Deployment assets are separate. `panel-update` only schedules the fixed
oneshot; its success means accepted, not that update/health checks passed.
Read the durable updater result and the oneshot journal. Reconfiguration and
world recovery are separate actions; a panel update must never restart Minecraft.

Updater journals, locks, staging and snapshots remain root-private under
`/opt/new-empire-control/updates` (root:root 0700). The panel submits requests and
reads root-owned 0644 operation reports inside its own 0700 data directory; it
does not read or write the private journal. Version binaries and `current` remain
administrator-controlled. Legacy panel-owned updater state is refused, not
silently chowned into trust: preserve/review it offline and provision clean state.

Automatic maintenance reuses a matching verified stage and rechecks the current
policy, channel, window and postponement immediately before applying it. Failed
versions do not retry automatically until reviewed and successfully staged
manually; a newer version remains eligible. After restart, only interrupted jobs
are reconciled from final-operation reports. A successful stage alone does not
complete an automatic apply workflow.

The updater requires an existing safe, panel-owned `panel.lock`; it never creates
a root-owned replacement. If absent, a normal panel start can create it. If a
legacy root-owned or unsafe lock exists, stop the panel and inspect/repair that
one file offline. Do not remove a held lock or grant group/world write access.

## Validation before adoption into service

Required live checks remain: create/import/adopt jobs with failed preflight;
permission preparation after restore; graceful/stalled stop; RCON rejection from
external IPv4 and IPv6; unit isolation and sandbox behavior; signed install and
tamper rejection; panel-only update/failed-health recovery; reboot behavior and
backup restore. Run them on disposable data before trusting this deployment.
