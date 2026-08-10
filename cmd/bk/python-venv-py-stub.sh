#!/bin/sh

export VIRTUAL_ENV="$(cd "$(dirname "$0")"/.. && pwd)/venv"
export ARG0="$(basename "$0")"

PKGX_PYTHON="$(which python)"
PKGX_PYHOME="$(dirname "$PKGX_PYTHON")"

cat <<EOSH > "$VIRTUAL_ENV/pyvenv.cfg"
home = $PKGX_PYHOME
include-system-site-packages = false
executable = $PKGX_PYTHON
EOSH

find "$VIRTUAL_ENV"/bin -maxdepth 1 -type f | xargs \
  sed -i.bak "1s|.*/python|#!$VIRTUAL_ENV/bin/python|"

rm "$VIRTUAL_ENV"/bin/*.bak

ln -sf "$PKGX_PYTHON" "$VIRTUAL_ENV"/bin/python

exec "$VIRTUAL_ENV"/bin/$ARG0 "$@"
