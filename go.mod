module github.com/AkaraChen/open-agent-runtime

go 1.26.0

require (
	github.com/Code-Hex/vz/v3 v3.8.0
	github.com/google/go-containerregistry v0.22.1
	github.com/klauspost/compress v1.20.3
	github.com/mdlayher/vsock v1.3.0
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/spf13/cobra v1.10.2
	github.com/ulikunitz/xz v0.5.17
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

replace github.com/Code-Hex/vz/v3 => github.com/norio-nomura/vz/v3 v3.7.2-0.20260210033012-e27a5fb55e59
