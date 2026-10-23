# Where `runc create` spends its time, cheap knobs, and crun

Item A4 of `perf-round-2-plan.zh-CN.md`. Everything below was measured on one native Linux host; there is no KVM on it, so the guest numbers
(19.5 ms create) are not reproduced here, only the mechanisms and the deltas.

## Method

- Host: Linux 7.0.0-38 (Ubuntu 26.04), 8 vCPU AMD EPYC 9645 (itself a VM), HZ=1000, shared with other jobs (load average 2 to 4).
  runc 1.4.0 (host package, dynamic) and runc 1.5.2 (the guest's static binary), crun 1.21 and 1.30.1 (upstream static builds).
- Bundle: the spec the agent generates for `examples/agent-runtime` (overlay rootfs, 10 mounts, 5 namespaces, 10 masked + 5 read-only paths,
  14 caps), process `/bin/sleep`, cgroup `/oar/<leaf>`. Stdio as in `Runc.Create`: stdin `/dev/null`, stdout/stderr to a file.
- Timing: a Go harness using `os/exec` like the agent, variants interleaved round by round with a rotating order, 20 to 100 ms idle between
  iterations, 3 warm-up rounds. Medians and p90 only; with an IQR of about 4 ms a median of n=40 is good to roughly +-0.7 ms, n=150 to +-0.4 ms.
- Tracing: bpftrace (sched and syscall tracepoints, following the process tree, 1 us per probe). `strace -f` perturbs `runc create`: plain
  `strace -f` made nsexec fail in 2 of 3 runs and hangs while the created init is traced; `strace -f --seccomp-bpf -e trace=...` works and
  agrees with bpftrace (cgroup.procs write 4.2 ms, unshare 0.33 ms, pivot_root 0.64 ms, 8 bpf calls).
- CPU: `perf stat` (task-clock of the whole tree).

## Part 1: breakdown of `runc create` (runc 1.4.0, 15 traced runs, ms, median)

| phase | ms | what it is |
|---|---|---|
| A runc parent: Go runtime + CLI until it opens config.json | 3.3 | process start (a bare `runc --version` is 4.3 ms elapsed, 3.8 ms CPU) |
| B parent: load and convert the spec | 1.0 | |
| C parent: state dir, exec.fifo, spawn `runc init` | 1.2 | re-exec through an overlay mount of /proc/self/exe |
| D init stage 0 (C code) + parent creates the cgroup dir | 1.6 | mkdir of the cgroup 0.26 |
| **E parent writes the init pid to cgroup.procs** | **5.4** (3.1 to 15.5, mean 6.6) | `synchronize_rcu` inside `rcu_sync_enter` (cgroup threadgroup rwsem) |
| F nsexec: clone, unshare(0.44), clone | 1.0 | |
| G init Go runtime start until libcontainer's first syscall | 3.9 | second process start |
| H rootfs: 13 mounts, 6 mknod, 6 symlinks | 2.7 | only 0.43 ms of it is time inside the syscalls; the rest is runc's Go code |
| I parent: device eBPF filter (overlaps J) | 0.5 | 8 `bpf()` calls, 3 of them feature probes |
| J pivot_root (0.66), 23 masked/read-only mounts (0.77 wall), caps | 2.5 | |
| K final sync, exit | 1.2 | |
| total exec to exit | 25.5 (traced), 24.0 untraced | medians do not add up exactly |

By cause: two Go runtime starts 7.2 ms (28%), the RCU grace period 5.4 (21%), rootfs setup and pivot 5 to 6 (23%), parent-side preparation 3.8 (15%),
nsexec 1.0, eBPF 0.5, final sync 1.2. CPU is 21.2 ms for 26.8 ms elapsed (perf stat, n=60), 400 context switches, 1781 page faults; the
parent alone burns 12.6 ms CPU because the Go runtime spins threads. So `create` is CPU-bound with one 5 ms sleep in it.

Per command (n=100 interleaved, median / p90 ms; CPU from perf stat n=60):

| | create | start | state | delete -f | CPU create / start / state |
|---|---|---|---|---|---|
| runc 1.4.0 | 24.0 / 28.8 | 4.9 / 6.2 | 4.4 / 5.7 | 106.3 / 107.7 | 21.2 / 5.1 / 4.8 |
| runc 1.5.2 | 24.9 / 29.1 | 5.2 / 6.1 | 4.5 / 5.8 | 106.3 / 107.8 | 22.6 / 4.9 / 5.0 |
| crun 1.30.1 | 5.0 / 5.9 | 1.5 / 2.0 | 1.3 / 1.8 | 1.4 / 2.0 | 5.2 / 1.1 / 1.3 |
| crun 1.30.1 no-systemd | 4.7 / 6.1 | 1.3 / 1.7 | 1.2 / 1.5 | 1.3 / 1.6 | |
| crun 1.21 | 5.7 / 6.7 | 1.4 / 1.9 | 1.3 / 1.6 | 1.4 / 2.3 | |

`start` and `state` are almost entirely process start (a Go exec is 4 to 5 ms); `delete -f` is the 100 ms poll in runc's `killContainer`.
Syscalls per create: 3375 (1.4.0, bpftrace mean of 15; `strace -f -c` 3679 in the one run that survived), 6477 for 1.5.2 (libpathrs:
796 fstatfs, 676 newfstatat, 558 statx) at the same wall time, so syscall count is not the bottleneck.

## Part 2: knobs (create median ms, interleaved)

| knob | n | result | verdict |
|---|---|---|---|
| **`rcu_expedited=1`** (host sysfs), 1.5.2 | 60 | 27.7 -> 19.7 (-8.0); cgroup.procs write 5.4 -> 0.09 | **-8 ms**, nothing else in create moved |
| same, runc 1.4.0 | 60 | 26.6 -> 18.8 (-7.8) | |
| same, pinned to 2 CPUs | 60 | 26.1 -> 18.2 (-8.0); start/state unchanged | |
| **`favordynmods`** | | cannot be toggled on the host safely; guest kernel 6.12.113-oar under QEMU TCG, cgroup.procs write after idle: 8.8 -> 2.3 ms (rcu_expedited: 3.5; both 2.3), n=60 | same effect as above; costs one grace period when enabled (5 to 8 ms in TCG) so the commit does it in a goroutine after the agent starts |
| spec: no masked paths / no read-only paths / neither | 40 / 150 | 24.8 vs 25.8 / 26.8 / 26.2; at n=150: 26.4 vs 25.9 (-0.5) | below noise; bound from the trace: <= 0.8 ms |
| spec: no mqueue / no sysfs / no sysfs+cgroup / no devpts / no shm / no /tmp / no pts+shm+mqueue / no resolv.conf | 40 | 24.7, 26.1, 26.4, 25.1, 25.6, 26.3, 25.3, 26.0 (base 24.8) | noise (+-2); each mount costs ~0.15 ms in runc |
| spec: "light" = no masked/ro, no mqueue, no sysfs | 150 | 26.4 -> 25.1 (-1.3) | only knob near 1 ms, and it removes hardening and /sys |
| spec: allow-all device rule (no eBPF filter) | 40 | 27.1 (+2.3, noise) | bound 0.5 ms; weakens isolation |
| `--no-new-keyring` | 60 | 25.8 vs 25.8 | none |
| `run --detach` vs create + start | 60 | 26.6 vs 25.8 + 5.4 = 31.2 (-4.6) | saves the `start` exec, as plan A5 says |
| `GOMAXPROCS=1` / `GOGC=off` for runc | 60 | 27.5 / 26.5 (worse) | none |

What a spec knob would cost the container: masked paths hide /proc/kcore, keys, sched_debug, timer_list and friends; read-only paths protect
/proc/sys, /proc/sysrq-trigger, /proc/irq, /proc/bus, /proc/fs; mqueue is the /dev/mqueue listing only (the syscalls keep working); sysfs is
what many tools read for CPU topology; /dev/pts and /dev/shm are needed by ptys and POSIX shm; /tmp as tmpfs becomes disk backed.
**No spec knob reaches 1 ms without a functional or hardening loss, so there is no spec commit.**

Committed: `rcupdate.rcu_expedited=1` on the guest command line (`internal/vm`) and `favordynmods` (`internal/agent/init_linux.go`), as separate
commits. They remove the same wait; use one (favordynmods is the more targeted, rcu_expedited helps every `synchronize_rcu` and needs no boot-time
work). CI A/B each alone with `4-start/create`; `--guest-cmdline rcupdate.rcu_expedited=0` switches the first off. Guest TCG create runs were
too noisy (IQR 100 ms) to show 5 ms; only the isolated write is conclusive.

## crun

Same bundle, same harness: create **-19.9 ms** (24.9 -> 5.0), start -3.7, state -3.2, delete -105; CPU per create 22.6 -> 5.2 ms, 443 -> 13 context
switches, 1935 -> 178 page faults. crun has no Go runtime to start twice, and it joins the cgroup at clone time (`CLONE_INTO_CGROUP`), so it never
waits for the grace period (no gain from `rcu_expedited`: 6.1 vs 6.0). It does not stack with the kernel knobs: expedited runc is 18 to 19 ms,
crun 5.

Expected gain for a hot start in the KVM guest, scaling the measured crun/runc ratios onto 19.5 / 4.6 / 4.6: about -14 (create), -3 (start), -3
(state), roughly **-20 ms of 42**. After plan items A1 to A3 remove the `state` execs the saving is about -17 ms; the 10-way concurrent start
(CPU bound on 2 vCPUs) should improve about 4x in CPU. `BACKEND=native hack/e2e.sh` **passes** with the agent pointed at crun 1.21 and 1.30.1
(hack only, not committed): guest total for a cached start 9 ms (create 5, start 1), reload 14 ms.

Compatibility checked: `--root`, create/start/state/kill/delete/exec (`--env`, dashed args, exit status), `run --detach`, stdio to a file,
`--no-pivot` (accepted even with rprivate propagation), missing entry point fails at create like runc, `checkpoint --leave-running --image-path
--work-path` and `restore --detach --bundle`: with the agent, run -> reload -> reload restores (restore 58 ms vs 96 ms with runc), HTTP via
ingress, exec and SIGTERM stop work on the restored instance.

Risks found:

1. **CRIU annotation.** The upstream static 1.30.1 refuses any spec with `org.criu.config` ("libcriu RPC config files supported in CRIU >= 4.2",
   its embedded libcriu is older); checkpoint fails, the agent falls back to normal starts. With the annotation removed checkpoint and restore work.
   crun 1.21 ignores the annotation and works. `compose.Spec` always sets it, so the spec or the crun build (libcriu >= 4.2, e.g. a distro build)
   has to change; CRIU options would then come from `CRIU_CONFIG_FILE` for both checkpoint and restore. A runc-made snapshot cannot be restored by
   crun (CRIU -52): key snapshots by runtime.
2. **Device cgroup default.** runc denies all devices except the standard list even when the spec has no rules; crun does not. In a crun container
   `mknod /dev/lc c 10 237` then open succeeds (CAP_MKNOD is in the cap list). Fix: put the default-deny rule list into the spec (+0.56 ms, create 6.05 -> 6.61).
3. **Error text.** crun 1.21 says "error opening file ...: No such file or directory" for a missing container, so the agent's `does not exist`
   matching in `State`/`Delete` fails; 1.30.1 says "container `x` does not exist" (state, kill, delete -f fine), but plain `delete` still says
   "cannot open directory". Pin the version and match both. `kill` on a stopped container: "container not running: No such process" (matches).
4. Packaging: `guest/build.sh` downloads runc; crun static is 3.6 MB (2.1 MB without systemd) vs 10.8 MB, amd64 and arm64 exist with .asc signatures.
   Not tested here: the Alpine guest, arm64/vz, CRIU 4.2 inside the guest.
5. Error and log formats differ (logrus `time=... level=error` vs plain text); only failure tails change.

## Recommendation

1. Ship the kernel knob first (cheap, -8 ms host, same mechanism in the guest kernel): A/B `rcu_expedited` and `favordynmods` separately on kvm.
2. Pursue crun: it is the largest single item (about -20 ms per start, 4x less CPU). Work list: spec devices rules, CRIU annotation handling or a
   crun built with libcriu >= 4.2, error matching, snapshot key per runtime, guest image, then a kvm/vz A/B. Exec count work (A1 to A3) still pays
   less once an exec costs 1.3 ms instead of 4.5.
3. Do not spend CI on the spec knobs; the best one combined is 1.3 ms and costs hardening. `--no-new-keyring`, GOMAXPROCS and GOGC are no-ops.
4. `run --detach` instead of create + start saves 4.6 ms with runc but only about 1.5 ms with crun.
