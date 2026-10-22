# Platforms

## Backends

| Backend | Host | How it runs the guest |
|---|---|---|
| `vz` | macOS 26+ | Apple Virtualization.framework, virtio-vsock, a vmnet NAT network on a subnet the host does not use, sparse data disk |
| `kvm` | Linux | QEMU with KVM (falls back to TCG emulation), `microvm` machine with KVM (minimal ACPI, virtio-pci on its PCIe bridge), vhost-vsock |
| `qemu` | Linux, macOS | QEMU with KVM, HVF or TCG; vsock on Linux, loopback TCP forward elsewhere |
| `native` | Linux | no VM: the agent drives `runc` on the host kernel (requires root, via sudo) |
| `wsl2` | Windows | imports the guest root filesystem as a WSL2 distribution (experimental) |
| `android` | — | extension point (AVF / crosvm), planned |

Defaults: `vz` on macOS (or `qemu` before macOS 26 and when the host has no hypervisor support), `native` on Linux, and
`wsl2` on Windows. Override the default with `--backend`, and list what works on the
current host with `oar vm backends`.

## Verified in CI

Each entry runs the full acceptance suite (start, deterministic digests, warm compose < 1 s, runc
lifecycle, resident guest) unless noted.

| Host | Backend | Guest |
|---|---|---|
| Ubuntu | `native` | host kernel (amd64) |
| Ubuntu | `kvm` | linux/amd64 (boots in under 1 s) |
| macOS 26, Intel | `vz` | linux/amd64 |
| macOS 26, Apple Silicon | `qemu` | linux/arm64 |
| macOS 26, Apple Silicon | `vz` | not testable: hosted runners lack nested virtualization; the fallback to `qemu` is verified instead |
| Windows | — | unit tests and build only |

## Guest image

```console
$ guest/build.sh amd64     # or arm64; needs docker and go
$ ls guest/out/amd64
initramfs.cpio.lz4  kernel  kernel.config  rootfs.tar
```

`oar vm start` looks for the image in `--guest-dir`, then `<state directory>/guest/<arch>`, then
next to the binary (`../share/oar/guest/<arch>`, `guest/<arch>`, or `../guest/out/<arch>` of a
checkout), and finally `guest/out/<arch>` of the current directory.

## macOS

The `oar` binary must be signed with the `com.apple.security.virtualization` entitlement.
`hack/build.sh` builds with cgo and signs ad hoc:

```console
$ hack/build.sh
$ bin/oar vm start --cpus 4 --memory 4096
```

Artifact building on macOS needs no Nix installation: packages are evaluated by the evaluator built
into `oar` (`hack/build.sh` links it; `oar --version` shows it). Store paths themselves are fetched
and converted without touching the case-insensitive filesystem.
