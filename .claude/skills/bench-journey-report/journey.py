#!/usr/bin/env python3
"""Print the numbers for a user-journey benchmark report.

usage: journey.py BASELINE_DIR RESULTS_DIR [--backends native,kvm,vz-intel,qemu-arm64]

Both directories hold results-<backend>.json files written by hack/bench
(BASELINE_DIR is usually docs/benchmarks/00-baseline). For each step of
the journey it prints "before -> after" per backend, then the runner
calibration (0-env) so noisy comparisons can be called out.
"""
import json
import os
import sys

STEPS = [
    ("1. oar build", [
        ("1-build", "cold.total", "冷构建"),
        ("1-build", "incr.addnew", "加一个包后重建"),
        ("1-build", "noop", "无变化重建"),
    ]),
    ("2. oar vm start", [
        ("3-vm", "boot.cold", "VM 冷启动"),
        ("3-vm", "boot.warm", "VM 重启（数据盘已在）"),
        ("3-vm", "rtt", "控制通道往返"),
        ("3-vm", "rtt.cli", "CLI 单条命令"),
    ]),
    ("3. oar warm", [
        ("2-warm", "cold", "冷 warm 整个组合"),
        ("2-warm", "cold.upload", "  其中上传并解包"),
        ("2-warm", "cold.view", "  其中等 store 视图"),
        ("2-warm", "cold.flush", "  其中写回缓存盘"),
        ("2-warm", "cold.wire", "  实际传输（压缩后）"),
        ("2-warm", "incr", "增量 warm（新闭包）"),
    ]),
    ("4. oar run", [
        ("4-start", "first.total", "首次启动（刚 warm 完）"),
        ("4-start", "e2e", "每次启动"),
        ("4-start", "dirty", "首次启动前待写回"),
        ("7-density", "concurrent", "10 个并发：单个"),
        ("7-density", "bundles", "多个组合轮流启动"),
    ]),
    ("5. 改配置，线上切换", [
        ("5-switch", "replace.wall", "replace 切换"),
        ("5-switch", "replace.failed", "  replace 丢请求数"),
        ("5-switch", "ingress.wall", "入口代理切换"),
        ("5-switch", "ingress.failed", "  入口代理丢请求数"),
        ("5-switch", "prepared.wall", "预创建后切换"),
        ("5-switch", "rollback", "回滚"),
    ]),
    ("6. 慢启动应用（初始化 3 s）", [
        ("5-switch", "slow", "不用热备/快照的切换"),
        ("5-switch", "standby.switch", "热备切换"),
        ("9-snapshot", "take", "打快照（一次性）"),
        ("9-snapshot", "cold", "从快照冷启动"),
        ("9-snapshot", "restore", "  其中 runc restore"),
    ]),
    ("7. 宿主机访问容器服务", [
        ("6-runtime", "host.latency", "请求延迟"),
        ("6-runtime", "host.tput", "下载吞吐"),
        ("6-runtime", "net.local", "容器 → 宿主机建连"),
    ]),
    ("8. 运行时开销", [
        ("6-runtime", "cpu.compileall", "编译 Python 标准库"),
        ("6-runtime", "python.start5", "python 启动 ×5"),
        ("6-runtime", "node.start5", "node 启动 ×5"),
        ("6-runtime", "store.readtree", "读整个 python store path"),
    ]),
]


def load(d, backend):
    p = os.path.join(d, f"results-{backend}.json")
    if not os.path.exists(p):
        return None, {}
    r = json.load(open(p))
    return r, {(m["group"], m["key"]): m for m in r["metrics"]}


def fmt(m):
    if m is None:
        return "—"
    if m.get("status") != "ok":
        return m.get("status", "?")
    v = m.get("p50") or 0
    u = m.get("unit", "")
    if u == "s":
        return f"{v:.2f} s" if v < 10 else f"{v:.1f} s"
    if u == "ms":
        return f"{v / 1000:.2f} s" if v >= 1000 else (f"{v:.0f} ms" if v >= 100 else f"{v:.1f} ms")
    return f"{v:.1f} {u}" if v < 100 else f"{v:.0f} {u}"


def main():
    args = sys.argv[1:]
    backends = ["kvm", "native", "vz-intel", "qemu-arm64"]
    if "--backends" in args:
        i = args.index("--backends")
        backends = args[i + 1].split(",")
        del args[i:i + 2]
    base_dir, new_dir = args[0], args[1]
    data = {b: (load(base_dir, b), load(new_dir, b)) for b in backends}
    for b, ((rb, _), (rn, _)) in data.items():
        print(f"# {b}: baseline commit {rb and rb.get('commit')}, new commit {rn and rn.get('commit')}, new notes {rn and rn.get('notes')}")
    for title, rows in STEPS:
        print(f"\n## {title}")
        print("| 指标 | " + " | ".join(backends) + " |")
        print("|---|" + "---|" * len(backends))
        for g, k, name in rows:
            cells = []
            for b in backends:
                (_, mb), (_, mn) = data[b]
                a, n = mb.get((g, k)), mn.get((g, k))
                if a is None and n is None:
                    cells.append("—")
                    continue
                note = (n or {}).get("note") or ""
                cell = f"{fmt(a)} → {fmt(n)}"
                if note and k in ("first.total", "incr", "cold.wire"):
                    cell += f" ({note[:60]})"
                cells.append(cell)
            print(f"| {name} | " + " | ".join(cells) + " |")
    print("\n## runner 校准（0-env，越低越快；两边差 20% 以上时，相关数字要注明受 runner 影响）")
    for b in backends:
        (_, mb), (_, mn) = data[b]
        row = []
        for k in ("cpu", "disk", "fork"):
            a, n = mb.get(("0-env", k)), mn.get(("0-env", k))
            if a and n and a.get("p50") and n.get("p50"):
                ratio = n["p50"] / a["p50"]
                flag = "  ⚠" if abs(ratio - 1) > 0.2 else ""
                row.append(f"{k} {a['p50']:.1f} → {n['p50']:.1f} ({ratio:.2f}x){flag}")
            elif n and n.get("p50"):
                row.append(f"{k} 基线没有 → {n['p50']:.1f}")
        print(f"- {b}: " + "; ".join(row))


if __name__ == "__main__":
    main()
