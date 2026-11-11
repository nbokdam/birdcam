#!/bin/bash
# Run on the Pi: runtest.sh <seconds> <outprefix> <gst-launch args...>
# Runs the pipeline, pipes stdout into rtpstat.py, samples CPU with pidstat.
dur=$1; out=$2; shift 2
t0=$(date +%s.%N)
gst-launch-1.0 -q "$@" 2>"$out.gst.err" | python3 ~/rtpstat.py --interval 30 --duration "$dur" --t0 "$t0" >"$out.stat" &
sleep 1
gpid=$(pgrep -n gst-launch-1.0)
pidstat -h -u -p "$gpid" 10 $(( (dur-5)/10 )) > "$out.cpu" 2>&1 &
wait %1
kill -INT "$gpid" 2>/dev/null; sleep 1; kill "$gpid" 2>/dev/null
wait
awk '!/^#/ && NF>8 {s+=$8; n++} END {if(n) printf "CPU avg %.1f%% (n=%d)\n", s/n, n}' "$out.cpu" >> "$out.stat"
grep -v '^$' "$out.gst.err" | sort | uniq -c | sort -rn | head -5 >> "$out.stat"
