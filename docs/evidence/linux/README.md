# Disposable Linux service evidence

On **2026-11-02**, a private QEMU/HVF ARM64 VM ran Debian **13.7**, kernel
**6.12.113+deb13-cloud-arm64** and systemd **257.13-1~deb13u1**. Ext4 ACLs and
persistent default-drop egress had already passed reboot checks. QEMU remained
restricted: no host shares, host credentials, bridge or direct public egress.
The cloud image had HTTPS-origin/checksum trust, not an independently signed
image. This is not distribution certification or production-readiness evidence.

## Development service fixture

The fixture binary was built from **`ad8be19cd9d03d03d862dea9b3d776764020a48b`**
with development version `0.0.0-dev.2` and the unchanged production public key.
Its SHA-256 was
`bb8dbf22c554a876cc5e18940c62286bbc410ccdf73f6dfb48221996cd0aa1d0`.
The deployment layout/unit-writing code was reused with original repository
wrappers, units, sudoers and configuration. **The official signed-release
installer entrypoint was not run or certified.** No signature gate, key or
application transport was replaced; this was an explicitly separate development
fixture, not an official installation or update.

The actual HTTP service ran as panel UID **999**, initially PID **2073**.
Readiness and `/proc/<pid>/exe` matched the selected binary, and an attempt to run
arbitrary sudo as that account was denied. The panel remained loopback-only.

## Actual-schema recovery drill

An assert-based private fixture invoked the actual CLI and fixed systemd service:

```sh
sudo -n python3 -I /root/nec-development-fixture/schema-service-drill.py
```

It stopped the panel, verified and retained its standalone database copy, then
fault-injected `PRAGMA user_version` as the panel account while holding the
existing `panel.lock`. A root-owned canonical fixture journal selected interrupted
pre-switch recovery; it was not an authenticated staged release. Each case ran:

```sh
/opt/new-empire-control/control update --independent \
  --config /etc/new-empire-control/config.json recover
```

- Actual schema **0** and **2**: helper exit **1**, journal `manual_recovery`,
  service `MainPID=0`. Database bytes/ownership/mode/mtime, installed binary and
  current symlink remained unchanged by the helper. Private schema-copy storage
  was removed after checking.
- Actual schema **1**: helper exit **0**, unchanged panel restarted as PID
  **2222**, and authenticated version/schema/PID readiness passed. This confirms
  the helper released `panel.lock` before service startup. The journal correctly
  recorded the interrupted update as **`failed`**, not as an applied update.
- Explicit fixture cleanup restored the verified original database and original
  journal state, then restarted the panel as PID **2239**. Original bootstrap
  state was confirmed. No Minecraft instance or world existed during this drill.

[Sanitized actual results](schema-service-evidence.json) contain no database,
token, password, key material or host-private paths. This proves the real
stopped-state guard/restart boundary, **not** official release discovery,
signature verification, application, post-switch rollback or unchanged running
Minecraft during an update. Those remain separate acceptance gates.
