#!/bin/bash
# run-one.sh <label> <binary> [env assignments...] -- launch, time to first window, sample tree RSS, kill.
LABEL="$1"; BIN="$2"; shift 2
H="/tmp/lqbench/$LABEL-home"; rm -rf "$H"; mkdir -p "$H"
"$BIN" > "/tmp/lqbench/$LABEL.log" 2>&1 &
PID=$!
T0=$(python3 -c 'import time; print(time.time())')
WIN=$(swift /tmp/lqbench/winpoll.swift "$PID" 90)
RC=$?
if [ $RC -ne 0 ]; then echo "$LABEL: WINDOW TIMEOUT"; kill $PID 2>/dev/null; exit 2; fi
sleep 18
RSS=$(python3 /tmp/lqbench/tree_rss.py "$PID")
kill $PID 2>/dev/null; sleep 2; kill -9 $PID 2>/dev/null
echo "$LABEL pid=$PID window_ms=$WIN rss='$RSS'"
