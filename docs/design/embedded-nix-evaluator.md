# Design: embedding iets as the Nix evaluator

Status: implemented (2026-10-20). The sections below are the design as
built; "Measured" at the end has the numbers from the first run.

## Goal

`oar build` must work on a machine with neither Nix nor Docker installed.
Today the builder needs one of them for a single step: turning `nix:<attr>`
components into store paths. Everything after that (closure walk, NAR
download, signature check, tar conversion) is already pure Go
(`internal/builder/nix.go`, `internal/nar`).

The proposal is to link [iets](https://github.com/numtide/iets), a Nix
evaluator written in Rust, into `oar` as a static library behind a C ABI,
expose it as the `oar nix-eval` command, and keep `nix-instantiate` and
Docker as optional backends of that same command.

Non-goals: building derivations, a local `/nix/store`, flakes, evaluating
anything but the attribute lookup the builder does today.

## What oar asks of Nix

`Builder.evalPackages` (`internal/builder/nix.go`) evaluates, once per
`(nixpkgs rev, system)`, one expression that returns for each attribute its
output store paths and `meta.mainProgram`:

```nix
let pkgs = import (builtins.fetchTarball "https://github.com/NixOS/nixpkgs/archive/<rev>.tar.gz") {
      system = "<system>"; config = { allowUnfree = false; }; overlays = [];
    };
in { "<attr>" = { outputs = [ "/nix/store/..." ]; mainProgram = "..."; }; ... }
```

Results are cached in `artifacts/manifests/nix-eval.json`, so the evaluator
runs only for attributes never seen at that revision. The store paths are then
fetched from `cache.nixos.org` by narinfo. This matters for correctness: a
wrongly evaluated path is not in the cache and the build fails loudly, so the
binary cache acts as an oracle for the evaluator.

## iets, as far as it matters here

Facts checked against the source at commit `bdd26f2c` (2026-10-19):

- A from-scratch Nix evaluator: parser, lazy evaluator, derivation and store
  path computation in memory. No local store, no builds. Goal number one is
  byte-exact `.drvPath` parity with `nix-instantiate` on nixpkgs.
- One crate, `iets`, that is both a library and a binary. `src/api.rs` is the
  documented library facade; `examples/eval.rs` is a 90-line frontend that
  does exactly what we need: evaluate an expression and print strict JSON.
- Platforms: Linux (x86_64, aarch64, riscv64), macOS (x86_64, aarch64) and
  Windows (x86_64, aarch64) have context-switch code and a `sys` backend.
  The flake builds `aarch64-darwin` and static musl binaries for both Linux
  architectures.
- Pure evaluation reads only files below a `project_root`, which suits us:
  the root is the nixpkgs checkout.
- `builtins.fetchTarball` needs a `sha256`. The expression above cannot run
  as is; the nixpkgs tree has to be fetched by oar and imported by path.
- Memory: every value lives in a `Session` arena and is freed when the
  session drops, so one session per evaluation is the documented pattern.
  Released address space stays reserved.
- `parallel::try_run` with one worker runs the evaluation on the calling
  thread's own stack; the iets CLI therefore starts a thread with
  `api::MAIN_STACK` (4 GiB of address space) for it (`src/cli/run.rs`).
  With more workers the main fiber gets its own mapped stack.
- The parallel runtime installs its own `SIGSEGV`/`SIGBUS` handlers
  (`src/parallel/fiber.rs` → `sys::fault::install`) and a 64 KB
  `sigaltstack` on each thread it runs on. A fault it does not own gets the
  default disposition. Inside a Go process that replaces the Go runtime's
  handlers for the rest of the process's life.
- Paths that end the process rather than return an error: a Rust panic
  (release profile is `panic = "abort"`, and a panic on a fiber stack has no
  frame to unwind to anyway; the only `catch_unwind` is in `src/cli/run.rs`),
  `process::exit(1)` when a heap or arena `mmap` fails (`src/runtime/heap.rs`,
  `src/runtime/arena.rs`) or the symbol table overflows
  (`src/parallel/interner.rs`), `_exit(1)` on a fiber stack overflow and
  `abort()` on a use after session release (the fault handler). There is no
  API to cancel a running evaluation.
- Version `0.1.0`, no releases, README calls it "very fresh" and "LLM-ware".
  The library API "can change".
- License LGPL-2.1-or-later.

Alternatives considered: snix (formerly tvix) has a Rust evaluator too, but
weaker nixpkgs parity and a larger surface. Compiling iets to WebAssembly for
wazero is out: the runtime needs futexes, `madvise`, `/proc/self` and
hand-written context switches. Embedding a prebuilt `iets` executable with
`go:embed` and exec'ing it is the cheapest fallback if the static library
turns out to be too much friction; it needs no cgo and no toolchain on the
Go side, at the cost of writing the binary to disk at first use.

## Architecture

```
oar.yaml ─▶ spec ─▶ builder.evalPackages
                        │ exec os.Executable() nix-eval --json … (child process)
                        ▼
                    oar nix-eval ─▶ nixeval.Evaluator (interface)
                                      ├─ embedded  cgo ─▶ libiets_ffi.a ─▶ iets   (default when linked in)
                                      ├─ local     nix-instantiate on PATH
                                      └─ docker    nixos/nix image
                    nixpkgs source cache: $OAR_CACHE_DIR/nixpkgs/<rev>/  (fetched by Go)
```

The builder never links the evaluator into its own call path. It runs
`oar nix-eval` as a child and parses its stdout. The child picks the backend.
One code path for all three backends, and the same command a user runs by
hand to see what the build sees.

### `oar nix-eval` (public command)

```
oar nix-eval [flags] ATTR...
oar nix-eval [flags] --expr EXPR

  --system SYS        x86_64-linux | aarch64-linux (default: host)
  --nixpkgs REV|DIR   40-char nixpkgs commit (default: runtime.nixpkgs of
                      oar.yaml, else spec.DefaultNixpkgsRevision), or a
                      local checkout for people hacking on nixpkgs
  --backend NAME      embedded | local | docker (default: $OAR_NIX, else
                      embedded when compiled in, else local, else docker)
  --workers N         evaluator threads (embedded only)
  --json              machine output; the default prints a table
```

With attributes it evaluates the builder's expression verbatim and prints
`{ "<attr>": { "outputs": [...], "mainProgram": "..." } }`. With `--expr`
it evaluates an arbitrary expression with `pkgs` in scope, for debugging.
Evaluation errors go to stderr with exit status 1. A crash of the embedded
evaluator is whatever the process died with (`SIGABRT`, status 1 from
`process::exit`), and its stderr is the message.

Debugging flow this enables: `oar nix-eval --backend local nodejs_22` next
to `oar nix-eval --backend embedded nodejs_22` and diff. `oar version`
reports the embedded iets revision.

### Builder side

`Builder.runNixEval` is replaced by one function: run
`os.Executable() nix-eval --json --system S --nixpkgs REV ATTR...` under
`exec.CommandContext(b.ctx)`, parse stdout, and on
a non-zero exit wrap the captured stderr into the error together with a hint
(`set OAR_NIX=local to use nix-instantiate`). The context cancels the child.
Nothing else in the builder changes, and the attribute cache in
`manifests/nix-eval.json` stays as it is.

Why a child process rather than calling the binding in the builder: every
process-ending path listed above becomes a reported error, the signal
handler swap and the reserved address space stay in a process that exits
right away, and cancellation works. The cost is one exec of the `oar` binary
per `(rev, system)` cache miss.

### `internal/nixeval` (Go)

```go
type Request struct {
    NixpkgsDir string   // absolute path of the extracted tree
    System     string   // x86_64-linux, aarch64-linux
    Attrs      []string // or Expr for --expr
    Expr       string
    Workers    int
    CacheDir   string
}
type Evaluator interface {
    Eval(ctx context.Context, req Request) (map[string]Result, error)
}
```

Backends `embedded`, `local` and `docker`; the last two are the code that
lives in `builder/nix.go` today, moved. `nixeval.Available(name)` says
whether a backend was compiled in or its tool is on `PATH`.

### `internal/nixeval/iets` (cgo binding)

Build-tagged `cgo && (linux || darwin)`. A `!cgo` stub reports the backend as
unavailable, so `CGO_ENABLED=0 go build` keeps working for contributors and
for the `windows/amd64` cross-compile in CI.

Per-platform link files hold the search path and system libraries, because
cgo directives cannot interpolate `GOOS`/`GOARCH`:

```go
// link_linux_amd64.go
// #cgo LDFLAGS: -L${SRCDIR}/lib/linux_amd64 -liets_ffi
// link_darwin_arm64.go
// #cgo LDFLAGS: -L${SRCDIR}/lib/darwin_arm64 -liets_ffi -framework Security -framework CoreFoundation
```

The Go side marshals the request to C strings, calls `oar_iets_eval`, copies
the JSON out and frees it. Nothing of iets's value API crosses the boundary.
The binding is called from the `nix-eval` process only; after the call that
process writes stdout and exits, so the handler swap never matters.

### `rust/iets-ffi` (Rust, `crate-type = ["staticlib"]`)

A thin crate that depends on `iets` by git revision and exports a C ABI:

```c
// iets_ffi.h
int32_t oar_iets_eval(const char *expr,          // Nix source
                      const char *system,        // "x86_64-linux"
                      const char *project_root,  // the nixpkgs dir
                      const char *cache_dir,     // IR cache, may be NULL
                      uint32_t    workers,
                      char      **json_out,      // strict JSON, caller frees
                      char      **error_out);    // rendered report, caller frees
void        oar_iets_free(char *p);
const char *oar_iets_version(void);              // "iets 0.1.0+bdd26f2c"
```

Per call, on a thread started with `stack_size(iets::api::MAIN_STACK)` and
joined before returning (the same thing the iets CLI does, and what keeps
iets's thread-local state and `sigaltstack` off the Go thread):
`Session::new()` without `load_settings`, so the user's `iets.toml` and
`nix.conf` never leak in; `EvalConfig::new().system(..).pure(true)
.project_root(..).session(..)`; `Run::over(config, &args)`; `run.go` with
`api::evaluate` and `print::print_json`; errors through `api::report_of`
into `error_out`. The tracing cache is off; the IR cache points at
`cache_dir`. `Run::on` rather than `Run::over`, so the lints stay off and no
`NIX_PATH` or `nix.conf` search path is read: that is how iets's own
`nix-instantiate` compatibility mode behaves. No signal juggling, no panic
catching: the process boundary handles both. About 200 lines
(`rust/iets-ffi/src/lib.rs`).

### nixpkgs source cache

The Go side downloads `https://github.com/NixOS/nixpkgs/archive/<rev>.tar.gz`
and extracts it to `<cache>/nixpkgs/<rev>/` through a temporary directory
and a final rename, so a directory that exists is complete. `<cache>` is
`OAR_CACHE_DIR`, else the user cache directory plus `oar`: a checkout is a
build input every project shares, not an artifact, and a nixpkgs tree holds
Go files that would break `go test ./...` of any module the artifacts
directory sits in. Later builds at the same revision skip the download.
`OAR_NIXPKGS_MIRROR` overrides the URL prefix. The tree is around 200 MB on
disk per revision; the `fetchTarball` path before cost the same inside
`/nix/store` or the Docker volume.

The iets IR cache lives at `<cache>/iets/` so a second evaluation at the
same revision compiles no nixpkgs file twice.

### Verification

- Unit: the binding evaluates `{ a = 1; }` with no nixpkgs, called directly
  in the test process. Proves linkage on every platform in the unit matrix.
- Integration: `oar nix-eval --json hello nodejs_22` at
  `DefaultNixpkgsRevision` for both systems, then check that every output
  path has a narinfo on `cache.nixos.org`. This is the oracle described
  above and needs no Nix.
- Parity: the `e2e-macos` job already installs Nix. Add a step that runs
  `oar nix-eval --backend local` and `--backend embedded` on the same
  attributes and diffs the JSON.
- Crash path: a test that makes the child die (an attribute that throws, and
  a fake `OAR_NIX_CRASH=1` the child honours) and checks the builder's error
  text.
- The existing `hack/e2e.sh` runs unchanged with `OAR_NIX` unset, so the
  embedded backend is what the acceptance suite exercises.

## Build and distribution

| Go target | Rust target | C toolchain for `go build` |
|---|---|---|
| linux/amd64 | `x86_64-unknown-linux-musl` | `zig cc -target x86_64-linux-musl` |
| linux/arm64 | `aarch64-unknown-linux-musl` | `zig cc -target aarch64-linux-musl` |
| darwin/arm64 | `aarch64-apple-darwin` | Xcode clang (already required for VZ) |
| darwin/amd64 | `x86_64-apple-darwin` | Xcode clang |
| windows/amd64 | `x86_64-pc-windows-gnu` | mingw-w64, deferred |

musl on Linux keeps `bin/oar` the fully static binary it is today
(`-ldflags '-linkmode external -extldflags -static'`). iets measured musl at
9–13 % slower than glibc, which is irrelevant for a few attributes. `zig cc`
gives one cross-compiling C toolchain on every CI runner; `musl-gcc` works
too on a Linux host.

Pieces:

- `rust/iets-ffi/`: `Cargo.toml` (iets pinned by `rev`, committed
  `Cargo.lock`, `rust-toolchain.toml`, `[profile.release] lto = "fat"`,
  `panic = "abort"`, `strip = true`), `src/lib.rs`, `include/iets_ffi.h`.
- `hack/build-iets.sh <goos>/<goarch>`: `cargo build --release --target …`,
  copies the `.a` and header into `internal/nixeval/iets/lib/<goos>_<goarch>/`
  and records its sha256 in `internal/nixeval/iets/iets.lock` next to the
  iets revision.
- `.github/workflows/iets-lib.yml`: matrix over the four targets, keyed on
  the iets revision and the crate hash, publishes the `.a` files as release
  assets under a tag like `iets-lib-<rev>`.
- `hack/fetch-iets.sh`: downloads the assets for the host (or a requested
  target), verifies the sha256 from `iets.lock`. `hack/build.sh` calls it and
  falls back to `build-iets.sh` when `cargo` is present and the download
  fails. The `.a` files stay out of git (`lib/` is ignored).
- CI `unit` job: fetch the libs, build with cgo on Linux and macOS, keep one
  `CGO_ENABLED=0` build to prove the stub compiles.

Expected cost, to be measured in the spike: the library pulls in tokio, hyper,
rustls and tonic even though only the evaluator is called; with fat LTO and
`--gc-sections` the binary should grow by roughly 10–20 MB. If that is too
much, the fix is an `eval-only` cargo feature in iets, which is a small
upstream change its layering already supports.

## Licensing

iets is LGPL-2.1-or-later; oar is MIT. Static linking is allowed by LGPL
section 6 when the recipient can relink the application against a modified
library. oar's source is public, so a recipient can rebuild it with any iets.
To comply: ship iets's `COPYING` and a notice in `third_party/iets/`, name
the pinned revision in the notice, and mention the dependency in the README.
Do not relicense oar. This is a reading of the license, not legal advice.

## Risks

| Risk | Mitigation |
|---|---|
| iets API changes under us | pinned revision, a 100-line FFI crate that is the only consumer |
| An attribute evaluates differently from Nix | binary-cache oracle fails the build before a wrong path ships; parity step in CI; `oar nix-eval --backend local` to compare by hand |
| A nixpkgs revision uses a builtin iets lacks | `local`/`docker` backends stay; the error names the builtin |
| Rust abort, `process::exit`, stack overflow in the evaluator | child process; the builder reports exit status and stderr |
| Signal handlers replaced by iets | confined to the `nix-eval` process, which exits right after |
| Cross-compiling cgo | prebuilt libraries from CI, `zig cc` as the one C toolchain |
| Binary size | measure in the spike; upstream `eval-only` feature if needed |
| Windows | stub backend now, `windows-gnu` library later; WSL2 is experimental anyway |

## Where things are

| Piece | Path |
|---|---|
| FFI crate, header, pinned iets revision | `rust/iets-ffi/` |
| Go backends, child runner, source cache | `internal/nixeval/` |
| cgo binding (build tag `iets`) and its stub | `internal/nixeval/iets/` |
| `oar nix-eval` | `internal/cli/nixevalcmd.go` |
| builder call site | `internal/builder/nix.go` (`evalPackages`) |
| build, fetch, library workflow | `hack/build.sh`, `hack/build-iets.sh`, `hack/fetch-iets.sh`, `.github/workflows/iets-lib.yml` |
| license and notice | `third_party/iets/` |

The Go tree builds and tests without the library; `-tags iets` links it. On
Linux `hack/build.sh` builds against glibc and links the whole binary
statically, as the evaluator-less binary was. The musl target of the table
above is available through `OAR_IETS_TARGET` and a musl C toolchain, and is
not the default.

## Measured

First run on a Linux x86_64 laptop, nixpkgs `8ce73368` (nixos-26.05):

| What | Value |
|---|---|
| `libiets_ffi.a` (release, fat LTO, stripped) | 24 MB |
| `oar` binary growth, stripped | about 5 MB |
| nixpkgs archive download and extraction, 52,924 files | 3.6 s |
| `hello` + `nodejs_22`, cold evaluator cache | under 1 s |
| `python313` + `git` + `ripgrep`, warm cache, whole `oar nix-eval` process | 0.27 s |
| store paths of five attributes against `nix-instantiate` in Docker | identical |
| `hack/e2e.sh` on the native backend | pass |

## Later

- Windows library (`x86_64-pc-windows-gnu`, mingw cgo).
- An upstream `eval-only` cargo feature in iets to leave tokio, hyper,
  rustls and tonic out of the library.
- An optional `nixpkgs_sha256` in `oar.yaml` for a verified source tree.
- Decide whether the Docker backend is still worth its place in the matrix.
