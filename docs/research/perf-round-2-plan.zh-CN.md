# 第二轮性能优化计划（接 issue #8，基线 `13-baseline`）

> 2026-10-22。本计划基于 issue #8 的交接、`docs/benchmarks/13-baseline` 的原始数据，以及五路并行的代码审读（启动与切换路径、store view 与解包、冷构建、native 后端与测量基础设施、VM 启动与 macOS 后端）。每一条都附代码位置；写明"估计"的数字都是估计，没有实测。
>
> 方法论只有一条：**先修测量，再定位，再优化；每一项改动一次只改一个变量，用同机 A/B 验证，p50 和 p99 都看，结果锁进 CI。**

## 0. 结论速览

1. **基线里有五个数字是测量假象或与 oar 无关**（第 1 节）。不先修正，这一轮的优先级会排错：VM 冷启动里有 50–95 ms 是 CLI 自己的 100 ms 轮询；"就绪等待 30 ms"是轮询步长量化出来的；冷构建的 6.4 s "求值"几乎全是首次拉 nixpkgs 源码树；"组件构建 4 s"是 bench 自己的 `go build`；native 比 kvm 慢 2–3 倍是 runner 的 CPU 型号不同。
2. **真正的热点在"边界"上**：每次启动 exec 三次 runc（create、start、再加一次 `runc state`），每次切换再加两次；在 kvm 上一次 exec 约 4.6 ms，在 vz 上约 100 ms，在 qemu/TCG 上约 250 ms。减少 exec 次数是跨后端、把握最大、成本最低的一项（第 3.A 节）。
3. **warm 的瓶颈是 CPU 不是 I/O**：view 建链和解包在 guest 里是 90% sys CPU、零块 I/O；2 vCPU 下 cold warm 426 MiB/s 就是 CPU 上限。方向是少做（view 复用、delta overlay）和少分配（解包 buffer 复用），不是换存储格式（第 3.C 节）。
4. **冷构建里 nix 求值本身只有 0.2–0.3 s**，其余是源码树下载与解包、iets 编译缓存冷。方向是让"首次在这台机器上构建"变快，或者干脆不需要源码树（第 3.D 节）。
5. **macOS 的 CI 数字主要度量嵌套虚拟化和 TCG**，真机从未测过。vz/qemu 相关优化的价值在拿到一次真机基线之前无法判断（第 3.E 节）。

## 1. 第 0 步：先修正测量（必做，否则后面的 A/B 都不可信）

| # | 问题 | 证据 | 受影响的指标 | 修法 | 验证 |
|---|---|---|---|---|---|
| 1.1 | `oar vm start` 拉起 supervisor 后每 100 ms 轮询一次 `state.json` | `internal/cli/vmcmd.go:238`；历史上所有 `boot.cold` 都是 100 的整数倍加 8–16 ms（kvm 1013/708/510/409，native 116） | VM 冷启动、重启、"CLI 拉起 supervisor" | supervisor 就绪后通过继承的管道通知父进程（或退而求其次：轮询间隔改 5 ms） | kvm 409 → 约 355 ms，native 116 → 约 20 ms。**这是测量修正，不是优化**，报告里要单独说明 |
| 1.2 | 就绪等待每轮 = 探测 + 一次 `runc state` 子进程（约 4.6 ms）+ 10 ms sleep，结果量化到 14.6 ms 的整数倍 | `internal/agent/agent_linux.go:976,985`；14 步 kvm/native 的 `ready` 中位数里 24/28 落在 2 轮，其余落在 1 轮 | 就绪等待、热启动、所有切换、replace 停机窗口 | 见 3.A2 | 用 `MY_AGENT_INIT_DELAY` 扫 0–30 ms：修正前呈 14.6 ms 台阶，修正后应线性 |
| 1.3 | "Nix 求值 6.4 s"几乎全是首次在这台机器上拉 nixpkgs 源码树（GitHub tarball、单 goroutine 解 5.3 万个文件、iets 编译缓存冷）；真正求值 0.2–0.3 s。同机 A/B 时（缓存已热）kvm 冷构建只有 1.6 s | `internal/nixeval/source.go:77-186` 在 Eval 计时之内；`13-baseline/README.md` 第 11 行 vs 第 2 节 | 冷构建、"其中：Nix 求值" | bench 把"首次在这台机器上构建"（清空 `os.UserCacheDir()/oar`）和"产物目录为空、源码缓存热"拆成两个指标；nix-eval 子进程 stderr 里的源码计时（`source.go:78,114`）收进 `build --json` 的 stats | 两个指标分别有数；后者应接近 1.6 s |
| 1.4 | "组件构建与导入 4 s"是 bench 自己的 `go build ./my-agent`（CI 换到 Go 1.26 和 ubuntu-26.04 后缓存冷），而且三路并发后它不在关键路径上 | `internal/builder/files.go:27-55`（每次 build 都无条件跑用户 build 命令），`hack/bench/run.go:296-297` | "其中：组件构建与导入" | stats 里把用户 build 命令（`files.go:45`）和导入分开计时；bench 的报告注明关键路径是 Eval + Fetch | 导入部分应回到 < 100 ms |
| 1.5 | native 与 kvm 跨 CPU 型号比较：native 跑在 EPYC 7763，kvm 跑在 Xeon 6973P-C；同 CPU 时两者在 0.9–1.2 倍之间。0-env 的 sha256 校准（42 vs 39 ms）看不出差异 | `13-baseline/README.md:43-44`；14 步 `fs.create` 按 CPU 分组：EPYC 上 native 437 / kvm 364，Xeon 上 131 / 109–133 | 6-runtime 全组、以及任何跨 runner 的百分比 | ① `Result.Host` 已含 CPU 型号，`report.go` 解析后：CPU 不同就不打 🟢/🔺，改标"CPU 不同"；② 0-env 加元数据型校准（宿主机 tmpfs 上创建/stat/删除 5000 个文件、fork+exec 100 次）；③ native 后端忽略 `--cpus 2 --memory 4096`（`internal/vm/native_linux.go:44-69`），并发与密度指标不可比：给 native 的 agent 套一个 cgroup，或报告里注明 | 重算第 13 步的 native/kvm 差异，应消失 |
| 1.6 | "首次启动前待写回 1145 MiB"是宿主机全局 Dirty，包含刚构建出来的 638 MiB 产物 | `internal/agent/stats_linux.go:90-110` | 4-start 的脏页说明 | warm 之前先 `stats Sync:true`，或只统计缓存目录 | — |
| 1.7 | `boot-profile` 工作流的 `initcall_debug` 配了 `loglevel=8`，串口打印把启动拖到 2–4 s，数据不可用 | 5 次历史 run | 内核启动分析 | agent 监听后再 dump `/dev/kmsg`，不提高 loglevel | 单次启动时间回到 ~400 ms |

另外两个会影响判断的事实：局部 `main` 指针过期（63d1e0e），`origin/main` 才是 4d5dda9；bench 不隔离 nix 源码缓存目录（`hack/bench/run.go:182-185`），同一台 runner 上只有第一次构建付源码树的钱。

## 2. 测量基础设施（这一轮的护栏，和第 1 节一起做）

**2.1 PR 级同机 A/B 门禁。** 把 `bench.yml` 的 `bench-ab` 改成 `workflow_call`，PR 打 `perf` 标签时跑 `backends=native,kvm`、`base=<PR base sha>`、`rounds=2–3`、quick（每侧约 2 min，总计 10–14 min）。去掉 A/B 模式的 `-no-ingress`（它丢掉了入口代理、热备和快照指标）。新增 `go run ./hack/bench compare -base 'ab-*/base-*.json' -head 'ab-*/head-*.json' -fail-on 10%`：每侧取 p50 的中位数，只看 n ≥ 5 的指标，劣化 > 10% 且 > 3 ms 并且每一轮都成立才 fail；门禁集合 `4-start/e2e`、`4-start/create`、`5-switch/*.wall`、`3-vm/boot.cold`、`9-snapshot/restore`，cold warm 用 20%；`cpu.compileall` 两侧漂移 > 5% 则本轮判为"不可判定"而不是失败；任何 `status: fail` 直接失败；`report`/`run` 在失败时退出码非零（现在恒为 0，`hack/bench/main.go:74-81`）。

**2.2 agent 内的 profiling 钩子。** 现在没有任何 pprof、trace、perf 或 `strace -c`（仅 `reportSlow` 的 goroutine dump 和启动时间线）。加一个 `debug.pprof` 协议操作（cpu/heap/trace，带秒数）和隐藏子命令 `oar vm pprof --cpu 10s`，走控制通道，对四个后端一致；同时每个 `ComposeResult`/`RunResult` 带 agent 侧的 rusage 差值（utime、stime、minflt/majflt）和 PSI。CPU 时间比墙钟稳定得多，能直接区分"CPU 受限"和"在等"。符合"不加环境变量"的项目约定。

**2.3 启动路径的 exec 计数指标。** agent 里给每次 runc exec 计数，随 `RunResult` 返回，bench 作为一个指标记录（现在热启动是 3 次、切换 5 次）。这是启动路径的"系统调用计数"式护栏，不受 runner 噪声影响。

**2.4 n = 1 的指标补样本。** 冷构建、cold warm、增量 warm、首次启动现在只有一个样本，进不了门禁；增量 warm 可以换三个不同的小包各测一次，首次启动已有 `first.settled`（3 个组合）可用。

## 3. 优化假设（按 预期收益 × 把握 ÷ 成本 排序）

每条给出：现状（代码位置）、假设、预期收益、做法、验证、风险。"估计"就是估计。

### 3.A 启动与切换路径：减少 runc exec（所有后端受益，把握最高）

现状：kvm 热启动 42.2 ms = `runc create` 19.5 + `runc start` 4.6 + 结尾 `runc state` 4.6 + CLI 进程约 4.3 + 三次往返约 1.8 + compose/实例化 1.2 + 约 6 ms 未归因。vz 上 create 109 / start 29，qemu 上 281 / 53。10 个并发启动时 create 从 19.5 涨到 117 ms，是 2 vCPU 上的 CPU 受限（三次 exec 约 28.7 ms CPU），不是锁（`Server.mu` 只读锁、按名互斥、`groups.mu` 热路径只是 map 查找）。

| # | 做法 | 现状 | 预期收益（估计） | 验证 | 风险 |
|---|---|---|---|---|---|
| A1 | 去掉 run/reload 结尾的 `observe`（一次 `runc state`），pid 从 create 时 runc 写的 state.json 读，或 create 后记一次 | `agent_linux.go:772`、`:1077-1086` | kvm −4.6 ms/次（42 → 37），vz 约 −100 ms，qemu 约 −250 ms | 2.3 的 exec 计数从 3 降到 2；A/B `4-start/e2e` | 返回的状态可能不是"刚刚观测"的；需要 `pidfd` 判活替代 |
| A2 | 就绪循环：sleep 10 → 1 ms；判活不再 exec `runc state`，改为对容器 init pid 的 `pidfd_open` + poll；`log` 方式用 inotify 看 console.log | `agent_linux.go:944-990` | 每次启动/切换 −1 到 −15 ms；replace 停机窗口（45.7 ms）同量减少 | 1.2 的台阶扫描；`ready` 应等于应用真实就绪时间（示例应用在监听前串行跑 `node`、`python3`、`git --version`，`examples/agent-runtime/my-agent/main.go:55-63`） | 1 ms 轮询在 TCP 探测上会多发连接；可用 pidfd 退出事件兜底 |
| A3 | stopOld 直接 `kill(2)` 容器 init pid + pidfd 等待，替代 `runc kill` + `runc state` 轮询（50 ms sleep 从未命中，但两次 exec 的成本在） | `agent_linux.go:661-662`、`runc_linux.go:210-222`；`exp/stop-old` 的 pidfd 版仍保留了 `runc kill` 和 `State()`，没解决 | 每次切换 −8 到 −9 ms；热备切换 33 → 约 24 ms；vz/qemu 上 −100 到 −400 ms | A/B `5-switch/*.wall`、StopOld 子项 | 信号语义要和 runc 的 `kill --all`（进程组）一致；容器 init 进程 pid 复用用 pidfd 避免 |
| A4 | `runc create` 19.5 ms（占 46%）的成分未知：**先 profile**（2.2 的钩子 + native 上 `strace -f -tt -T`；`diag-c` 分支有采样器 `internal/agent/diag_linux.go`），再做两步：① 便宜旋钮 A/B：减少 10 个 masked 和 5 个 readonly 路径、不挂 mqueue/sysfs、cgroup2 `favordynmods`、`rcupdate.rcu_expedited=1`（`internal/compose/compose.go:279-300`、`init_linux.go:58`）；② 评估 **crun** 替代 guest 内的 runc 1.5.2（C 实现，create+start 通常比 runc 快一倍以上，支持 CRIU） | `runc_linux.go:55,146`；spec 10 个挂载、5 个命名空间、无 seccomp/AppArmor/hooks | ① 3–10 ms；② 可能 −10 ms（估计，未测） | ① 逐项同机 A/B；② 用同一 bundle 直接比 `runc create` vs `crun create` 的墙钟与 CPU | ② 快照/恢复、`--no-pivot`、console 日志格式、静态链接体积都要重新验收；这是本轮最大的单项，也是最需要先测的 |
| A5 | 用 `runc run --detach` 替代 create + start（只在非 prepare、非 replace 路径） | `agent_linux.go:806,727` | 约 −4 ms/次 | exec 计数 | 与热备/预创建路径分叉，代码复杂度上升 |
| A6 | CLI 热路径：跳过 `info` 和 `warm.status`（compose 已报告缺失产物和 arch，`compose.go:130,142`）；模板命中时只发 digest，而不是每次发约 100 KB 的 lock JSON 再由 agent 重新 hash（`compose.go:126`） | `internal/cli/runcmd.go:153,421`、`vmcmd.go:346` | −2 到 −5 ms | 记录 `r.Total` 与 `r.Warm.Status`；`wc -c` lock | 协议版本要同步 |

A1–A3 合计的估计：kvm 热启动 42 → 约 30 ms，replace 切换 84 → 约 65 ms，热备切换 33 → 约 22 ms；10 并发按 exec 数减少等比例改善；vz/qemu 的绝对收益更大。

### 3.B VM 启动与探测（一次性成本，但 vz/qemu 上是秒级）

| # | 做法 | 现状 | 预期收益（估计） | 验证 |
|---|---|---|---|---|
| B1 | 见 1.1（100 ms 轮询） | `vmcmd.go:238` | kvm −55、native −95、macOS −50 ms | — |
| B2 | 第一个 `info` 不等 `runc --version`（`runcVerOnce` 让它阻塞；在 TCG/嵌套 guest 里 exec 一个 10 MiB 静态二进制要 100–200 ms）：后台算，info 先返回；探测间隔 20 → 2 ms，或由 console marker 唤醒 | `agent_linux.go:106-112,342`、`internal/vm/vm.go:248-255,294` | qemu −150 到 −190 ms，vz ≤ −100，kvm −5 到 −10 | `boot.detect` 指标 |
| B3 | mkfs 移出关键路径：agent 先监听，`New()` 里需要数据盘的部分等 mkfs 完成（仅冷启动） | `init_linux.go:244`、`agent_linux.go:82-87` | kvm −47、qemu −486、vz −824 ms（冷启动） | `boot.mkfs` 子项 |
| B4 | 内核裁剪：关 `DEBUG_*`、`JITTERENTROPY`（`jent_mod_init` 7–12 ms）、E1000、HID/I8042、SELinux/AUDIT、RAPL、VGA；加 `nokaslr`。先用 1.7 修好的 boot-profile 拿到干净的 initcall 数据 | CI 的 `kernel.config` | −20 到 −50 ms（估计） | A/B `boot.kernel` |
| B5 | init 里 `ip route` 的 exec 改 netlink；mount、网络、agent 拉起并行 | `init_linux.go:219,123` | 5–15 ms | `/run/oar/boot.json` 的时间线 |
| B6 | PVH `vmlinux` 直接启动 / qboot 固件：对比 Cloud Hypervisor 的数据，上限约 20 ms | `guest/build-kernel.sh:32-35` 已经产出 vmlinux | ≤ 20 ms | 低优先级 |
| B7 | guest 时钟开始之前的 110 ms（kvm）/ 6.9 s（vz）无法细分：加 `Linux version` marker，扫 `--memory 512…4096` 看是否随内存线性（memmap/页表初始化） | — | 未知；若线性，默认内存减半或开 `DEFERRED_STRUCT_PAGE_INIT` | 扫描曲线 |

### 3.C warm 与 store view（少做、少分配）

现状：view 建链每文件 1 次 `linkat`、每符号链接 2 次、每目录 9 次系统调用（`storeview_linux.go:170-224`），4 个 worker（`min(NumCPU*2, n)`，bench 是 2 vCPU）；本机 ext4 上 link 4.7–5.9 µs、mkdir 14–15 µs，90% sys CPU、零块 I/O；1/2/4/8 worker = 150/85/57/36 ms。整个建链过程持有全局 `groups.mu`（`storeview_linux.go:41-42`）。

| # | 做法 | 现状 | 预期收益（估计） | 验证 | 风险 |
|---|---|---|---|---|---|
| C1 | **view 复用，donor 限定为"已启动且当前空闲"的 view**，加相似度门槛（共享 ≥ 50% 路径、≥ 8 个），并在 stats 里记 steal/rebuild 计数。issue 里的 `.used` 想法要反过来理解：如果 donor = "从未启动"，受害者恰好是 warm 预建的那些 view，首次启动照样慢；donor = "已启动且空闲"才同时保住首次启动，代价是 run→reload 循环里第一次 reload 拿不到 live 的 v1，从第二次开始受益 | `perf/view-reuse` 的 patch 可直接 apply 到 HEAD（只有偏移）；标记写在 `createInstance`（`agent_linux.go:784`）和 `instantiateSeeded`（`:1020`）之前，覆盖 prepare 和 restore；若与 `exp/view-lock` 同时上，标记和 steal 必须共用索引锁并跳过 in-flight 的 view | view −90%：kvm 130 → 约 12 ms，vz 1170 → 约 100，qemu 2412 → 约 200 | 新增 run→reload×3 场景；`first.settled` 必须不变 | 没有 view GC，持久标记永不清除（用实例目录标记则会清）；一个 view 服务多个 bundle digest，标记按闭包 |
| C2 | **delta overlay view**：以一个完整 view 为底层，新闭包只建"新增路径"的小目录 + 白名单删除，`lowerdir=delta:base` 一次 overlay 挂载。O(变化量)，不拆任何 view，对 direct run/reload（那里 view 占 50–85%：kvm 196/382、181/243 ms）同样有效 | 今天 `/nix/store` 由 runc 按 config.json 做 bind（`compose.go:306-309`）；`mountOverlay` 已支持 `lowerdir+`（`storeview_linux.go:253-296`） | 同 C1，且覆盖 C1 覆盖不了的场景 | 需要 root 做 `runc create` A/B：每次启动多一个 overlay 挂载的代价（第 7 步 120 个 bind 曾让 create 15 → 28 ms） | CRIU 解析 mountinfo；overlay 顶层查找要探 2 层 |
| C3 | view 建链期间暂停后台 `syncfs` 循环（kvm/qemu 开着，本地测得它让建链慢 29–46%，噪声大） | `storeview_linux.go:540-567`、`compress.go:35-37` | kvm/qemu 增量 warm −10 到 −30% | 同机 A/B | 结尾刷盘变长，要看总和 |
| C4 | 合并 `exp/view-lock`（全局锁只保护索引，建链在锁外） | `storeview_linux.go:41`，分支 8187916 | 不改单次 warm 时间；改善"新闭包 warm 期间启动已缓存组合"的长尾（现在最慢 50 ms） | A/B 该指标的 p99 | 与 C1 的标记逻辑交互 |
| C5 | 解包：复用 128 KiB buffer（现在每个文件新分配 32 KiB，22.4k 文件共 581 MB）、原始 fd 不走 `os.NewFile`（省一次 fcntl）、`futimens` 代替 `/proc/self/fd/N` 的 `utimensat` | `extract_linux.go:222,224,312,374` | 解包 −25 到 −30%（agent 原型实测：tmpfs 305–345 → 211–230 ms，分配 581 → 36 MB）；cold warm 在 kvm 上是 CPU 上限，直接变成吞吐 | A/B `2-warm/cold`、CPU 时间 | 低 |
| C6 | 建链按 store path 大小降序而不是按名字（`compose.go:221-233`） | — | 并行尾部更短，约 1.2–1.5× | 本地 benchmark | 低 |
| C7 | 数据盘 mkfs 加 `-O ^metadata_csum`（guest 内核没有 crc32c 加速） | `init_linux.go:277` | 5–15%（未测） | A/B | 掉电一致性本来就不保证 |
| C8 | native 增量 warm 381 ms vs kvm 130 ms（CPU 还多一倍）：假设是宿主机脏页写回竞争 | `compress.go:32`（native 不刷盘） | 若成立，可降到约 130 ms | 增量 warm 前对缓存目录 `syncfs` 看差异；PSI | 若成立，native 的"不刷盘"取舍要改成"只在增量 warm 前刷缓存目录" |
| C9 | issue 第 7 条（vz 增量 warm 慢 20%）：vz 上分配密集型操作慢得离谱（创建 5000 文件 37×，而 chmod 只 2.5×），符合嵌套虚拟化下首次触碰页面的 EPT fault；建链要分配 2.2 万 dentry、2.9 千 inode | — | 缓解 = 少分配（C1/C2） | 在 vz guest 里连续建两次同一 view，第二次明显快就是首次触碰成本 | — |

### 3.D 冷构建（让"首次在这台机器上构建"变快）

| # | 做法 | 现状 | 预期收益（估计） | 验证 |
|---|---|---|---|---|
| D1 | nixpkgs 源码树：按目录分片并行解包、klauspost gzip；更彻底的是不解到磁盘（取决于 iets 能否直接读 tarball，不在本仓库内，需查） | `source.go:100,121-186` 单 goroutine、stdlib gzip | 首次构建 −3 到 −5 s | `XDG_CACHE_HOME=$(mktemp -d) oar nix-eval hello` 读 stderr 的两行计时；`curl -w` 看 GitHub tarball 的 TTFB 和速率 |
| D2 | 为默认 pin（`8ce7336…`，nixos-26.05，`internal/spec/spec.go:19-21`）预生成 `nix-eval.json`：CI 用 `oar nix-eval` 生成两个 system 的表，按 hash pin，随 release 分发。默认 rev 的常见属性不再需要源码树；`@rev` 自定义 pin 仍走 D1。`packages.json.br` 不含 store path，不能替代 | `internal/builder/nix.go:40-63` | 默认 rev 上求值与源码树一起归零 | 首次构建指标 |
| D3 | `resolveBaseDigest` 串行在三路并发之前；nixpkgs 下载在 `Build` 一开始就发起 | `builder.go:208`、`base.go:59-71` | −0.1 到 −0.4 s | stats 时间线 |
| D4 | base 1.56 s：stdlib gzip → klauspost pgzip，两遍 tar 合一，缓冲写 | `base.go:103-165` | −0.3 到 −0.6 s（D1 之后才在关键路径上） | 拆分 `remote.Image` 与拷贝循环的计时 |
| D5 | 组件 build 命令每次都跑；无变化重建的 64 ms 里约 45 ms 是 `go build` no-op | `files.go:27-55` | ≤ 45 ms；需要声明输入才能跳过 | 低优先级 |
| D6 | iets `--workers`、Rust 库 LTO/PGO | 求值本身只有 0.2–0.3 s | 上限 0.5 s | 放最后 |

### 3.E macOS 后端：先拿真机基线，再谈优化

| # | 做法 | 说明 |
|---|---|---|
| E1 | **在一台真机 Apple Silicon 上跑一次完整 bench**（vz 走原生虚拟化，qemu 走 HVF），存为 `docs/benchmarks/<NN>-apple-silicon/` | 没有它，vz 冷启动 10 s（6.9 s 在 guest 时钟之前，嵌套虚拟化）、host→container 13 ms / 51 MiB/s 这些数字的优化价值无法判断 |
| E2 | vz 宿主机访问：直连 vmnet（HTTP 请求 = 握手 + 请求，两次 vmnet 往返）vs 走控制通道，一行改动 A/B（`vz_darwin.go:222` 的 `GuestNetwork()` 返回 false） | 第 8 步的结论是在网段冲突下测的；`OAR_VZ_FORWARD=channel` 已在 63d1e0e 删除，`08-port-forwarding/README.md:16` 的文档过期 |
| E3 | qemu on macOS 的 slirp 不换：`vmnet` 需要 root 或 entitlement，`passt` 仅 Linux，而第 8 步已证明瓶颈是 TCG | 不投入 |
| E4 | vz 的 `Boot()` 0.7 s 混了 vmnet 创建和 `Start`，拆开计时 | 低成本 |

### 3.F 内存与密度（低优先级，列出来是为了不再重复评估）

第 9 步的 free page reporting 之所以"无效"，是因为 guest 留住的是 page cache，不是空闲页。若有密度需求：idle 时 guest `drop_caches` + `virtio-balloon-pci,free-page-reporting=on`（guest 内核已开 `PAGE_REPORTING`），看 `vm.rss.after`。不在本轮。

### 3.G 编译器层（免费但小）

Go 1.21+ 支持 PGO：用 2.2 的钩子在 bench 下采 agent 的 CPU profile，`go build -pgo` 构建 oar-agent 与 oar，典型 2–7%。做完 3.A/3.C 之后再做，否则 profile 会过期。

## 4. 执行顺序

| 阶段 | 内容 | 产出 | 验证方式 |
|---|---|---|---|
| 0 测量修正 | 1.1–1.7、2.1–2.4 | `docs/benchmarks/14-measurement/`：修正后的完整 bench，报告里逐项说明哪些"变好"只是测量修正 | 完整 bench ×1；1.1/1.2 的量化消失 |
| 1 启动路径 | A1、A2、A3、B2、B3 | 每项一个分支、一次同机 A/B（native,kvm 各 3 轮；vz/qemu 两种顺序各一次） | exec 计数 3 → 2（启动）、5 → 2（切换）；`4-start/e2e`、`5-switch/*.wall` |
| 2 warm | C5、C3、C6，然后 C1（含 run→reload×3 新场景）；C2 做原型并只测 `runc create` 代价，决定是否替代 C1 | 同上 | `2-warm/*`、`first.settled` 不变、长尾 p99 |
| 3 冷构建 | D1、D3、D2 | 同上 | 1.3 拆出的两个指标 |
| 4 探索 | A4（profile → 旋钮 → crun 评估）、B4、E1 真机基线 | 评估报告，采用与否按数据 | — |

执行规则（来自上一轮的教训）：一次只改一个变量；优化后回到 profile 确认热点消失而不是搬家；平均改善但 p99 恶化的不合并；每个取舍留 `// PERF:` 注释写清楚为什么；每步报告进 `docs/benchmarks/NN-*/`，`-baseline` 指向上一步。

## 5. 明确不做 / 不再评估

- EROFS 只读镜像、Cloud Hypervisor、virtio-balloon 回收：第 9 步已实测并由你确认。
- ext4 去 journal：`git clone` 出错。
- view 复用时 donor 为"任意未挂载的 view"：已证明拖慢首次启动（本轮的 C1 改了 donor 定义）。
- native 改 loop 挂载的数据盘：前提（native 文件系统慢）不成立，是 CPU 型号差异。
- qemu on macOS 换网络栈：瓶颈是 TCG。
- 用 `packages.json.br` 跳过求值：不含 store path。
- 宿主机到 guest 的数据面改 splice/零拷贝：kvm 上已是 1980 MiB/s，不是瓶颈。

## 6. 分支

- 可删（已合并）：issue #8 第 6 节列出的八个。
- `perf/view-reuse`：C1 的基础，patch 可直接 apply 到 HEAD。
- `exp/view-lock`：C4，未合并。
- `exp/stop-old`：被 A3 取代（它仍保留 `runc kill` 和 `State()`），可删。
- `exp/direct-run`、`exp/writeback-lifecycle`、`bench-experiments`、`nix-executor-concurrency`：按 patch-id 各有 1–3 个提交不在 main，但对应功能（direct.run 指标、流式闭包下载 #7）已以 squash 形式合并；逐个 `git diff main...<branch>` 确认后删。
- `diag-c`：`internal/agent/diag_linux.go` 的采样器对 A4 有用，不要删。
