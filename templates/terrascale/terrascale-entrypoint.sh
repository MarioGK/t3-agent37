#!/bin/sh
set -eu
if [ -n "${HOME:-}" ] && (umask 077 && mkdir -p "$HOME/.local/share/t3-agent37") 2>/dev/null; then
  script=/opt/t3-agent37/startup.py
  if [ -f "$HOME/.local/share/t3-agent37/startup.py" ]; then
    script="$HOME/.local/share/t3-agent37/startup.py"
  fi
  (umask 077 && exec /usr/bin/python3 -B "$script" preboot >>"$HOME/.local/share/t3-agent37/preboot.log" 2>&1) &
fi
exec /usr/local/bin/entrypoint.sh "$@"
