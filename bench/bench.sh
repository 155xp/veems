#!/bin/bash
# End-to-end timing for veems on this machine.
#
#   bench/bench.sh [new args...]            e.g.  bench/bench.sh linux
#   CHECK='cd demo && make -q' bench/bench.sh --project demo
#
# Each run measures, in wall-clock milliseconds as a caller sees them:
#   new    `veems new ...` until it prints the id (the guest agent has answered)
#   check  the first `veems exec` (CHECK, default `true`) until it returns
#   ready  new + check: from asking for a VM to having run a command in it
#   rm     `veems rm`
# Environment: VEEMS (binary, default veems), N (runs, default 10),
# CHECK (command line for the first exec), GAP (seconds between runs, default 1).
set -eu
VEEMS=${VEEMS:-veems}
N=${N:-10}
CHECK=${CHECK:-true}
GAP=${GAP:-1}
[ $# -gt 0 ] || set -- linux

now() { date +%s%N; }
ms() { echo $(( ($2 - $1) / 1000000 )); }

news=() checks=() readys=() rms=()
for i in $(seq "$N"); do
  t0=$(now)
  vm=$("$VEEMS" new "$@")
  t1=$(now)
  "$VEEMS" exec "$vm" "$CHECK" >/dev/null
  t2=$(now)
  "$VEEMS" rm "$vm"
  t3=$(now)
  news+=("$(ms "$t0" "$t1")") checks+=("$(ms "$t1" "$t2")")
  readys+=("$(ms "$t0" "$t2")") rms+=("$(ms "$t2" "$t3")")
  printf 'run %2d  new %5d  check %5d  ready %5d  rm %5d\n' "$i" "${news[-1]}" "${checks[-1]}" "${readys[-1]}" "${rms[-1]}"
  sleep "$GAP"
done

stats() {
  sort -n | awk '{ a[NR] = $1 } END { printf "median %5d  min %5d  max %5d\n", a[int((NR + 1) / 2)], a[1], a[NR] }'
}
echo "veems new $*   (N=$N, check: $CHECK)"
printf '  new    '; printf '%s\n' "${news[@]}" | stats
printf '  check  '; printf '%s\n' "${checks[@]}" | stats
printf '  ready  '; printf '%s\n' "${readys[@]}" | stats
printf '  rm     '; printf '%s\n' "${rms[@]}" | stats
