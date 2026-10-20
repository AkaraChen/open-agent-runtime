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

复现方法：

```console
$ hack/build.sh
$ go run ./hack/bench run -backend native -out results-native.json
$ go run ./hack/bench report -title "..." -baseline docs/benchmarks/00-baseline -out report.md results-*.json
```
