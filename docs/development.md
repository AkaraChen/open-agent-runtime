# Development

## Repository layout

```text
cmd/oar            host CLI
cmd/oar-agent      guest agent (PID 1 inside the guest)
internal/spec      configuration parsing and validation
internal/builder   artifact builder (OCI base, Nix closures, components, conflict checks)
internal/nar       NAR → tar conversion, narinfo signatures
internal/manifest  lock and manifest types, bundle digest
internal/compose   compositor (bundle templates, OCI spec)
internal/agent     guest agent: cache, runc lifecycle, init
internal/proto     Host ↔ Guest protocol
internal/vm        backends and the resident-VM supervisor
internal/cli       command line
guest/             kernel config and guest image build scripts
hack/              build, acceptance and benchmark scripts
examples/          sample runtimes
```

## Build

```console
$ hack/build.sh            # bin/oar (cgo + entitlement signing on macOS)
$ guest/build.sh amd64     # guest kernel + initramfs (docker)
```

## Test

| Command | What it covers |
|---|---|
| `go test ./...` | order invariance, digests, conflict and ABI checks, NAR conversion, signature verification, extraction safety, component import |
| `BACKEND=<native\|kvm\|qemu\|vz> hack/e2e.sh` | MVP acceptance: start, deterministic digest and environment across shuffled configs, warm compose < 1 s, reload/stop/delete, resident guest |
| `BACKEND=<...> hack/switch-bench.sh [N]` | switching between compiled bundles under client load, readiness rollback |

Without KVM, VM paths can still be exercised under emulation:

```console
$ OAR_QEMU_TRANSPORT=tcp BACKEND=qemu hack/e2e.sh
$ OAR_QEMU_TRANSPORT=tcp GUEST_ARCH=arm64 BACKEND=qemu hack/e2e.sh
```

## Continuous integration

`.github/workflows/ci.yml`:

- runs unit tests on Linux, macOS and Windows;
- builds both guest images (the kernel is cached);
- runs the acceptance suite on Linux native, Linux KVM, macOS `vz` (Intel) and macOS `qemu` (Apple
  Silicon);
- runs the switch benchmark on Linux native and KVM.

## Environment variables

| Variable | Purpose |
|---|---|
| `OAR_HOME` | state directory (default `~/.local/share/oar`) |
| `OAR_BACKEND` | default backend |
| `OAR_GUEST_DIR` | guest image location |
| `OAR_NIX` | `local` or `docker` for Nix evaluation |
| `OAR_NIX_SUBSTITUTERS`, `OAR_NIX_TRUSTED_KEYS` | binary caches and their keys |
| `OAR_RUNC` | runc binary for the native backend |
| `OAR_QEMU`, `OAR_QEMU_ACCEL`, `OAR_QEMU_TRANSPORT` | QEMU binary, accelerator, `tcp` transport |
| `OAR_GUEST_CMDLINE` | extra guest kernel parameters (QEMU backends; debugging) |
