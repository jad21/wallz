#!/bin/sh

# Objetivo: preferir el binario instalado y conservar el fallback durante la integración gradual.
if command -v wallz >/dev/null 2>&1; then
    exec wallz rotate "$@"
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$script_dir/rotate-wallpaper.py" "$@"
