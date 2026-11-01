#!/bin/sh
# Build veems into ~/.local/bin (or $PREFIX/bin) and install the agent skill.
set -eu
cd "$(dirname "$0")"
bin="${PREFIX:-$HOME/.local}/bin"
mkdir -p "$bin"
go build -trimpath -ldflags="-s -w" -o "$bin/veems" .
echo "installed $bin/veems"
"$bin/veems" skill install
case ":$PATH:" in *":$bin:"*) ;; *) echo "note: add $bin to your PATH" ;; esac
"$bin/veems" doctor || echo "then run: veems build linux"
