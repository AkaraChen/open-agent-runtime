# Benchmark 报告

每一步优化都用同一套 benchmark（`hack/bench`，CI 工作流 `bench.yml`）在 native、KVM、VZ（Intel Mac）、QEMU（Apple Silicon，TCG 模拟）四个后端上重跑，并和上一步对比。

| 步骤 | 报告 | 内容 |
|---|---|---|
| 0 | [基线](00-baseline/README.md) | 测量工具与基线 |
| 1 | [并发、预组合与预创建](01-concurrency-prepare/README.md) | 按容器名加锁、warm 时预组合、`oar prepare`、启动探测 |
| 2 | [控制通道](02-control-channel/README.md) | 缓存 runc 版本、guest 连接池、VZ 磁盘模式、环境基准 |

复现方法：

```console
$ hack/build.sh
$ go run ./hack/bench run -backend native -out results-native.json
$ go run ./hack/bench report -title "..." -baseline docs/benchmarks/00-baseline -out report.md results-*.json
```
