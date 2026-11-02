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

## Real API import and first-launch failure

On the same VM/source, the actual API passed owner bootstrap, authenticated Java
inspection, upload HEAD/offset resumption, rejection of an incorrect upload
offset, fresh official metadata preview, explicit EULA rejection/acceptance, and
the native import/provisioning job. The **66,403,718-byte** ZIP included the
unchanged official launcher/server/libraries and an explicitly **locally derived
595-byte manifest-only Fabric cache JAR**. That cache is not an official signed
runtime asset. Java was **25.0.5**, installed offline from authenticated Debian
packages; metadata selected Minecraft **26.3** / Fabric Loader **0.20.1**.

The application used its original resolver, netguard and TLS validation. A
temporary test-only channel admitted only Fabric/Mojang metadata through two
exact guest hosts mappings, two public `/32` loopback routes, one owned NAT table
and a bounded SNI/public-address-checked TLS-byte relay. Six independent fresh
metadata probes as UID 999 passed default CA/hostname verification over TLS 1.3;
Mojang's selected version SHA-1 was checked. No application HTTP response or
transport was faked, no CA/key was replaced, QEMU remained restricted and the
protective firewall was unchanged. This fixture channel is **not a supported
production network/proxy configuration**.

**The Minecraft start job failed; readiness was not reached.** The actual Java
server and Fabric executed, but the watchdog fired while the server thread
waited for Authlib's service/public-key discovery. The discovery thread was
blocked in DNS for an endpoint outside the metadata-only channel. Logs named
`https://discovery.minecraftservices.com/minecraft/client`; a separate read-only
fetch of that official discovery document identified
`https://api.minecraftservices.com/publickeys`. Authentication, secure-profile
enforcement and the watchdog were not disabled to force a passing result.

The API truthfully reported the failed start and `ready:false`; fixture cleanup
left the new instance stopped. The planned RCON/firewall, backup/restore,
successful restart and panel-update checks were **not reached**, not passed.
The temporary channel was then removed: owned routes/NAT/listeners/processes
were deleted and original guest hosts/SSH/network state restored, without
changing the protective firewall or removing the application's instance guard.
[Sanitized partial results and exact job IDs](api-first-launch-evidence.json)
preserve this failure independently of any later retry. No production data or
valuable world was used.
