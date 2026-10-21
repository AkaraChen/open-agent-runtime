---
name: bench-journey-report
description: Run the oar benchmark on CI and report the results to the user as a narrated end-user journey (build → vm start → warm → run → switch → slow-start app → host access → runtime, then macOS), with "before → after" numbers. Use when the user asks how fast oar is now, wants a speed summary "from the user's point of view", or asks to rerun the bench and report in this format.
---

# Benchmark report as a user journey

The user wants speed reported as an end user would experience it: one walk
through a working day with oar, with every step carrying concrete
**before → after** timings. Tables of metrics are supporting material, not
the report.

## 1. Get the numbers

1. Make sure everything to be measured is pushed to `main`.
2. Run the full benchmark on CI (all four backends: native, kvm, vz-intel,
   qemu-arm64):

   ```console
   $ gh workflow run bench.yml --ref main
   $ gh run list --workflow bench.yml -L 1 --json databaseId -q '.[0].databaseId'
   ```

   Wait for it in the background (`until [ "$(gh run view $R --json status -q .status)" = completed ]; do sleep 60; done`,
   `run_in_background`, maximum timeout). macOS jobs take 1–1.5 h and may queue
   behind other branches' runs; report progress briefly if asked, do not poll in the foreground.
3. Download the results into a fresh directory:

   ```console
   $ for l in native kvm vz-intel qemu-arm64; do gh run download $R -n results-$l -D <dir>; done
   ```

4. Extract the journey numbers against the baseline. "Before" is the step 0
   baseline (`docs/benchmarks/00-baseline`, measured before any
   optimization) unless the user names another point:

   ```console
   $ python3 -I .claude/skills/bench-journey-report/journey.py docs/benchmarks/00-baseline <dir>
   ```

   Every number in the report must come from this output (or from a
   result file you can point to). Never estimate a number without saying it
   is an estimate.

5. Check the run's `notes` (printed in the header lines): a failed phase
   means missing metrics; say so instead of filling the gap.

## 2. Write the report (in the user's language, normally Chinese)

Structure, in this order:

1. One framing sentence: which backend the walk-through uses (kvm on
   Linux: a resident lightweight VM per container host), what the numbers
   mean (`优化前 → 现在`, before = step 0 baseline, all from CI benchmark),
   and the workload (Debian + Node.js + Python + git + the agent, 120 Nix
   store paths, about 553 MiB; take the size from `1-build/closure.*`).
2. Numbered journey steps, each a bold heading plus a short paragraph or
   bullets, numbers in bold as **before → after**:
   1. **写好 `oar.yaml`，第一次 `oar build`**: cold build, adding one package.
   2. **`oar vm start`**: cold boot, restart, control channel round trip, CLI command.
   3. **`oar warm`**: cold warm of the whole set, incremental warm. Explain
      where the time goes (upload, view, flush) and **state any cost first
      and plainly** if something got slower, with the reason and what it buys.
   4. **`oar run`**: first start after warm, every start, 10 concurrent.
   5. **改配置，线上切换**: replace vs ingress switch (time and dropped
      requests), prepared switch, rollback.
   6. **慢启动应用（初始化 3 秒）**: no standby/snapshot vs hot standby
      (`oar prepare --start`) vs application snapshot (`snapshot: true`,
      one-off snapshot cost).
   7. **从宿主机访问容器里的服务**: latency, throughput, port availability.
   8. **容器里的程序跑得怎么样**: runtime overhead, say "±N%" if flat.
3. A **macOS (vz)** section: relative changes only, and say the CI Intel Mac
   runner is nested virtualization (absolute numbers are inflated; VM boot
   is dominated by VZ loading the kernel there).
4. **一句话体验**: before vs now in one sentence, then the costs in one
   sentence.

## Rules

- Lead with the user's experience ("以前要盯着终端等将近一分钟"), then the
  number. Keep sentences short; one idea per bullet.
- Costs and regressions are reported, never buried: put them in the step
  where they occur, before the benefits they pay for.
- Mark weak evidence: a single sample, or baseline and new run on different
  runners. When `journey.py` flags a calibration difference (⚠, >20% on
  cpu/disk/fork), say that the related numbers partly reflect the runner.
  Prefer same-runner A/B results (bench `ab_env` / `base` modes) for claims
  about a specific change, and say which kind of evidence a claim rests on.
- `6-runtime/net.connect` goes to the public internet and measures the
  runner's network; do not report it as oar's speed (use `net.local`).
- Do not mention metrics that did not change unless the step needs them for
  the story; do not add steps that are not in the journey.
- Mention unfinished work or open problems at the end, briefly.
- If the user also wants it recorded, save the generated `hack/bench report`
  next to the results under `docs/benchmarks/<NN>-<topic>/` and commit; the
  journey report itself goes to the user.
