#!/bin/sh
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
unset PYTHONHOME PYTHONPATH LD_PRELOAD LD_LIBRARY_PATH
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
exec /usr/bin/python3 -I "$script_dir/install.py" "$@"
