# Docker: frontend development only

No Docker Minecraft runtime adapter is implemented. `deploy/compose.yaml` runs
the frontend development server only: it does not install the panel, provision
instances, access host systemd, run lifecycle jobs, or make container backups.
Native runtime actions on unsupported/container-only hosts must fail closed.

Choose a trusted Node image compatible with `web/package.json` (pin its digest
for a repeatable environment), then from the repository root:

```sh
export NEC_UI_DEV_IMAGE='your-reviewed-compatible-node-image@sha256:your-reviewed-digest'
docker compose -f deploy/compose.yaml run --rm ui npm ci
docker compose -f deploy/compose.yaml up ui
```

Replace the example image with a real reviewed reference; it is not a published
project image. Source is mounted read-only, dependencies use a named volume,
the UI is bound to `127.0.0.1:5173`, and the container drops capabilities and
disables privilege escalation. Configure any development API proxy explicitly
according to the frontend tooling; no backend is fabricated by this compose file.

Never mount `/var/run/docker.sock`, host systemd, `/etc`, root credentials or
live instance data. Do not add `privileged: true` or a host-root runtime helper.
For actual server management use [native Linux](linux-systemd.md), preferably
in a dedicated host/VM with verified backup and firewall behavior.
