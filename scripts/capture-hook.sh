#!/bin/sh
# capture-hook.sh: dump a raw agent hook payload (stdin) to
#   $HOME/.loopguard-capture/<hook_event_name>-<timestamp>-<random>.json
# Used to collect golden payloads for testdata/hooks/ (see docs/capturing-payloads.md).
#
# Like LoopGuard itself, this must never interfere with the agent: it prints
# nothing on stdout/stderr and always exits 0.

exec 1>/dev/null 2>&1

dir="${HOME:-.}/.loopguard-capture"
mkdir -p "$dir" || exit 0

tmp="$dir/.partial-$$"
cat >"$tmp" || { rm -f "$tmp"; exit 0; }

event=""
if command -v jq >/dev/null; then
  event=$(jq -r '.hook_event_name // empty' <"$tmp")
else
  event=$(grep -o '"hook_event_name"[[:space:]]*:[[:space:]]*"[^"]*"' "$tmp" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')
fi
event=$(printf '%s' "$event" | tr -cd 'A-Za-z0-9_-')
[ -n "$event" ] || event="unknown"

ts=$(date -u +%Y%m%dT%H%M%SZ)
rand=$(od -An -N4 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
[ -n "$rand" ] || rand="$$"
out="$dir/$event-$ts-$rand.json"

# Pretty-print with jq when possible; otherwise (no jq, or invalid JSON) keep the raw bytes.
if command -v jq >/dev/null && jq . <"$tmp" >"$out"; then
  rm -f "$tmp"
else
  mv -f "$tmp" "$out"
fi

exit 0
