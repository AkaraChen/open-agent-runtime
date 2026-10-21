# Configuration reference

A runtime is described by a single YAML file, `oar.yaml` by default (select another with `-f`).

```yaml
runtime:
  base: debian:trixie-slim        # OCI image; or debian:trixie-slim@sha256:...
  platform: linux/arm64           # default platform
  platforms: [linux/amd64, linux/arm64]   # build artifacts for all of these
  nixpkgs: 8ce73368d1efd4d657e9064d29b144a52046b4e8   # full commit hash
  hostname: oar

components:                       # an unordered set
  - nix:nodejs_22                 # any nixpkgs attribute (dotted paths allowed)
  - nix:hello@b6018f87da91d19d0ab4cf979885689b469cdd41   # pinned to its own revision
  - binary:my-agent
  - file:app-config
  - dir:assets

sources:
  my-agent:
    path: "dist/my-agent-{{arch}}"
    target: /opt/my-agent
    build: [go, build, -o, "dist/my-agent-{{arch}}", ./my-agent]
    build_env: {GOOS: linux, GOARCH: "{{arch}}", CGO_ENABLED: "0"}
  app-config:
    target: /etc/app/config.json
    content: |
      {"listen": ":8080"}
  assets:
    path: ./assets
    target: /srv/assets

process:
  command: ["/opt/my-agent"]
  env: {LOG_LEVEL: info}
  cwd: /
  user: "0:0"
  ready:
    log: "serving on"
    timeout: 10s
```

## `runtime`

| Field | Description |
|---|---|
| `base` | OCI base image. The tag is resolved to a digest on first build and pinned in the lock. Re-resolve it with `oar build --update`. |
| `platform` | Default platform (`linux/amd64` or `linux/arm64`). `oar run` always uses the guest's architecture. |
| `platforms` | Additional platforms to build artifacts for. |
| `nixpkgs` | nixpkgs commit used for `nix:` components. It must be a full 40-character hash: branch names and tags are rejected because they move. It defaults to a pinned nixos-26.05 commit. |
| `hostname` | Container hostname (default `oar`). |
| `nixpkgs_mirror` | Base URL serving `<commit>.tar.gz` nixpkgs archives, for networks that cannot reach GitHub (default `https://github.com/NixOS/nixpkgs/archive`). |
| `substituters` | Binary caches store paths are fetched from (default `https://cache.nixos.org`). |
| `trusted_keys` | Public keys the fetched store paths must be signed with (default the `cache.nixos.org` key). Every path is also checked against its NarHash. |

## `components`

An unordered set of references, written `kind:name`. Declaration order never affects the result.

| Kind | Meaning |
|---|---|
| `nix:<attr>` | A nixpkgs package and its complete runtime closure. Its `bin/` goes on PATH. |
| `nix:<attr>@<commit>` | The same package, evaluated at its own nixpkgs commit. |
| `binary:<name>` | A single executable from `sources.<name>`. Default target `/opt/<name>`, mode `0755`. |
| `file:<name>` | A single file from `sources.<name>`. Mode is kept from the source, or `0644` for inline content. |
| `dir:<name>` | A directory tree from `sources.<name>`. |

## `sources`

Defines where `binary:`, `file:` and `dir:` components come from.

| Field | Description |
|---|---|
| `path` | File or directory, relative to the config file. `{{arch}}` and `{{os}}` are expanded per platform. |
| `content` | Literal file content, used instead of `path` (`file:` and `binary:` only). It is written verbatim, with no placeholder expansion. |
| `target` | Absolute path in the container. |
| `mode` | Octal file mode, e.g. `"0755"`. |
| `build` | Command run before import, once per platform. Placeholders are expanded, and `OAR_ARCH`, `OAR_OS` and `OAR_PLATFORM` are set. |
| `build_env` | Extra environment for `build`. Placeholders are expanded. |
| `env` | Environment variables contributed to the container. Two components setting different values is an error. |
| `path_entries` | Absolute directories added to PATH. |
| `requires` | Other components this one needs. They are added automatically. |
| `override` | Allow replacing files that already exist in the base image. |

## `process`

| Field | Description |
|---|---|
| `command` | Entry point and arguments. An absolute entry point must exist in the composed filesystem. |
| `env` | Environment variables. These take precedence over component and base values. |
| `cwd` | Working directory (default `/`). |
| `user` | Numeric `uid[:gid]` (default `0:0`). |
| `ports` | Ingress ports: `- {name: http, port: 8080, env: PORT}`. Each instance listens on a private port passed in `env`; oar serves the public `port` and switches it atomically between instances. It is not part of the bundle digest. |
| `snapshot` | `true` checkpoints the first ready instance of a bundle (CRIU) and restores later instances from it, skipping application start-up. It requires `criu` on native hosts (bundled in the guest image), and falls back to a normal start when unavailable. It is not part of the bundle digest. |
| `ready` | Readiness check used by `run` and `reload`. Set exactly one of `tcp: host:port`, `exec: [cmd, ...]` (exit status 0) or `log: "substring"` (container output). `timeout` defaults to 30s. It is not part of the bundle digest. |

When a service binds with `SO_REUSEPORT`, prefer a `log` or `exec` check. During an overlapping
reload, a `tcp` check would also be answered by the old instance.

## Validation rules

The builder fails, rather than silently picking a winner, when:

- two components provide the same file;
- a component would replace a base file (unless `override: true`) or shadow a base symlink;
- two Nix packages provide the same binary name, including the same attribute pinned at two
  revisions;
- two components set the same environment variable to different values;
- an imported ELF binary targets the wrong architecture, or needs an interpreter that is missing;
- a component writes under `/nix`, `/proc`, `/sys` or `/dev`.

See [`examples/`](../examples) for complete configurations.
