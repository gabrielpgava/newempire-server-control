# Proxmox LXC

Use the [native systemd guide](linux-systemd.md) **inside** a dedicated Linux
container, not on the Proxmox host. This configuration is unverified on live
Proxmox; no blanket LXC compatibility or security claim is made.

- Require systemd/journald, kernel openat2, POSIX ACLs, dedicated service accounts
  and the guide's package/tool versions. Verify bind-mounted data ACL behavior
  with the container's UID/GID mapping before importing worlds.
- The helper must be able to install/verify nftables input protection **in the
  same network namespace as Minecraft**. Unprivileged LXC commonly blocks the
  needed capability. In that case start fails intentionally. Do not disable the
  guard, make the panel root, enable privileged Docker, or grant broad host access.
- Prefer a dedicated VM when safe per-container nftables/systemd sandboxing is
  unavailable. A host firewall alone is not silently substituted for helper
  verification; that adapter is not implemented.
- Keep backups outside instance roots and copy verified disaster-recovery
  material outside the container. Container snapshots are not a replacement for
  an application-coordinated or offline Minecraft backup.
- Expose only game and approved HTTPS proxy ports. Never forward RCON. Keep the
  panel on loopback behind your authenticated deployment/network boundary.

Before live use, verify actual start/stop, IPv4/IPv6 RCON rejection, a verified
backup/restore and panel-only updater recovery in a disposable container. Do not
weaken isolation merely to make the checks pass.
