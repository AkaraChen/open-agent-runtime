# Benchmark 报告

每一步优化都用同一套 benchmark（`hack/bench`，CI 工作流 `bench.yml`）在 native、KVM、VZ（Intel Mac）、QEMU（Apple Silicon，TCG 模拟）四个后端上重跑，并和上一步对比。

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

复现方法：

```console
$ hack/build.sh
$ go run ./hack/bench run -backend native -out results-native.json
$ go run ./hack/bench report -title "..." -baseline docs/benchmarks/00-baseline -out report.md results-*.json
```
