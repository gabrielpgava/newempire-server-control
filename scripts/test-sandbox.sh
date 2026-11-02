#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/nec-tests.XXXXXX")"
modules="$(go env GOMODCACHE)"
image="golang:$(cat "$root/.go-version")-bookworm"
if [ "$#" -eq 0 ]; then set -- ./...; fi
# Only application sources enter the sandbox; no Git credentials or other worktrees.
python3 - "$root" "$scratch/source" <<'PY'
import pathlib, shutil, sys
root, dst = map(pathlib.Path, sys.argv[1:])
dst.mkdir()
for name in ('go.mod', 'go.sum', 'internal', 'cmd', 'web', 'deploy', 'scripts'):
    source = root / name
    if not source.exists():
        continue
    if source.is_dir():
        shutil.copytree(source, dst / name, ignore=shutil.ignore_patterns('node_modules', '.git', 'test-results', 'playwright-report'))
    else:
        shutil.copyfile(source, dst / name)
PY
# Docker tmpfs may lack POSIX ACLs; only the helper's small ACL fixture uses this private volume.
volume="nec-acl-$(basename "$scratch")"
docker volume create "$volume" >/dev/null
trap 'docker volume rm "$volume" >/dev/null' EXIT
# These capabilities exercise ownership-preservation tests only inside isolated test storage.
docker run --rm --network none --read-only --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add FSETID --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges \
  --cpus 2 --memory 4g --pids-limit 256 --ulimit nofile=1024:1024 --ulimit fsize=1073741824:1073741824 \
  --tmpfs /scratch:rw,exec,nosuid,nodev,size=2147483648 \
  --mount "type=bind,src=$scratch/source,dst=/source,readonly" \
  --mount "type=bind,src=$modules,dst=/modules,readonly" \
  --mount "type=volume,src=$volume,dst=/acl-fixtures,volume-nocopy" \
  -w /source "$image" sh -c \
  'mkdir -p /scratch/tmp /scratch/cache /scratch/home && exec env -i PATH=/usr/local/go/bin:/usr/bin:/bin HOME=/scratch/home TMPDIR=/scratch/tmp GOTMPDIR=/scratch/tmp GOCACHE=/scratch/cache GOMODCACHE=/modules GOPROXY=off GOSUMDB=off CGO_ENABLED=1 NEC_TEST_ACL_DIR=/acl-fixtures go test -race -count=1 -timeout=5m "$@"' test "$@"
