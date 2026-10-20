<div align="center">

# open-agent-runtime

**Composable, reproducible Linux runtimes for AI agents and developer tools: declare once, start in milliseconds, anywhere.**

[![CI](https://github.com/AkaraChen/open-agent-runtime/actions/workflows/ci.yml/badge.svg)](https://github.com/AkaraChen/open-agent-runtime/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/AkaraChen/open-agent-runtime.svg)](https://pkg.go.dev/github.com/AkaraChen/open-agent-runtime)
![Status](https://img.shields.io/badge/status-alpha-orange)
![Platforms](https://img.shields.io/badge/platforms-linux%20%7C%20macOS-blue)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

</div>

---

## What is open-agent-runtime?

Agents and the tools they drive need real Linux environments: a language runtime or two, git, a
handful of CLIs, and your own binaries. Building those environments as container images is slow,
images drift, and every change means rebuilding and redistributing gigabytes.

**open-agent-runtime** (`oar`) treats an environment as a *composition* rather than an image. You
list what you need. `oar` compiles every piece ahead of time into immutable, content-addressed
artifacts, then assembles them into a running container in milliseconds. On macOS and Windows it
runs inside a lightweight Linux VM that stays resident.

```yaml
runtime:
  base: debian:trixie-slim

components:
  - nix:nodejs_22
  - nix:python313
  - nix:git
  - binary:my-agent

process:
  command: ["/opt/my-agent"]
```

```console
$ oar build     # compile once
$ oar run       # start in ~100 ms
$ oar reload    # switch to a new environment with no downtime
```

## Why open-agent-runtime?

- **Fast where it matters.** All resolution, downloading and packaging happens at build time.
  Starting or switching an environment only mounts cached layers and calls `runc`: about 100 ms end
  to end.
- **Reproducible by construction.** Base images are pinned by digest and packages by commit. The same
  set of components always produces the same bundle digest and the same environment, regardless of
  the order you list them in.
- **Composable, not monolithic.** Mix a distro base, any of the 100,000+ packages in nixpkgs, and your
  own binaries, directories or inline files. Conflicts are reported, never silently resolved.
- **Ship artifacts, not images.** Compiled environments are plain content-addressed files. Distribute
  them to fleets, deduplicate across versions, and roll out new environments without build tools on
  the target.
- **Safe rollouts.** Readiness checks and atomic reloads: a new environment that fails to come up
  never replaces the one that works.
- **One workflow on every machine.** The same CLI and the same environments on Linux servers, macOS
  laptops and CI.

## How it works

```mermaid
flowchart LR
    A["Declare<br/>oar.yaml"] --> B["Compile<br/>immutable artifacts"]
    B --> C["Warm<br/>resident Linux guest"]
    C --> D["Compose<br/>overlay + OCI bundle"]
    D --> E["Run<br/>runc"]
```

1. **Compile.** The builder resolves the base image, a pinned set of Nix packages with their full
   runtime closure, and your custom files into verified, content-addressed artifacts.
2. **Warm.** Artifacts are loaded once into the guest's cache. Later environments only transfer
   what is new.
3. **Compose and run.** Each start mounts a layered view of cached artifacts and launches it as an
   OCI container.

Read the [architecture guide](docs/architecture.md) for the full design.

## Features

| | |
|---|---|
| **Declarative environments** | Base image, Nix packages, binaries, directories and inline files in one YAML file |
| **Pinned and verified** | Digest-pinned bases, commit-pinned nixpkgs (per package if needed), signature-checked downloads |
| **Multi-architecture** | Artifacts for `linux/amd64` and `linux/arm64` from any build host |
| **Resident guest** | A minimal Linux VM boots once and serves every container: Virtualization.framework on macOS, KVM on Linux |
| **Zero-downtime reloads** | `overlap` and `replace` strategies, readiness checks, automatic rollback |
| **Offline distribution** | Start and switch compiled environments by digest: no config, compilers or network needed |
| **Isolated environments** | Each container sees only its own `/nix/store` closure, read-only |

## Getting started

> open-agent-runtime is in **alpha**. Interfaces may change.

```console
# build the CLI and the guest image
$ hack/build.sh
$ guest/build.sh amd64          # or arm64

# run the example environment
$ cd examples/agent-runtime
$ ../../bin/oar run
$ ../../bin/oar exec -- node --version
v22.23.3
$ ../../bin/oar stop --rm
```

See [platforms](docs/platforms.md) for host requirements and supported backends.

## Documentation

| Guide | |
|---|---|
| [Architecture](docs/architecture.md) | design, components, data flow, diagrams |
| [Configuration](docs/configuration.md) | the `oar.yaml` reference |
| [Operations](docs/operations.md) | switching environments, distributing caches, reload strategies |
| [Platforms](docs/platforms.md) | backends, guest image, macOS notes |
| [Development](docs/development.md) | building, testing, CI |

## Platform support

| Host | Status |
|---|---|
| Linux (native or KVM) | ✅ supported |
| macOS (Virtualization.framework, QEMU fallback) | ✅ supported |
| Windows (WSL2) | 🧪 experimental |
| Android | 🗺️ planned |

## Roadmap

- Port forwarding from VM guests to the host
- Remote artifact stores and incremental sync
- Windows (WSL2) support
- Android support

## Contributing

Contributions are welcome. Start with the [development guide](docs/development.md). Before you
submit a change, run `go test ./...` and the acceptance suite (`hack/e2e.sh`) on at least one
backend.

## License

open-agent-runtime is licensed under the [MIT License](LICENSE).
