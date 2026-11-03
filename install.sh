#!/usr/bin/env bash
set -euo pipefail

install_control() (
    set -euo pipefail
    PATH=/usr/sbin:/usr/bin:/sbin:/bin
    export PATH
    unset PYTHONHOME PYTHONPATH LD_PRELOAD LD_LIBRARY_PATH
    umask 077
    if [[ "$(uname -s)" != Linux ]]; then
        echo "Installation requires Linux with systemd (amd64 or arm64)." >&2
        exit 1
    fi
    for tool in curl python3 sha256sum timeout; do
        command -v "$tool" >/dev/null || { echo "Required command missing: $tool" >&2; exit 1; }
    done
    work="$(mktemp -d /root/new-empire-bootstrap.XXXXXXXX)"
    trap 'rm -rf -- "$work"' EXIT
    address="$(timeout 10 python3 -I -c 'import ipaddress,socket; values=list(dict.fromkeys(row[4][0] for row in socket.getaddrinfo("raw.githubusercontent.com",443,type=socket.SOCK_STREAM))); assert values and all(ipaddress.ip_address(value).is_global for value in values), "GitHub DNS must contain only public addresses"; print("["+values[0]+"]" if ":" in values[0] else values[0])')"
    status="$(curl --disable --fail --silent --show-error --proto '=https' --tlsv1.2 \
        --noproxy '*' --resolve "raw.githubusercontent.com:443:$address" \
        --connect-timeout 10 --max-time 30 --max-filesize 65536 \
        --output "$work/bootstrap.py" --write-out '%{http_code}' \
        https://raw.githubusercontent.com/gabrielpgava/newempire-server-control/main/deploy/bootstrap.py)"
    [[ "$status" == 200 ]] || { echo "Bootstrap download did not return HTTP 200." >&2; exit 1; }
    printf '%s  %s\n' 12567d9068c4987cf1d5313ad6f31a491a1a87876f2fcca18a7844a27b984d7c "$work/bootstrap.py" | sha256sum --check --status
    python3 -I "$work/bootstrap.py" "$@"
)

if [[ "$(id -u)" == 0 ]]; then
    install_control "$@"
else
    sudo /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /bin/bash \
        -c "$(declare -f install_control); install_control \"\$@\"" bash "$@"
fi
