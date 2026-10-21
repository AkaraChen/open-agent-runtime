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

Without KVM, VM paths can still be exercised under emulation:

```console
$ OAR_QEMU_TRANSPORT=tcp BACKEND=qemu hack/e2e.sh
$ OAR_QEMU_TRANSPORT=tcp GUEST_ARCH=arm64 BACKEND=qemu hack/e2e.sh
```

## Continuous integration

`.github/workflows/ci.yml`:

- runs unit tests on Linux, macOS and Windows, with the embedded evaluator on Linux and macOS;
- builds both guest images (the kernel is cached);
- checks that the embedded evaluator and `nix-instantiate` agree on a few attributes (macOS);
- runs the acceptance suite on Linux native, Linux KVM, macOS `vz` (Intel) and macOS `qemu` (Apple
  Silicon);
- runs the switch benchmark on Linux native and KVM.

## Environment variables

| Variable | Purpose |
|---|---|
| `OAR_HOME` | state directory (default `~/.local/share/oar`) |
| `OAR_BACKEND` | default backend |
| `OAR_GUEST_DIR` | guest image location |
| `OAR_NIX` | Nix evaluation backend: `embedded` (default when linked in), `local` (nix-instantiate), `docker` |
| `OAR_NIX_IMAGE` | image for the `docker` backend (default `nixos/nix:latest`) |
| `OAR_NIXPKGS_MIRROR` | base URL serving `<rev>.tar.gz` nixpkgs archives (default GitHub) |
| `OAR_CACHE_DIR` | nixpkgs checkouts and the evaluator cache (default: the user cache directory, `oar/`) |
| `OAR_NIX_SUBSTITUTERS`, `OAR_NIX_TRUSTED_KEYS` | binary caches and their keys |
| `OAR_NO_IETS` | `1`: `hack/build.sh` builds without the embedded evaluator |
| `OAR_IETS_LIB_URL`, `OAR_IETS_TARGET` | where `hack/fetch-iets.sh` downloads from; the Rust target `hack/build-iets.sh` builds for |
| `OAR_RUNC` | runc binary for the native backend |
| `OAR_QEMU`, `OAR_QEMU_ACCEL`, `OAR_QEMU_TRANSPORT` | QEMU binary, accelerator, `tcp` transport |
| `OAR_QEMU_FORWARD`, `OAR_VZ_FORWARD` | `channel`: tunnel ingress ports through the control channel instead of QEMU's own port forwarding / a direct connection to the VZ guest |
| `OAR_QEMU_MACHINE` | `q35` or `microvm`: machine type for amd64 guests (default: `microvm` with KVM, `q35` otherwise) |
| `OAR_WARM_COMPRESS` | `1` or `0`: compress warm uploads with zstd (default: only over a QEMU port forward, the macOS QEMU backend; the compressed copy is cached as `<artifact>.zst`) |
| `OAR_WARM_WRITEBACK` | `1` or `0`: the agent syncs the cache in the background while a warm uploads (default: QEMU-based guests yes, VZ and native no) |
| `OAR_VIEW_PREPARE` | `0`: build the store view only at the end of a warm (comparisons) |
| `OAR_WARM_FLUSH` | `1` or `0`: write the cache back at the end of a warm (default: VM guests yes, so their first start does not wait for ext4's journal; native no) |
| `OAR_QEMU_MICROVM_PCIE` | `0`: microvm devices use virtio-mmio instead of its PCIe bridge |
| `OAR_GUEST_CMDLINE` | extra guest kernel parameters (QEMU and VZ backends; debugging) |
| `OAR_STORE_VIEW` | `binds`: containers see their closure through one bind mount per store path instead of a hard-link tree (read at `vm start`; for comparisons) |
