#!/usr/bin/env bash
# Prints a snapshot of the runner's resources: disk, memory, the biggest
# benchmark directories and leftover oar / VM processes. Used by the bench
# workflow between rounds and, with --watch, in the background, to find out
# why long vz jobs lose their runner.
#
#   hack/ci-resources.sh [label]
#   hack/ci-resources.sh --watch <interval-seconds> <log-file>
#
# With --watch it also stops the benchmark when the disk is nearly full, so
# the job still ends normally and uploads its results and this log.
set -u

min_free_kb=$((2 * 1024 * 1024))

snapshot() {
	echo "=== resources $(date -u +%H:%M:%S) ${1:-}"
	df -h / "${TMPDIR:-/tmp}" "$GITHUB_WORKSPACE" 2>/dev/null | awk '!seen[$0]++'
	if [ "$(uname)" = Darwin ]; then
		sysctl -n vm.swapusage
		memory_pressure 2>/dev/null | tail -1
		vm_stat | awk '/Pages (free|active|inactive|wired down|occupied by compressor)/ {printf "%s %d MiB\n", $0, $NF*4096/1048576}' | sed 's/:  */: /'
	else
		free -m
	fi
	uptime
	echo "--- network"
	if [ "$(uname)" = Darwin ]; then
		ifconfig | awk '/^[a-z]/ {i=$1} /inet / {print i, $2, $4}'
		netstat -rn -f inet | awk 'NR>3 && ($1=="default" || $1 ~ /^192\.168/)' | head -8
	else
		ip -4 -br addr
		ip -4 route | head -8
	fi
	t=$(date +%s)
	if host -W 3 github.com >/dev/null 2>&1 || nslookup -timeout=3 github.com >/dev/null 2>&1; then
		echo "dns github.com: ok ($(($(date +%s) - t)) s)"
	else
		echo "dns github.com: FAILED ($(($(date +%s) - t)) s)"
	fi
	echo "--- largest bench dirs"
	du -sh "${TMPDIR:-/tmp}"/oar-bench-home-* "$GITHUB_WORKSPACE"/*/examples/*/.bench-artifacts-* 2>/dev/null | sort -h | tail -8
	echo "--- oar / VM processes"
	ps -A -o pid=,ppid=,rss=,%cpu=,etime=,command= | grep -E 'oar|qemu|Virtualization|hack/bench' | grep -v -E 'grep|ci-resources' | cut -c1-200
}

if [ "${1:-}" = --watch ]; then
	interval=$2 log=$3
	while sleep "$interval"; do
		snapshot watch 2>&1 | tee -a "$log"
		free_kb=$(df -k "$GITHUB_WORKSPACE" | awk 'NR==2 {print $4}')
		if [ -n "$free_kb" ] && [ "$free_kb" -lt "$min_free_kb" ]; then
			echo "::error::runner disk almost full ($((free_kb / 1024)) MiB free): stopping the benchmark" | tee -a "$log"
			pkill -f 'hack/bench' || true
			pkill -f '/bench run' || true
		fi
	done
fi
snapshot "$@"
