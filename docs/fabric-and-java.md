# Fabric, Java, and native runtime

## Runtime choices come from upstream

The Fabric domain client reads the official Fabric [metadata API](https://meta.fabricmc.net/)
and [endpoint documentation](https://github.com/FabricMC/fabric-meta#fabric-meta).
`Versions` returns supported games, loader releases, and installers with their
upstream `stable` flags. The API/UI should show stable choices by default and
make unstable selections explicit. No current game, loader, installer, or Java
release is hard-coded.

`Loaders(ctx, game)` uses the game's dedicated Fabric loader metadata endpoint.
Fabric chooses an intermediary mapping version; it is not necessarily identical
to the Minecraft version. The plan records the authoritative intermediary choice
and validates its Maven identity rather than guessing a mapping version.

Planning requires explicit `minecraft_version`, `loader_version`, and
`installer_version`. All must appear in official metadata, including the selected
game/loader combination. Mutable aliases such as `latest` are not resolved.

The Java minimum comes from the selected version's `javaVersion.majorVersion` in
Mojang's version metadata. The client discovers that document through the official
[Mojang version manifest](https://piston-meta.mojang.com/mc/game/version_manifest_v2.json)
and verifies its bytes against the manifest's SHA-1 before parsing them. It also
checks the version identity and official Minecraft server URL, hash, and size.
The effective minimum is the greater of Mojang's requirement and Fabric's
declared loader minimum, when present.

Old versions lacking a declared Mojang Java requirement fail closed. A Java
minimum is not proof of mod compatibility, nor a promise that every future JVM
will run an old modpack. Test the chosen Java, Fabric, and mods before migrating
valuable worlds. Some old Fabric loaders/installers are listed in metadata but
are not supported by Fabric's unattended launcher endpoint; staging will return
the upstream error rather than attempt another installation method.

## Download trust and staging

`fabric.NewClient(nil)` uses `internal/netguard.New`: official HTTPS hosts only,
public DNS addresses only, no proxy inherited from the environment, bounded
redirects, and bounded response sizes. Tests may inject a trusted `*http.Client`
transport; end users must not control that injection. URLs and redirects are
still validated. Metadata responses are limited to 8 MiB and each request has a
two-minute deadline (or the earlier caller deadline).

Fabric's [server launcher endpoint](https://fabricmc.net/use/server/) generates
a JAR by embedding `install.properties` into the server installer. Its upstream
[implementation](https://github.com/FabricMC/fabric-meta/blob/master/src/main/java/net/fabricmc/meta/web/ServerBootstrap.java)
does **not** publish an independent checksum or signature for that generated
artifact. Accordingly, plans explicitly report
`official_fabric_https_without_published_checksum`. This is official HTTPS-origin
trust, **not** independent checksum/signature authentication. The recorded local
SHA-256 detects later changes to staged bytes; it does not strengthen origin
authentication. The application must not describe this launcher as signed or
independently verified. Mojang metadata hashes are verified where published.

`Stage(ctx, selection, stagingRoot, properties)` replans from upstream and downloads
only the official generated launcher. The JAR must be at most 64 MiB compressed,
have at most 10,000 entries and 256 MiB total declared expanded content, and have
valid archive CRCs, safe entry paths, no symlinks, the expected server bootstrap
entry point, and embedded game/loader versions matching the selection. Nothing
is extracted or executed.

The caller supplies a panel-owned **0700 staging root outside every instance**.
Rooted filesystem access (`os.Root`) creates a unique `runtime-<random-ID>` child
directory containing only:

- `launcher.jar` — downloaded launcher, mode 0600.
- `server.properties` — validated caller-supplied bytes, mode 0600; may contain
  an RCON credential and must never be downloadable through a general file API.
- `manifest.json` — plan, directory, and local launcher/properties SHA-256 hashes,
  written last and synchronized with the stage and its parent directory.

Existing files are never overwritten; failed stages remove only their own known
artifacts. A process/power failure can leave an incomplete stage, which must not
be applied. Staging itself does not stop a server, create a backup, alter an
instance, accept the Minecraft EULA, or run Java. Fabric's bootstrap downloads
the Minecraft server and libraries on the first approved launch; this is not a
fully offline installation or a panel-verified library cache.

The job/API owner must compare `StageResult.Plan` against the administrator's
approved plan hash, inspect the configured Java executable, create and verify an
appropriate restore point, gracefully stop the instance, verify it is stopped,
recheck staged hashes, and commit runtime/properties files through its audited
file transaction. Stage output is `launcher.jar`; the integration owner chooses
the installed launcher filename and updates the instance configuration. A failed
runtime/content update must not automatically roll back world data. Starting is
a separate operation requiring explicit EULA acceptance and a deployment-created
`eula.txt`; neither staging nor the adapter creates it.

## Inspecting Java safely

`InspectJava(ctx, executable, allowlist)` executes only an exact, clean absolute
path in the administrator-configured allowlist, and only with `-version`. It
never searches `PATH`, installs Java, scans arbitrary directories, or uses a
shell. The executable must be a regular executable and cannot be group/world
writable. Administrators must also protect its parent directories and symlink
targets from untrusted replacement. Probes have five-second deadlines and 64 KiB
combined output limits; Java option injection environment variables are not
inherited.

`InspectJavaInstallations(ctx, allowlist)` checks up to 32 unique configured paths,
returning per-path failures rather than inventing an installation. An empty
allowlist is an actionable configuration error. `ParseJavaVersion` supports
legacy `1.x` and modern Java version output. `ChooseJava` chooses the smallest
working configured major satisfying the metadata minimum. No JAR is run during
inspection.

## Imported and adopted runtime identity

Initial import/adoption does not trust browser-supplied version labels. `InspectSourceLauncher`
reads a bounded regular JAR without executing it and supports Fabric's unattended bootstrap
format: `META-INF/MANIFEST.MF` must select `net.fabricmc.installer.ServerLauncher`,
`install.properties` must contain valid `game-version` and `fabric-loader-version`, and the
bootstrap class must have a valid Java class header. This matches the official
[Fabric installer launcher](https://github.com/FabricMC/fabric-installer/blob/master/src/main/java/net/fabricmc/installer/ServerLauncher.java).
Its paths are relative to the process working directory, so a wrapped archive must be
rebased to its actual server root before registration; pointing `-jar` into the wrapper
does not select that wrapper's world or properties.

Source inspection uses the existing 64 MiB compressed JAR, 10,000-entry, 256 MiB expanded,
safe-path, duplicate-entry, and CRC bounds. Embedded properties are limited to 32 KiB.
Legacy installed launchers, custom launchers, and non-Fabric launchers without this
verifiable structure fail closed. They are not silently converted or labelled Fabric.
Supply a supported launcher through a separate reviewed offline migration; preserve the
original world and take a verified backup first.

Both import and adoption resolve the detected game/loader through official Fabric and
hash-verified Mojang metadata, then inspect the selected allowlisted Java executable.
The effective minimum is the maximum of Mojang's requirement, Fabric's declared loader
minimum, and the bootstrap class's Java requirement. Source/runtime mismatches and
insufficient Java fail before service provisioning. The initial preview reports
`minecraft_version`, `loader_version`, and `java_required`; apply rechecks those facts
and the exact preview hash. A changed authoritative minimum requires reinspection.

Static metadata inspection proves consistency of supported launchers, **not their origin
or safety**. Uploaded/adopted code is still administrator-supplied code. No untrusted JAR
is executed during inspection, and neither a successful parse nor a Java minimum proves
mod compatibility. See [source roots and adoption reservations](server-import.md) for
the request fields and stable dedicated-account preparation/retry flow.

## Properties and adoption

`ReadProperties` parses bounded UTF-8 Java properties, including escaped keys,
Unicode escapes, continuations, comments, and last-key-wins duplicates.
`UpdateProperties` is a pure byte transformation: untouched logical records,
unknown keys, comments, duplicate records, and line endings remain intact. It
changes only the effective last occurrence of an edited key, appends new keys
deterministically, and validates common ports, booleans, modes, player limits,
world-folder names, and RCON passwords. Invalid UTF-8/Unicode escapes fail rather
than being silently rewritten. Property documents are limited to 1 MiB.

`ManagedProperties(instance, existingBytes)` explicitly prepares Minecraft/RCON
ports and the configured RCON secret for a **new managed installation or a
confirmed migration**. It performs no filesystem writes. It must not be called
implicitly to adopt or change an externally managed instance. Adoption must
first validate safe existing RCON configuration and a known credential, or
require a separate administrator-approved migration with backup and shutdown.
HTTP property readers must omit RCON secrets; writers must allow only their
supported safe keys and reserve managed credentials/ports for the proper workflow.

## systemd and RCON boundaries

`runtime.NewSystemd(helper)` implements `runtime.ServerRuntime`; see the exact
privileged helper protocol in [`internal/runtime/API.md`](../internal/runtime/API.md).
The adapter uses an absolute helper path with `/usr/bin/sudo -n` and fixed argument
arrays, never a shell. Non-Linux systems return `runtime.ErrUnsupported`, not a
simulated working runtime. Lifecycle helper calls have a two-minute deadline;
status/log calls have a thirty-second deadline. Call completion is not readiness.

Status combines systemd process/accounting fields and Linux `CLOCK_MONOTONIC`
uptime with authenticated RCON `list`. An active JVM alone is not healthy. Domain
states are `stopped`, `starting`, `running`, `stopping`, `failed`, `unhealthy`, and
`unknown`; `Ready` is true only after a valid Minecraft player list. The HTTP
integration maps these to its public status vocabulary. Custom/mod-localized
player-list output may fail readiness rather than produce invented player data.
Missing accounting is reported as zero, not a measured consumption guarantee.
Logs are bounded newline-delimited journald JSON; malformed/oversized output is
an error. Treat log text as potentially sensitive and authorize access.

`rcon.Execute` accepts numeric loopback endpoints only, authenticates with checked
request IDs, bounds packets/responses, uses five-second deadlines and context
cancellation, and gathers multi-packet replies until a distinct empty-command
response boundary. Timeouts/disconnects are errors, never truncated success.
Passwords are not included in errors. Console input is a single UTF-8 line of at
most 1446 bytes, not an OS shell command. Responses are limited to 4 MiB and 4096
packets. Runtime console calls always target `127.0.0.1:<instance RCON port>`.

**Client loopback dialing does not isolate the listening RCON server.** Vanilla
may bind it on all interfaces when `server-ip` is blank. Before start/restart, the
privileged helper must enforce root-owned firewall rules denying external access
to each instance's RCON port (or enforce an explicitly secured internal network).
Without that enforcement, deployment/start must fail; do not claim RCON is safe
because the panel dials loopback. The parent integration owns this helper,
service units, credential storage, deployment firewall, role checks, durable
jobs, exclusive instance locks, backups, recovery, and final file application.

## Verification

Domain tests use real loopback TCP mock RCON peers, bounded helper-process tests,
fake official-host HTTP transports, staged JAR fixtures, and an allowlisted
`-version` executable. They cover authentication/ID failures, chunking, deadlines,
malformed metadata, origin/hash/Java failures, readiness failures, filesystem
escapes, and properties preservation. Run:

```sh
go test -race ./internal/runtime ./internal/rcon ./internal/fabric
go vet ./internal/runtime ./internal/rcon ./internal/fabric
NEWEMPIRE_UPSTREAM_TEST=1 go test ./internal/fabric -run TestLiveUpstreamStage -v -count=1
```

The opt-in live smoke test discovers current stable versions and stages an
official launcher without running it. Actual Linux/systemd service, firewall,
Minecraft readiness, first-launch downloads, and migration/recovery acceptance
tests remain the integration owner's responsibility; mocked adapter tests are
not evidence that those deployment tests passed.
