#!/bin/bash
# cpuprobe.sh <label> <gst-launch args...>: run pipeline 14s to /dev/null, report CPU and fps-ish output size
label=$1; shift
gst-launch-1.0 -q "$@" > /tmp/probe.out 2>/tmp/probe.err &
P=$!
sleep 4
cpu=$(pidstat -p $P 8 1 | awk '/Average/ {print $8}')
s1=$(stat -c %s /tmp/probe.out); sleep 2; s2=$(stat -c %s /tmp/probe.out)
kill -9 $P 2>/dev/null; wait $P 2>/dev/null
echo "$label: cpu=${cpu}% outrate=$(( (s2-s1)*8/2000 ))kbps err=$(head -c 300 /tmp/probe.err | tr '\n' ' ')"
