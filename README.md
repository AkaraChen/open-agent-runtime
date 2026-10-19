# open-agent-runtime — Composable Linux Runtime

`oar` builds declarative Linux environments ahead of time, as immutable artifacts. It composes them
into OCI bundles in about a millisecond and runs them with `runc` inside a resident Linux guest.

```yaml
runtime:
  base: debian:trixie-slim
  platform: linux/arm64

components:
  - nix:nodejs_22
  - nix:python313
  - nix:git
  - binary:my-agent

process:
  command: ["/opt/my-agent"]
```

```console
$ oar build            # resolve, download and pre-compile everything (once)
$ oar run              # boot/reuse the guest, warm its cache, compose, runc create/start
$ oar exec -- node -v  # v22.23.3
$ oar reload           # start a new instance, then retire the old one
$ oar stop --rm
```

## Architecture

```text
Host (macOS / Linux / Windows)                 Linux Guest (resident)
┌──────────────────────────────┐   vsock /     ┌───────────────────────────────────────┐
│ oar build  ─► artifacts/     │   unix sock   │ /init  (oar-agent, PID 1)             │
│ oar run    ─► supervisor ────┼──────────────►│  └─ oar-agent serve                   │
│              (vz|kvm|native) │               │      ├─ /var/lib/oar  (ext4 data disk) │
└──────────────────────────────┘               │      │   ├─ bases/<sha>/rootfs         │
                                               │      │   ├─ store/nix/store/...  (ro)  │
                                               │      │   ├─ components/<sha>/tree      │
                                               │      │   └─ instances/<id>/{rootfs,..} │
                                               │      └─ runc create/start/kill/delete  │
                                               └───────────────────────────────────────┘
```

The work is split into three stages.

1. **Artifact Builder** (`internal/builder`). Every step here is slow, but it runs only once.
   - **Base:** the Debian image is pinned by digest (recorded in the lock; `--update` re-resolves the
     tag). Its layers are flattened, whiteouts applied, into `bases/<name>/<platform>/rootfs.tar`.
   - **Nix:** a pinned nixpkgs revision is evaluated once per attribute, using `nix-instantiate` or
     the `nixos/nix` container, and the result is cached. The runtime closure is then fetched from
     `cache.nixos.org`. Each narinfo's ed25519 signature and NarHash are verified. NARs are converted
     to tar in pure Go, so Linux store paths can be prepared on case-insensitive macOS volumes. Output
     is one tarball per store path in `store/nix/store/<hash-name>.tar`.
   - **Binaries, directories and files:** an optional per-arch `build` command runs first. The result
     is imported into a deterministic `tree.tar` (uid 0, fixed mtime).
   - **Metadata:** dependency closure, PATH, environment, file-conflict checks, and ELF ABI checks
     (machine type, plus whether the interpreter exists in the base or the closure). Everything goes
     to `manifests/bundle.lock.json`, separately for `linux/amd64` and `linux/arm64`.
2. **Guest Runtime** (`guest/`, `internal/agent`, `internal/vm`). The guest is a minimal Linux:
   - Linux 6.12 LTS with everything built in.
   - An initramfs holding `oar-agent` as `/init`, a static `runc`, and `mkfs.ext4`.

   PID 1 mounts the persistent data disk and supervises the agent, which listens on vsock port 1024. On
   the host, a supervisor process keeps the VM resident and exposes the agent as
   `~/.local/share/oar/vms/<name>/agent.sock`.
3. **Compositor** (`internal/compose`). This is the hot path. It verifies the bundle digest and stats
   the warm cache. On a cache miss it writes a bundle template, cached by digest. Then:
   - It mounts an OverlayFS view: component trees over the base rootfs, plus a per-instance upper layer.
   - It bind-mounts the shared `/nix/store` read-only.
   - It writes `config.json` and runs `runc create` and `runc start`.

   There is no Nix evaluation, no network access and no file copying. The bundle is never materialized
   as an image.

### Determinism

Components form an unordered set. `requires` is expanded transitively, and everything is sorted. The
bundle digest is the SHA-256 of a canonical description: the base digest, store paths with their
NarHash, component content hashes, the process, and the environment. Conflicts are errors, never
"last one wins":

- the same file provided by two components;
- a component replacing a base file without `override: true`;
- a component path that would shadow a base symlink (e.g. `/bin -> usr/bin`);
- the same binary name provided by two Nix packages;
- the same env var set to different values;
- an ELF file built for the wrong machine, or missing its interpreter.

PATH order is fixed: Nix packages sorted by attribute name, then component `path_entries`, then the
base PATH.

## Backends

| backend   | host          | how                                                                                            |
|-----------|---------------|------------------------------------------------------------------------------------------------|
| `vz`      | macOS         | Virtualization.framework (`Code-Hex/vz`), virtio-vsock, NAT, sparse raw data disk              |
| `kvm`     | Linux         | QEMU + KVM (TCG fallback), vhost-vsock                                                         |
| `qemu`    | Linux, macOS  | QEMU with KVM/HVF/TCG; vsock on Linux, loopback TCP forward elsewhere                          |
| `native`  | Linux         | the agent runs on the host kernel (as root, via sudo); fastest for Linux development           |
| `wsl2`    | Windows       | imports the guest rootfs as a WSL2 distro (experimental)                                       |
| `android` | —             | extension point (AVF/crosvm), not part of the MVP                                              |

The default is `vz` on macOS, `native` on Linux and `wsl2` on Windows; override it with `--backend` or
`OAR_BACKEND`.

```console
$ guest/build.sh arm64          # kernel + initramfs -> guest/out/arm64 (docker + go)
$ hack/build.sh                 # bin/oar (signed with the virtualization entitlement on macOS)
$ bin/oar vm start --backend vz --cpus 4 --memory 4096
$ bin/oar vm status
```

## Configuration reference

```yaml
runtime:
  base: debian:trixie-slim        # or debian:trixie-slim@sha256:...
  platform: linux/arm64           # default platform
  platforms: [linux/amd64, linux/arm64]
  nixpkgs: 8ce73368d1ef...        # pinned revision (default: nixos-26.05 pin)
  hostname: oar

components:                       # unordered set
  - nix:<attr.path>               # any nixpkgs attribute
  - binary:<name>                 # single executable  (default target /opt/<name>)
  - file:<name>                   # single file
  - dir:<name>                    # directory tree

sources:
  <name>:
    path: dist/tool-{{arch}}      # {{arch}} / {{os}} placeholders
    target: /opt/tool
    mode: "0755"
    build: [go, build, -o, "dist/tool-{{arch}}", ./tool]
    build_env: {GOOS: linux, GOARCH: "{{arch}}", CGO_ENABLED: "0"}
    env: {TOOL_HOME: /opt/tool}
    path_entries: [/opt/tool/bin]
    requires: [nix:git]
    override: false

process:
  command: ["/opt/my-agent"]
  env: {KEY: value}
  cwd: /
  user: "0:0"
```

## Testing

- `go test ./...` runs the unit tests: order invariance, digests, conflict and ABI checks, NAR
  conversion, signature verification, and extraction safety.
- `BACKEND=<native|kvm|qemu|vz> hack/e2e.sh` runs the MVP acceptance suite:
  1. Debian slim plus three Nix packages plus a custom binary starts and reports node 22, Python 3.13
     and git.
  2. Three shuffled configs produce the identical digest and an identical observed environment.
  3. Warm compose stays under 1 s.
  4. runc start, stop, delete and reload all work.
  5. A single resident guest serves every request.
- CI (`.github/workflows/ci.yml`) runs the unit tests on Linux, macOS and Windows. It builds both guest
  images and runs the e2e suite on Linux native, Linux KVM and macOS (vz and qemu).
