# Benchmark 16：Apple M3 本地实测（VZ）

> 本报告由 `hack/bench` 在本机直接运行生成，没有使用 GitHub Actions。原始数据见同目录的
> `results-vz-m3.json`（commit `eea334e`、`quick` 档位、耗时 206 s）。

- 宿主机：Apple M3，8 核，24 GiB 内存，macOS 27；guest 分配 2 vCPU、4 GiB。
- 后端：`vz`（Virtualization.framework，硬件虚拟化），guest 为 linux/arm64。
- 与 CI 的区别：CI 上的 Apple Silicon runner 没有硬件虚拟化，arm64 guest 只能跑在 QEMU/TCG
  模拟下；这里是真正的 Apple Silicon 硬件虚拟化，所以 macOS 数据比之前 README 引用的 CI
  数据快很多。Intel Mac 的嵌套虚拟化数据不再引用。
- 快照项为 —：Alpine 的 arm64 仓库没有 `criu` 包，guest 里装不上，快照会退回普通启动。
- `build.fresh` 为 n/a：macOS 的 `os.UserCacheDir` 不跟随 `XDG_CACHE_HOME`，限制条件同其他
  macOS 后端。
- 构建项受网络影响：走公网时 `cold.total` 为 40.8 s，其中闭包下载 40.6 s、基础镜像拉取与
  展平 33.8 s（两路并行）；换个时间重跑同一构建是 22.4 s。把基础镜像放进本机 registry、
  把 120 个 store path 的 NAR 缓存到本机 HTTP mirror 后，全新的空产物目录构建为
  **1.75 s**（两次 1.76 / 1.75 s，明细见 `build-nowan.json`），即去掉 WAN 下载后 oar
  自身的构建开销。


## 测试环境

| 后端 | 平台 | 宿主机 | commit | 档位 | 耗时 |
|---|---|---|---|---|---|
| vz | linux/arm64 | darwin/arm64 mb-air-m3 · Apple M3 · 8 CPU | eea334e | quick | 206 s |

读表方法：单值指标直接给出数值；多次采样的指标写成 `p50 / p99`。括号里是相对上一步基线的变化：🟢 表示改善，🔺 表示变差，≈ 表示变化不到 5%。— 表示该后端不适用，❌ 表示失败。


## 0. 环境基准（判断 runner 噪声用，与 oar 无关）

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 宿主机 fork+exec 延迟（true） | ms | 2.34 / 3.76 |  |
| 宿主机单核 CPU（sha256 64 MiB） | ms | 32.4 / 33.2 |  |
| 宿主机元数据操作（创建+stat+删除 5000 个文件） | ms | 1028 / 1048 | 5000 个 1 字节文件，在 os.TempDir() 下新建的目录里 |
| 宿主机磁盘顺序写（256 MiB + fsync） | MiB/s | 1159 |  |

## 1. 构建

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 首次在这台机器上构建（含 nixpkgs 源码获取） |  | — | darwin 上 os.UserCacheDir 不跟随 XDG_CACHE_HOME（macOS 是 ~/Library/Caches）；把 nixpkgs 源码缓存放进空目录只能改 HOME，那会连 Go 构建缓存等一起清空，所以不测 |
| 全量构建（空产物目录，源码缓存已热） | s | 40.8 | 基础镜像、Nix（求值，然后下载）、组件导入三路并发，总时间取最长的一路，所以下面各项相加会超过总计；源码缓存冷的第一次构建见“首次在这台机器上构建” |
| 　其中：基础镜像拉取与展平 | s | 33.8 |  |
| 　其中：Nix 求值 | s | 0.26 | nixpkgs 源码和求值器的编译缓存（用户缓存目录下的 oar/）已由一次不计时的构建填好，这里只是求值本身 |
| 　其中：闭包下载与 NAR→tar | s | 40.6 |  |
| 　其中：组件 build 命令（用户命令） | s | 0.27 | sources.*.build 里的命令（这里是 go build ./my-agent），不是 oar 的开销，每次构建都会运行 |
| 　其中：组件导入 | s | 0.01 |  |
| 下载的 store path / 字节 | MiB | 577 | 120 个 store path |
| 闭包 store path 数 | 个 | 120 |  |
| 闭包大小（tar） | MiB | 577 |  |
| 无变化重建 | ms | 280 | 构建器 263 ms（基础镜像 2、求值 1、下载 4、组件 261（其中用户 build 命令 251、导入 10）），其余（CLI 进程、配置、锁文件）16 ms |
| 增量构建：删除一个组件（nix:git，3 个组合） | ms | 220 / 253 | 第一个：下载 0 个 path，求值 0 个属性；构建器 242 ms（基础镜像 3、求值 0、下载 4、组件 239（其中用户 build 命令 232、导入 7）），其余（CLI 进程、配置、锁文件）12 ms | 构建器 209 ms（基础镜像 3、求值 0、下载 4、组件 206（其中用户 build 命令 199、导入 8）），其余（CLI 进程、配置、锁文件）12 ms | 构建器 197 ms（基础镜像 4、求值 0、下载 4、组件 195（其中用户 build 命令 188、导入 7）），其余（CLI 进程、配置、锁文件）13 ms |
| 增量构建：新增未求值过的包（nix:hello） | s | 2.03 | 求值 0.3s，下载 1 个 path / 0.3 MiB |
| 去重率：新增 jq 的组合实际下载 / 闭包大小 | % | 0.22 | 下载 1.3 MiB / 闭包 577.9 MiB |
| 产物目录总大小（含所有已构建组合） | MiB | 683 | bases 99 MiB, store 578 MiB, components 5.3 MiB |
| 单个 store path 大小分布 | MiB | 0.24 / 82.9 |  |

## 2. 分发与预热

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 冷预热完整组合 | s | 2.32 | 122 个产物，680 MiB，25706 个文件 |
| 　其中：上传并解包（4 路并发） | s | 2.14 |  |
| 　其中：等待 store 视图完成 | ms | 0.02 |  |
| 　其中：写回缓存盘 | ms | 160 |  |
| 冷预热吞吐 | MiB/s | 293 |  |
| 冷预热文件处理速率 | 文件/s | 11075 |  |
| 增量预热（只多一个小包的组合：nix:jq / nix:hello / nix:tree） | ms | 144 / 153 | 第一个（nix:jq）：上传 3 个产物，1.28 MiB；CLI 内：查询缺失 1.5 ms，guest 解包合计 0.9 ms，precompose 114.1 ms（视图 52.3 ms，写回 43.6 ms），CLI 总计 133.4 ms；nix:hello：上传 1 个产物 0.34 MiB（视图 60.8 ms，写回 25.9 ms）；nix:tree：上传 1 个产物 0.14 MiB（视图 58.6 ms，写回 25.1 ms） |
| 连续 reload 到相邻闭包（view 复用）：第 1 次 reload 的 store 视图 | ms | 72.5 | A = 基础 + tree，B = A + jq，C = B + hello，D = C 去掉 tree；run A，再 reload 到 B、C、D，最后回到 A，每次等被替换的实例清理完再 reload。视图时间是 agent 创建实例时建 store 视图的耗时；所需产物已在 guest 缓存里，别的场景也不启动这几个闭包。第 1 次 reload 到 B：A 还有实例（不能拆），也没有别的已启动的 view，B 整个建链；端到端 104 ms |
| 连续 reload 到相邻闭包（view 复用）：之后每次 reload 的 store 视图（到 C、D） | ms | 3.03 / 3.44 | 上一个版本已空闲：C 拆 A 的 view，D 拆 B 的 view，只链接新增的 store path；各次视图 3.4 / 3.0 ms，端到端 63.6 / 44.9 ms。没有 view 复用的版本每次都整个建链，和第 1 次一样 |
| 连续 reload 到相邻闭包（view 复用）：回到最早的闭包（A）的 store 视图 | ms | 1.59 | A 的 view 在 reload 到 C 时被拆走，现在重建（这次拆 C 的 view）；端到端 60 ms。没有 view 复用的版本里 A 的 view 一直在，这一步几乎不花时间：这是拆 view 的代价 |
| 　其中：被拆走 store path 的 view 数（steal） | 个 | 3.00 | 4 次 reload，从第 2 次起有已启动且空闲的 view 可拆 |
| 　其中：被拆之后又被请求、重新建的 view 数（rebuild） | 个 | 1.00 |  |
| guest 缓存占用（全部已预热组合） | MiB | 666 | store 561 MiB / 22982 文件，bases 100 MiB |

## 3. 虚拟机层

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 虚拟机冷启动（含首次格式化数据盘） | ms | 520 | CLI 墙钟：vm start 到 agent 可用 |
| 　其中：CLI 拉起 supervisor 进程（含 daemonize） | ms | 31.1 |  |
| 　其中：后端创建并启动虚拟机（Boot 返回） | ms | 345 |  |
| 　其中：虚拟机启动后到 agent 首次可达 | ms | 143 |  |
| 　宿主机看到 guest init 启动（VMM + 固件 + 内核解压 + 内核启动） | ms | 459 |  |
| 　其中：guest 内核 + init 到 agent 就绪 | ms | 89.4 |  |
| 　其中：VMM/宿主侧开销（总时间 − guest 时间） | ms | 430 |  |
| 　其中：guest 内核启动（内核时钟开始到运行 init，含解压 initramfs） | ms | 60.0 |  |
| 　其中：switch_root（离开 initramfs） | ms | 0.07 |  |
| 　其中：格式化数据盘 | ms | 24.1 |  |
| 控制通道往返（info，经宿主 socket → guest agent） | ms | 0.20 / 3.61 |  |
| CLI 单次命令开销（vm status） | ms | 16.7 / 47.1 |  |
| 空闲时宿主机内存占用（supervisor + VMM 进程树 RSS） | MiB | 219 | ps 采样，含 com.apple.Virtualization.VirtualMachine XPC 进程 |
| 空闲时宿主机 CPU 占用（10 秒平均） | % | 1.30 |  |
| guest 可用内存 / 总内存 | MiB | 3822 | 总内存 4004 MiB |
| 虚拟机重启（数据盘已存在） | ms | 312 |  |
| 重启后缓存仍在（无需重新预热） | 需上传产物数 | 0 |  |

## 4. 组合与启动

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 首次启动前待写回的数据（Dirty + Writeback） | MiB | 0 | 预热开始前做过一次 sync，所以这里只是预热自己写入后还没写回的数据，不含构建产物（native 的 Dirty 是宿主机全局值） |
| 首次启动该组合（端到端，CLI，紧接预热之后） | ms | 28.8 | compose 0.5 ms（其中挂载 store 视图 0.0 ms，cached=true），create 9 ms；启动前 guest 脏页 0 MiB、写回中 0 MiB，挂载 10 个，视图 5 个 |
| 首次启动该组合（脏页已写回，3 个组合） | ms | 26.1 / 31.9 | compose 0.3 / 0.5 / 0.5 ms（其中挂载 store 视图 0.0 / 0.0 / 0.0 ms），create 6.9 / 6.3 / 6.4 ms |
| 容器内挂载点数量 | 个 | 19.0 |  |
| 端到端启动（热，CLI 墙钟） | ms | 28.1 / 35.6 |  |
| guest 内启动总耗时 | ms | 8.97 / 16.0 |  |
| 　组合（模板命中） | ms | 0.46 / 1.07 |  |
| 　实例化（overlay + config.json） | ms | 0.23 / 0.30 |  |
| 　runc create | ms | 6.77 / 12.3 |  |
| 　runc start | ms | 1.52 / 3.15 |  |
| 每次启动的 runc 子进程数 | 个 | 2.00 / 2.00 | agent 在这次请求里启动的 runc 进程数（create、start、state 等） |
| 每次启动 agent 侧 CPU 时间（含 runc 子进程） | ms | 6.73 / 12.9 | agent 进程及其 runc 子进程在这次请求期间的 CPU 时间；中位数 user 4.1 + sys 2.6 ms |
| 就绪等待（my-agent 启动到监听） | ms | 8.01 / 9.84 |  |
| warm compose（CLI 端到端，验收标准 <1s） | ms | 10.8 / 13.7 |  |
| 直接 run 一个产物不在 guest 缓存的组合（命令到就绪，CLI 墙钟） | ms | 194 | CLI 上传 2 个产物 44.7 MiB（上传 141 ms，视图 0.0 ms，写回 0 ms）；guest：compose 16.2 ms（视图 15.6 ms，cached=false），create 12 ms，guest 合计 33 ms |
| 直接 reload 到多一个新包的组合（命令到就绪，CLI 墙钟） | ms | 46.3 | CLI 上传 1 个产物 6.5 MiB（上传 20 ms，视图 0.0 ms，写回 0 ms）；guest：compose 1.9 ms（视图 1.4 ms，cached=false），create 8 ms，guest 合计 13 ms |

## 5. 切换（reload）

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| overlap + SO_REUSEPORT：切换耗时（CLI 墙钟） | ms | 39.2 / 54.8 |  |
| overlap + SO_REUSEPORT：切换耗时（guest 内） | ms | 17.9 / 35.4 |  |
| overlap + SO_REUSEPORT：等待旧实例退出（StopOld，guest 内） | ms | 0.41 / 0.76 |  |
| overlap + SO_REUSEPORT：每次切换的 runc 子进程数 | 个 | 2.00 / 3.00 | agent 在这次请求里启动的 runc 进程数（create、start、state 等） |
| overlap + SO_REUSEPORT：每次切换 agent 侧 CPU 时间（含 runc 子进程） | ms | 7.48 / 17.3 | agent 进程及其 runc 子进程在这次请求期间的 CPU 时间；中位数 user 5.6 + sys 2.3 ms |
| overlap + SO_REUSEPORT：客户端失败请求（10 次切换） | 个 | 1.00 | 共 711 个请求，失败率 0.14% |
| overlap + SO_REUSEPORT：客户端两次成功响应最长间隔 | ms | 64.0 |  |
| replace：切换耗时（CLI 墙钟） | ms | 41.6 / 55.4 |  |
| replace：切换耗时（guest 内） | ms | 20.5 / 26.3 |  |
| replace：等待旧实例退出（StopOld，guest 内） | ms | 0.59 / 0.70 |  |
| replace：服务端停机窗口 | ms | 11.6 / 15.8 |  |
| replace：每次切换的 runc 子进程数 | 个 | 2.00 / 3.00 | agent 在这次请求里启动的 runc 进程数（create、start、state 等） |
| replace：每次切换 agent 侧 CPU 时间（含 runc 子进程） | ms | 8.37 / 10.8 | agent 进程及其 runc 子进程在这次请求期间的 CPU 时间；中位数 user 7.0 + sys 1.6 ms |
| replace：客户端失败请求（10 次切换） | 个 | 18.0 | 共 689 个请求，失败率 2.61% |
| replace：客户端两次成功响应最长间隔 | ms | 38.7 |  |
| overlap + 预创建（prepare）：切换耗时（CLI 墙钟） | ms | 26.4 / 48.6 |  |
| overlap + 预创建（prepare）：切换耗时（guest 内） | ms | 11.2 / 20.4 |  |
| overlap + 预创建（prepare）：等待旧实例退出（StopOld，guest 内） | ms | 0.39 / 0.93 |  |
| overlap + 预创建（prepare）：每次切换的 runc 子进程数 | 个 | 1.00 / 2.00 | agent 在这次请求里启动的 runc 进程数（create、start、state 等） |
| overlap + 预创建（prepare）：每次切换 agent 侧 CPU 时间（含 runc 子进程） | ms | 3.23 / 6.82 | agent 进程及其 runc 子进程在这次请求期间的 CPU 时间；中位数 user 2.5 + sys 0.9 ms |
| overlap + 预创建（prepare）：客户端失败请求（10 次切换） | 个 | 0 | 共 746 个请求，失败率 0.00% |
| overlap + 预创建（prepare）：客户端两次成功响应最长间隔 | ms | 21.1 |  |
| 入口代理切换：切换耗时（CLI 墙钟） | ms | 41.0 / 47.4 |  |
| 入口代理切换：切换耗时（guest 内） | ms | 15.9 / 20.0 |  |
| 入口代理切换：旧实例排空在途连接 | ms | 0.00 / 0.00 |  |
| 入口代理切换：等待旧实例退出（StopOld，guest 内） | ms | 0.42 / 1.75 |  |
| 入口代理切换：每次切换的 runc 子进程数 | 个 | 2.00 / 3.00 | agent 在这次请求里启动的 runc 进程数（create、start、state 等） |
| 入口代理切换：每次切换 agent 侧 CPU 时间（含 runc 子进程） | ms | 6.94 / 9.52 | agent 进程及其 runc 子进程在这次请求期间的 CPU 时间；中位数 user 5.2 + sys 2.8 ms |
| 入口代理切换：客户端失败请求（10 次切换，无需 SO_REUSEPORT） | 个 | 0 | 共 687 个请求，失败率 0.00% |
| 入口代理切换：客户端两次成功响应最长间隔 | ms | 13.6 |  |
| 慢启动应用（初始化 3 秒）+ 热备：切换耗时 | ms | 19.1 / 20.5 | 热备准备（prepare --start，提前进行，不计入切换）p50 3056 ms |
| 热备切换：等待旧实例退出（StopOld，guest 内） | ms | 0.61 / 0.84 |  |
| 回滚：新版本未就绪 → 拒绝并保留旧实例 | ms | 139 | reload 返回错误=true，旧实例仍在服务=true |
| 慢启动应用（初始化 3 秒）的切换耗时 | ms | 3059 / 3066 | 受应用初始化时间限制；快照（P1）的目标场景 |
| 慢退出应用（SIGTERM 后 1 秒才退出）：切换耗时（CLI 墙钟） | ms | 1054 / 1063 |  |
| 慢退出应用：切换耗时（guest 内） | ms | 1025 / 1042 |  |
| 慢退出应用：等待旧实例退出（StopOld，guest 内） | ms | 1009 / 1011 | reload 等旧实例退出后才返回，这部分就是等待旧实例退出的时间 |
| 连续切换 30 次：切换耗时分布 | ms | 28.7 / 37.5 | 前 10 次 p50 29 ms，后 10 次 p50 30 ms |
| 连续切换 30 次后的资源泄漏（挂载/cgroup/实例数变化） | 个 | 0 | 挂载 11→11，cgroup 1→1，实例 1→1，实例目录 0.0→0.0 MiB |

## 6. 运行时开销

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 开始时待写回的数据（Dirty + Writeback） | MiB | 0.07 |  |
| 文件系统：创建 5000 个小文件（overlay rootfs） | ms | 74.5 |  |
| 文件系统：stat 5000 次 | ms | 8.00 |  |
| 文件系统：chmod 5000 次（元数据写） | ms | 5.00 |  |
| 文件系统：删除 5000 个文件 | ms | 19.8 |  |
| 顺序写 256 MiB + fsync | ms | 640 |  |
| 读取整个 python store path（只读 /nix/store） | ms | 130 |  |
| 编译 Python 标准库（CPU + 文件 IO） | ms | 1326 |  |
| git：1000 个文件 init/commit/clone | ms | 170 |  |
| node 冷启动 ×5 | ms | 113 |  |
| python 启动并 import json/ssl/sqlite3 ×5 | ms | 116 |  |
| 容器 → 外网 TCP 建连（1.1.1.1:443，中位数） | ms | 1.80 | 经公网，主要反映 runner 的网络；oar 自身的网络路径见 net.local |
| 容器内 DNS 解析 | ms | 30.5 |  |
| 容器 → 宿主机 TCP 建连（虚拟网卡 + NAT，10 次） | ms | 0.17 / 1.35 | 目标 192.168.64.1 |
| page cache 共享：第二个容器读同一 store path 新增缓存 / 第一个 | % | 0 | 第一个容器 +43 MiB，第二个 +0 MiB（越低越好） |
| 宿主机 → 容器服务：HTTP 请求延迟（每次新建连接） | ms | 1.01 / 2.13 |  |
| 宿主机 → 容器服务：下载吞吐（64 MiB） | MiB/s | 1432 / 1566 |  |

## 7. 密度与扩展性

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 每个空闲容器的内存开销（含 my-agent 进程） | MiB | 6.07 |  |
| 每个容器的磁盘开销（实例目录） | KiB | 5.09 |  |
| 每个运行中容器增加的 guest 挂载数 | 个 | 1.00 |  |
| 已有 9 个容器运行时的启动延迟（最后 3 个） | ms | 24.4 / 24.6 | 前 3 个：24/23/24 ms |
| 同时启动 10 个容器：单个延迟 | ms | 121 / 133 | 全部完成 134 ms，吞吐 74.8 个/s，失败 0；guest 内中位数：总计 68 ms = compose 0.7 + 实例化 0.3 + runc create 59 + runc start 6 ms |
| 同时运行 10 个容器并删除后：宿主机上 VM 进程树内存 | MiB | 1032 |  |
| guest 已缓存 10+ 个组合后的启动延迟 | ms | 25.2 / 26.5 | guest 挂载 10 个，已挂载视图 5 个 |
| 新闭包预热（建视图）期间启动已缓存的组合 | ms | 40.9 | 预热 23 ms 内启动 1 次，最慢 41 ms |
| 　其中最慢的一次 | ms | 40.9 |  |

## 8. 正确性与隔离

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 确定性：3 种打乱顺序得到相同 digest | 通过 | ✅ |  |
| 容器只看到自己的闭包且只读 | 通过 | ✅ | /nix/store 条目 120，闭包 120，ro |
| 篡改的 bundle lock 被拒绝 | 通过 | ✅ |  |
| 校验和不匹配的产物上传被拒绝 | 通过 | ✅ |  |

## 9. 快照

| 指标 | 单位 | vz | 说明 |
|---|---|---|---|
| 打快照 |  | — | criu is not installed |
