# Development

## Repository layout

```text
cmd/oar            host CLI
cmd/oar-agent      guest agent (PID 1 inside the guest)
internal/spec      configuration parsing and validation
internal/builder   artifact builder (OCI base, Nix closures, components, conflict checks)
internal/nixeval   Nix evaluation: `oar nix-eval`, the embedded iets evaluator, nix-instantiate and Docker backends, nixpkgs source cache
internal/nar       NAR → tar conversion, narinfo signatures
internal/manifest  lock and manifest types, bundle digest
internal/compose   compositor (bundle templates, OCI spec)
internal/agent     guest agent: cache, runc lifecycle, init
internal/proto     Host ↔ Guest protocol
internal/vm        backends and the resident-VM supervisor
internal/cli       command line
guest/             kernel config and guest image build scripts
rust/iets-ffi      C ABI around the iets Nix evaluator, built as a static library
third_party/iets   iets license and notice
hack/              build, acceptance and benchmark scripts
examples/          sample runtimes
```

## Build

```console
$ hack/build.sh            # bin/oar with the embedded Nix evaluator (cgo + entitlement signing on macOS)
$ guest/build.sh amd64     # guest kernel + initramfs (docker)
```

`hack/build.sh` links the iets evaluator as a static library. It installs the library from this
repository's releases (`hack/fetch-iets.sh`) or, when `cargo` is present, builds it from
`rust/iets-ffi` (`hack/build-iets.sh`; the first build takes a few minutes, later ones are cached).
The library goes to `internal/nixeval/iets/lib/<goos>_<goarch>/`, which is not tracked. The Go
packages build without it: `go build ./...` and `go test ./...` need neither cgo nor Rust, and the
evaluator comes in only with `-tags iets`. `OAR_NO_IETS=1 hack/build.sh` builds the evaluator-less
binary, which needs `nix-instantiate` or Docker at build time. On Linux the linker warns that
`getaddrinfo` in a static binary needs glibc's shared libraries at run time: the warning comes from
the evaluator's own HTTP client, which oar never calls (it fetches nixpkgs itself), so it is harmless.

The iets revision is pinned in `rust/iets-ffi/Cargo.toml`. To move it: change the `rev`, run
`hack/build-iets.sh`, run `go test -tags iets ./...`, and compare a few attributes against
`nix-instantiate` with `oar nix-eval --backend local`. Tagging `iets-lib-<rev prefix>` publishes the
libraries for every platform (`.github/workflows/iets-lib.yml`).

## Test

| Command | What it covers |
|---|---|
| `go test ./...` | order invariance, digests, conflict and ABI checks, NAR conversion, signature verification, extraction safety, component import, nixpkgs source cache, the `nix-eval` child process (errors, crashes, cancellation) |
| `go test -tags iets ./...` | the same plus the embedded evaluator binding |
| `BACKEND=<native\|kvm\|qemu\|vz> hack/e2e.sh` | MVP acceptance: start, deterministic digest and environment across shuffled configs, warm compose < 1 s, reload/stop/delete, resident guest |
| `BACKEND=<...> hack/switch-bench.sh [N]` | switching between compiled bundles under client load, readiness rollback |

Without KVM, VM paths can still be exercised under emulation (QEMU falls back to TCG, and to a TCP
control channel when `/dev/vhost-vsock` is not usable):

```console
$ BACKEND=qemu hack/e2e.sh
$ GUEST_ARCH=arm64 BACKEND=qemu hack/e2e.sh
```

## Continuous integration

`.github/workflows/ci.yml`:

- runs unit tests on Linux, macOS and Windows, with the embedded evaluator on Linux and macOS;
- builds both guest images (the kernel is cached);
- checks that the embedded evaluator and `nix-instantiate` agree on a few attributes (macOS);
- runs the acceptance suite on Linux native, Linux KVM, macOS `vz` (Intel) and macOS `qemu` (Apple
  Silicon);
- runs the switch benchmark on Linux native and KVM.

## Configuration and tuning

oar needs no environment variables. Everything that affects performance is chosen from the backend
in use, from decisions measured with same-runner A/B benchmarks (`docs/benchmarks/`):

| Choice | native | kvm (QEMU + KVM) | vz | qemu (TCG, port forward) |
|---|---|---|---|---|
| QEMU machine | — | `microvm`, virtio-pci on its PCIe bridge | — | `q35` (microvm needs kvm-clock) |
| Control channel | in-process | vhost-vsock | VZ vsock | TCP port forward (when vsock is not usable) |
| Host access to ingress ports | direct listen | control channel | direct to the guest's NAT address | QEMU's own forward (`hostfwd_add`) |
| Warm: flush at the end | no | yes | yes | yes |
| Warm: background writeback during upload | no | yes | no | yes |
| Warm: zstd-compressed upload | no | no | no | yes |
| Warm: store view built during upload, fewer syscalls per file | yes | yes | yes | yes |

The control channel is reported by the backend (`State.Transport` in the VM's `state.json`); the CLI
reads it to pick the warm settings.

What can be set, when the defaults do not fit:

- the backend and VM resources: `oar vm start --backend --cpus --memory --disk --arch`;
- the guest image: found next to the binary, in the state directory or in `guest/out/` of a
  checkout; `oar vm start --guest-dir` points elsewhere;
- networks that cannot reach GitHub or `cache.nixos.org`: `runtime.nixpkgs_mirror`,
  `runtime.substituters` and `runtime.trusted_keys` in `oar.yaml`;
- the state directory follows `$XDG_DATA_HOME` (default `~/.local/share/oar`), the evaluator cache
  `$XDG_CACHE_HOME` on Linux;
- debugging: `oar vm start --guest-cmdline '...'` appends guest kernel parameters (hidden flag; the
  `boot-profile` workflow uses it for `initcall_debug`); `oar vm profile --kind cpu --seconds 10 -o cpu.pprof`
  (hidden) saves a Go profile of the guest agent for `go tool pprof` (kinds: cpu, heap, allocs,
  goroutine, block, mutex, trace).
  `boot-profile` workflow uses it for `initcall_debug`). With `oar.dumpkmsg=1` the agent writes the
  kernel log (`/dev/kmsg`) to the console after the host's first request, so a boot can be profiled
  without printing the initcalls while it runs (`log_buf_len=4M` keeps all of them in the ring
  buffer).

The Nix evaluator is the first available of the embedded one, `nix-instantiate` and Docker; if the
embedded evaluator crashes, the build continues with the next available one.

Only the development build scripts read variables of their own: `OAR_NO_IETS=1 hack/build.sh` builds
without the embedded evaluator, `OAR_IETS_LIB_URL` and `OAR_IETS_TARGET` select where
`hack/fetch-iets.sh` downloads from and the Rust target `hack/build-iets.sh` builds for. Build
commands of `binary:` sources receive `OAR_ARCH`, `OAR_OS` and `OAR_PLATFORM` (see
`configuration.md`).
