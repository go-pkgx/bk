#!/bin/sh

export VIRTUAL_ENV="$(cd "$(dirname "$0")"/.. && pwd)/venv"
export ARG0="$(basename "$0")"

ZPKG_PYTHON="$(which python)"
ZPKG_PYHOME="$(dirname "$ZPKG_PYTHON")"

cat <<EOSH > "$VIRTUAL_ENV/pyvenv.cfg"
home = $ZPKG_PYHOME
include-system-site-packages = false
executable = $ZPKG_PYTHON
EOSH

find "$VIRTUAL_ENV"/bin -maxdepth 1 -type f | xargs \
  sed -i.bak "1s|.*/python|#!$VIRTUAL_ENV/bin/python|"

rm "$VIRTUAL_ENV"/bin/*.bak

ln -sf "$ZPKG_PYTHON" "$VIRTUAL_ENV"/bin/python

exec "$VIRTUAL_ENV"/bin/$ARG0 "$@"
