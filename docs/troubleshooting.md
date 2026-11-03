# Native deployment troubleshooting

Do not fix failures by making HTTP root, adding broad sudo/systemctl permission,
disabling RCON filtering, exposing the Docker socket, or making worlds writable
by everyone. Linux/systemd deployment is still unverified on a live host.

| Symptom | Check / safe next action |
| --- | --- |
| HTTP refuses to start as root | Use `new-empire-control.service` and its non-login account. Root is only for the validated CLI/helper/oneshot paths. |
| Installer rejects the key | Compare the documented raw-key fingerprint and root-owned PEM/ancestor permissions. Never fetch a replacement key beside untrusted assets. |
| Signature/manifest/archive rejected | Confirm the official explicit stable version, canonical schema-1 metadata and matching architecture archive. Do not bypass verification or add unsigned assets. |
| Installer refuses existing files/version | Same-release retries preserve identical artifacts/custom configs. Review differences offline; switch versions through the verified updater. |
| sudo denies helper | Validate `/etc/sudoers.d/new-empire-control` with `visudo --check`; require sudo 1.9.10+ regex arguments, exact wrapper, configured service UID and allowed operation/arguments. No fallback to arbitrary sudo. |
| Registration fails on root/Java path | Root policy requires exact canonical executable allowlist entries and trusted non-writable ancestors. Do not pass `/usr/bin/java` symlinks to the helper or broaden an allowroot to `/`. |
| Create/import is registered but failed | Check the durable job result and root helper journal. Preserve staged/data files and stopped registration for recovery; do not claim runnable success or delete worlds automatically. |
| Unit/manifest mismatch | Keep the unit stopped. Preserve both files/job plan and use the offline reconciliation procedure in operations; an interrupted reconfigure must not execute a mismatched launcher. |
| Managed permissions fail | Check unexpected/root owners, symlinks/hardlinks, mount crossings, tree limits, kernel openat2 and POSIX ACL support. Metadata may be partially repaired, but the JVM is not started. |
| Panel cannot read new Minecraft files | Managed named/default ACLs must survive on the filesystem. Do not grant Minecraft the panel group. For externally prepared adoption, use named panel ACLs or restart only the panel after changing supplementary groups. |
| Adoption rejects access | The helper deliberately never chowns/chmods/ACL-rewrites external data. Stop/back up the external server and prepare both dedicated user and panel access explicitly; also add the narrow panel sandbox write path. |
| RCON guard cannot be installed/verified | Check nftables binary/permissions, inet hooks, exact instance table and container restrictions. A missing guard is fatal, including in LXC. Use a VM rather than weakening isolation. |
| Guard rejects server.properties | Require explicit matching `server-port`, `rcon.port`, `enable-rcon=true` and a nonempty password. Review duplicate/escaped keys, continuations and escaped security values; passwords never belong in logs or manifests. |
| Status says unit missing | Registration/daemon-reload must finish before runtime use. Missing units are errors, not successful offline status. |
| Running JVM is not ready | Check authenticated loopback RCON, actual player-list response and journald. PID existence alone does not prove readiness. |
| Stop timed out | Inspect saving/storage health and confirm whether Java is still alive. `SendSIGKILL=no` is intentional; never apply a restore/update against the still-running process. |
| Audit socket unavailable | Restore systemd-journald `/dev/log`/syslog delivery. Privileged helper actions fail closed without a writable audit sink. |
| Panel update accepted but unchanged | `panel-update` schedules a separate oneshot. Check its journal and durable update result; unsigned metadata, missing CLI adapters, snapshot or readiness failures are real blockers. |
| Docker UI works but runtime does not | The compose file is frontend development only. There is no Docker runtime adapter or container management capability. |

Useful root-admin diagnostics (redact secrets before sharing):

```sh
sudo systemctl status new-empire-control.service new-empire-update.service
sudo journalctl --identifier=new-empire-runtime --no-pager --lines=100
sudo journalctl --unit=new-empire-control.service --no-pager --lines=100
sudo visudo --check
```

Do not paste RCON passwords, session/bootstrap tokens, encryption keys, private
signing material or raw configuration files into issues. Report actual executed
checks, host/kernel/systemd versions and redacted errors; do not infer release
publication or Linux validation from a successful local build.
