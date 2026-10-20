# Architecture

open-agent-runtime (`oar`) turns a declarative description of a Linux environment into a running
container in three stages. Each stage has one job and a clear boundary:

| Stage | Runs | Responsibility | Cost |
|---|---|---|---|
| **Artifact Builder** | on the build host, once per change | resolve, download and pre-compile everything into immutable, content-addressed artifacts | seconds to minutes |
| **Guest Runtime** | resident, per host | a minimal Linux guest that holds the artifact cache and runs containers | boot once (≈1 s on KVM) |
| **Compositor** | in the guest, per start or reload | assemble a bundle from cached artifacts and hand it to `runc` | milliseconds |

The design rule is simple: **anything slow happens in the builder; the hot path only mounts and
starts things.**

```mermaid
flowchart LR
    subgraph Host["Host (macOS / Linux / Windows)"]
        direction TB
        CFG["oar.yaml"] --> B["Artifact Builder"]
        B --> ART[("artifacts/<br/>content-addressed tarballs<br/>+ bundle locks")]
        CLI["oar CLI"] --> SUP["VM supervisor<br/>(vz · kvm · qemu · native · wsl2)"]
    end

    subgraph Guest["Resident Linux guest"]
        direction TB
        AG["oar-agent"] --> CACHE[("guest cache<br/>bases · /nix/store · components")]
        AG --> COMP["Compositor"]
        COMP --> RUNC["runc"]
        RUNC --> CT1["container"]
        RUNC --> CT2["container"]
    end

    ART -- "warm (only missing artifacts)" --> AG
    SUP -- "vsock / unix socket" --> AG
```

## 1. Artifact Builder

The builder runs on the developer machine or in CI. It needs network access and, for Nix
evaluation only, either `nix-instantiate` or Docker. Its output is a directory of immutable artifacts
plus a lock file. Both can be copied anywhere.

```mermaid
flowchart TB
    CFG["oar.yaml<br/>(unordered component set)"] --> RES["resolve components<br/>expand requires, sort"]

    RES --> BASE["Base image<br/>pin tag → digest<br/>flatten layers"]
    RES --> NIX["Nix packages<br/>evaluate attr @ pinned nixpkgs<br/>(cached per revision)"]
    RES --> FILES["Binaries · dirs · files<br/>optional per-arch build<br/>or inline content"]

    NIX --> CLOSURE["fetch runtime closure<br/>from binary cache<br/>verify signature + NarHash<br/>NAR → tar, in memory"]

    BASE --> META
    CLOSURE --> META
    FILES --> META["metadata & checks<br/>PATH · env · file conflicts<br/>ELF machine / interpreter"]

    META --> LOCK["bundle lock<br/>deterministic bundle digest<br/>per platform"]
```

The builder produces:

- **Base rootfs.** The OCI base image (Debian slim by default) is pinned by digest, its layers
  flattened (whiteouts applied), and written as a single `rootfs.tar`.
- **Nix closure.** Each package is evaluated once at a pinned nixpkgs commit. Packages may pin their
  own commit with `nix:attr@<rev>`. The full runtime closure is fetched from the binary cache, with
  every narinfo's ed25519 signature and NarHash verified. NARs are converted to tarballs in memory,
  never extracted on the host, so Linux store paths can be prepared on case-insensitive macOS
  filesystems. Each store path becomes its own tarball, which gives deduplication across bundles for
  free.
- **Components.** Custom binaries, directory trees and files (from disk or inline in the config)
  are packed into deterministic tarballs: uid 0 and a fixed mtime, keyed by content hash.
- **Metadata.** Dependency closure, entry point, PATH, environment, file conflicts and ABI
  constraints are all computed here, so nothing has to be decided at start time.

Artifacts are built separately for `linux/amd64` and `linux/arm64`.

### Artifact layout

```text
artifacts/
├── bases/<image>/<platform>/        rootfs.tar + manifest (entries, digest)
├── store/nix/store/<hash-name>.tar  one tarball per store path (+ .json metadata)
├── components/<name>/<platform>/    tree.tar + manifest (files, ELF info)
└── manifests/
    ├── bundle.lock.json             latest build of the default config
    ├── bundles/<digest>.json        every compiled bundle, immutable
    ├── configs/<config-hash>.json   config content → bundle digest per platform
    └── nix-eval.json                evaluation cache (revision, system, attr → outputs)
```

## 2. Guest Runtime

Containers always run on a Linux kernel. On macOS and Windows that kernel lives in a small,
**resident** virtual machine. On Linux it can be the host kernel itself.

### The guest image

| Piece | Contents |
|---|---|
| Kernel | Linux 6.12 LTS, with everything the runtime needs built in (virtio, vsock, ext4, overlayfs, cgroup v2), and no modules |
| Initramfs | `oar-agent` as `/init`, a static `runc`, `mkfs.ext4`, and a busybox userland |
| Data disk | sparse ext4 image, formatted on first boot, mounted at `/var/lib/oar` |

`oar-agent` runs as PID 1 to initialise the system and reap orphans. It then supervises a second
copy of itself that serves the control API. The guest has no package manager and installs nothing:
dependency management happens entirely in the builder.

### Backends

All backends implement one small interface: boot a machine, dial its agent, and stop it.

```mermaid
classDiagram
    class Backend {
        <<interface>>
        Name()
        Available() error
        Boot(config) Machine
    }
    class Machine {
        <<interface>>
        DialAgent() Conn
        Done()
        Kill()
    }
    Backend <|.. vz : macOS · Virtualization.framework
    Backend <|.. kvm : Linux · QEMU/KVM
    Backend <|.. qemu : Linux/macOS · KVM/HVF/TCG
    Backend <|.. native : Linux · host kernel
    Backend <|.. wsl2 : Windows · experimental
    Backend <|.. android : extension point
    Backend --> Machine
```

A host-side **supervisor** process boots the machine, keeps it running, and exposes the guest agent
on a local unix socket. Every CLI command talks to that socket, whatever the backend.

```mermaid
flowchart LR
    CLI["oar run / reload / ps ..."] -->|"unix socket<br/>~/.local/share/oar/vms/&lt;name&gt;/agent.sock"| SUP["supervisor"]
    SUP -->|"vsock :1024<br/>(TCP forward on QEMU without vhost-vsock)"| AGENT["oar-agent (guest)"]
    SUP -.->|"in-process"| NATIVE["oar-agent (native backend)"]
```

### Control protocol

Each request uses its own stream: a JSON header line, optionally followed by a raw body. Responses
mirror this, with streaming bodies for logs. Artifact tarballs travel as request bodies, so the same
protocol works over vsock, unix sockets and TCP, with no shared filesystem.

| Operation | Purpose |
|---|---|
| `info` | agent version, protocol version, kernel, architecture |
| `warm.status` / `warm.put` | find missing artifacts; upload one; the agent verifies its SHA-256 while extracting, then publishes it atomically |
| `compose` | build the bundle template (cached) and report its digest |
| `run` / `reload` | compose, instantiate and start; reload switches instances |
| `stop` / `delete` / `list` / `state` / `logs` / `exec` | container lifecycle |
| `shutdown` | stop all containers and power off the guest |

The CLI and agent check a protocol version, so a CLI refuses to drive an outdated guest image.

### Guest cache layout

```text
/var/lib/oar/
├── bases/<sha>/rootfs/            extracted base root filesystems
├── store/nix/store/<hash-name>/   shared immutable Nix store (all warmed store paths)
├── components/<sha>/tree/         extracted component trees
├── bundles/<digest>/bundle.json   composed bundle templates (layers + OCI spec)
├── bundles/<digest>/store/        /nix/store skeleton: one empty mountpoint per closure path
└── instances/<id>/                per-container: rootfs (overlay mount), upper/, work/,
                                   config.json, console.log
```

## 3. Compositor

The compositor turns a bundle lock and a warm cache into a runnable OCI bundle. It is the only stage
on the start path, so it is deliberately boring. It **never** evaluates Nix, touches the network,
installs packages, or copies file contents.

```mermaid
flowchart LR
    LOCK["bundle lock"] --> V{"digest matches content?<br/>artifacts warm?"}
    V -- no --> ERR["error"]
    V -- yes --> T{"template cached?"}
    T -- no --> GEN["generate layers + OCI spec<br/>cache by digest"]
    T -- yes --> INST
    GEN --> INST["instantiate<br/>overlay mount + config.json"]
    INST --> RC["runc create → start"]
```

The container's root filesystem is a mounted view, not a copied image:

```mermaid
flowchart TB
    subgraph View["container view"]
        ROOT["/  (OverlayFS)"]
        NS["/nix/store  (read-only skeleton)"]
        P1["/nix/store/…-nodejs-22"]
        P2["/nix/store/…-glibc-2.42"]
    end
    UP["instance upper/ (writable)"] --> ROOT
    C1["component tree: my-agent"] --> ROOT
    C2["component tree: app-config"] --> ROOT
    BASE["base rootfs: debian-trixie-slim"] --> ROOT
    SK["bundle skeleton<br/>(closure mountpoints)"] --> NS
    subgraph Shared["shared guest /nix/store"]
        S1["…-nodejs-22"]
        S2["…-glibc-2.42"]
        S3["…-git-2.54  (other bundle)"]
    end
    S1 -- "ro bind" --> P1
    S2 -- "ro bind" --> P2
```

Component layers are ordered by content hash, not by configuration order. This is safe because
conflicts were already rejected by the builder.

The guest keeps one shared, deduplicated `/nix/store`, but each container sees **only its own
closure**. A per-bundle skeleton of empty mountpoints is bound read-only at `/nix/store`, and each
closure path is bound read-only onto its mountpoint. Store paths warmed for other bundles stay
invisible, and nothing under `/nix/store` is writable. For a 120-path closure this adds about 12 ms
to `runc create`.

### Starting a container

```mermaid
sequenceDiagram
    participant U as oar CLI
    participant S as supervisor
    participant A as oar-agent
    participant R as runc
    U->>S: info (boot the guest if needed)
    U->>A: warm.status(artifacts of bundle)
    A-->>U: missing items
    U->>A: warm.put(tarball) × missing
    U->>A: run(name, bundle lock)
    A->>A: compose (cached) + overlay mount
    A->>R: create (process prepared, not exec'd)
    A->>R: start
    A->>A: wait for readiness check
    A-->>U: container + timings
```

### Reloading without downtime

`reload` replaces the running instance with one from another bundle. Two strategies trade overlap
for port exclusivity:

```mermaid
sequenceDiagram
    autonumber
    participant A as oar-agent
    participant Old as old instance
    participant New as new instance
    Note over A,New: strategy = overlap (default)
    A->>New: create + start
    A->>New: wait until ready
    alt ready
        A->>Old: SIGTERM (SIGKILL after grace)
        A--)Old: delete in background
    else not ready / exited
        A->>New: kill + delete
        Note over A,Old: rolled back, the old instance keeps serving
    end
```

```mermaid
sequenceDiagram
    autonumber
    participant A as oar-agent
    participant Old as old instance
    participant New as new instance
    Note over A,New: strategy = replace
    A->>New: create (namespaces, mounts, cgroup ready, process not exec'd)
    A->>Old: SIGTERM, wait for exit
    A->>New: start
    A->>New: wait until ready
    A--)Old: delete in background
```

- **overlap** has no gap. If the service listens on a port, it needs `SO_REUSEPORT` so that both
  instances can bind it during the handover.
- **replace** keeps ports exclusive. The gap is the old process's exit time plus the new process's
  startup, typically tens of milliseconds.

See [operations.md](operations.md) for measurements.

## Determinism

The same set of components always produces the same bundle digest and the same container
environment:

- Components are an **unordered set**: `requires` is expanded transitively, duplicates are removed,
  and everything is sorted.
- The **bundle digest** is a SHA-256 over a canonical description of everything the container can
  observe: base digest, store paths with their NarHash, component content hashes, process, and
  environment.
- **Conflicts are errors, never "last one wins".** The builder rejects:
  - a file provided by two components;
  - a base file replaced without `override: true`;
  - a component path that would shadow a base symlink;
  - the same binary provided by two Nix packages;
  - the same environment variable set to different values;
  - an ELF binary built for the wrong machine, or missing its interpreter.
- **PATH order is fixed**: Nix packages sorted by attribute, then component entries, then the base
  PATH.

## Security notes

- **Signatures.** Binary cache downloads are only accepted with a valid signature from a trusted
  key (`cache.nixos.org` by default), and a matching NarHash.
- **Integrity.** Every artifact is verified against its content hash while it is extracted in the
  guest. Bundle locks are re-hashed before use, so a tampered lock is rejected.
- **Extraction.** Tar extraction refuses to traverse symlinks or escape the destination.
- **Containers.** They run under `runc` with namespaces (pid, ipc, uts, mount, cgroup), a default
  capability set, `no_new_privs`, masked `/proc` paths, and a read-only `/nix/store` limited to
  the bundle's own closure.
- **Guest isolation.** In VM backends, the guest kernel isolates workloads from the host. The
  `native` backend runs containers on the host kernel and needs root.

## Current limitations

- Containers share the guest's network namespace. VM backends do not yet forward container ports to
  the host.
- On GitHub's Apple Silicon runners, Virtualization.framework cannot be tested because those runners
  lack nested virtualization. Intel Macs are covered, and Apple Silicon falls back to QEMU.
- WSL2 is experimental; Android is an extension point only.
