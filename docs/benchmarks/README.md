# Benchmark 报告

每一步优化都用同一套 benchmark（`hack/bench`，CI 工作流 `bench.yml`）在 native、KVM、VZ（Intel Mac）、QEMU（Apple Silicon，TCG 模拟）四个后端上重跑，并和上一步对比。第 16 步改为在本机 Apple M3 上直接实测，不再经过 CI。

| 步骤 | 报告 | 内容 |
|---|---|---|
| 0 | [基线](00-baseline/README.md) | 测量工具与基线 |
| 1 | [并发、预组合与预创建](01-concurrency-prepare/README.md) | 按容器名加锁、warm 时预组合、`oar prepare`、启动探测 |
| 2 | [控制通道](02-control-channel/README.md) | 缓存 runc 版本、guest 连接池、VZ 磁盘模式、环境基准 |
| 3 | [入口代理与热备](03-ingress-standby/README.md) | 私有端口 + 入口代理、`prepare --start` 热备、宿主机端口转发、产物按内容寻址 |
| 4 | [应用快照](04-snapshots/README.md) | CRIU 快照与恢复（`process.snapshot`）、guest switch_root（pivot_root）、CRIU 进 guest 镜像 |
| 5 | [store 视图改为硬链接树](05-store-views/README.md) | issue #2 的根因：视图挂载不再常驻 agent 命名空间；按闭包缓存视图（vz 回归待查） |
| 6 | [VM 冷启动](06-vm-boot/README.md) | 启动阶段拆分、amd64 改用 microvm、LZ4 内核与 initramfs、switch_root 不复制、去掉固定等待 |
| 7 | [warm 刷盘与就绪通知](07-warm-flush/README.md) | warm 结束前刷盘（首次启动 784 → 40 ms）、microvm 仅限 KVM、qemu 静态网络与就绪通知、store 视图同机 A/B（`ab-store-view/`） |
| 8 | [端口转发](08-port-forwarding/README.md) | qemu 由 QEMU 自己转发、vz 直连 guest 地址（P1-3）；native 也在 warm 结束前刷盘 |
| 9 | [剩余路线图评估](09-evaluations/README.md) | Cloud Hypervisor、只读 EROFS 镜像实测后不采用（你确认），内存回收实测无效；路线图完成情况汇总 |
| 10 | [vz 连接中断](10-vz-connection/README.md) | 宿主机 socket 代理重试并记录连接失败（vz 偶发 EOF 的根因）、连接池存活检查、失败时保存 VM 日志 |
| — | [总结：优化前后对比](11-final/README.md) | 合并 PR #3 后的完整 benchmark，括号内相对第 0 步基线 |
| 12 | [warm 加速](12-warm/README.md) | 刷盘按后端区分、上传期间后台写回、解包少系统调用、上传时建视图、qemu 上压缩传输；每项同机 A/B |
| 13 | [**新基线**](13-baseline/README.md) | 构建三路并行、guest 不校验 sha256、解包和建 view 改用目录 fd 系统调用、数据盘不保证持久化、vz 改用 vmnet 网络（修好 CI 上 vz 的"挂起"）；之后的优化以这里为基线 |
| 14 | [第二轮优化](14-round2/README.md) | 测量修正（CLI 轮询、就绪轮询量化、机器首次构建、按 CPU 型号判定、runc 子进程数）；启动路径少 exec runc、pidfd 停止；VM 启动探测；guest 内核裁剪与 `rcu_expedited`；解包缓冲复用；nixpkgs 源码流水线解包；view 复用（只拆已启动且空闲的） |
| 15 | [main](15-main/README.md) | 第二轮优化合并后 main 的完整运行；README 性能表 Linux 列的数据来源 |
| 16 | [**Apple M3 本地实测**](16-local-m3/README.md) | 不用 CI，在本机 Apple M3 上跑 `quick`（VZ，硬件虚拟化）；README 性能表 macOS 列的数据来源 |

复现方法：

```console
$ hack/build.sh
$ go run ./hack/bench run -backend native -out results-native.json
$ go run ./hack/bench report -title "..." -baseline docs/benchmarks/00-baseline -out report.md results-*.json
```
