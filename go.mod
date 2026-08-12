module github.com/brig-sh/hull

go 1.26.4

require (
	github.com/compose-spec/compose-go/v2 v2.14.0
	github.com/containerd/containerd v1.7.33
	github.com/containers/gvisor-tap-vsock v0.8.9
	github.com/google/go-containerregistry v0.20.1
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510
	github.com/opencontainers/runtime-spec v1.2.1
	github.com/sirupsen/logrus v1.9.4
	github.com/urfave/cli/v3 v3.10.0
	github.com/urunc-dev/urunc v0.0.0
	golang.org/x/sys v0.46.0
	golang.org/x/term v0.44.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/Microsoft/hcsshim v0.13.0 // indirect
	github.com/apparentlymart/go-cidr v1.1.1 // indirect
	github.com/cavaliergopher/cpio v1.0.1 // indirect
	github.com/containerd/cgroups/v3 v3.1.0 // indirect
	github.com/containerd/continuity v0.5.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/containerd/stargz-snapshotter/estargz v0.14.3 // indirect
	github.com/containerd/typeurl/v2 v2.3.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/cli v24.0.0+incompatible // indirect
	github.com/docker/distribution v2.8.3+incompatible // indirect
	github.com/docker/docker v27.3.1+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.7.0 // indirect
	github.com/docker/go-connections v0.4.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/elastic/go-seccomp-bpf v1.6.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/inetaf/tcpproxy v0.0.0-20250222171855-c4b9df066048 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20240710054256-ddd8a41251c9 // indirect
	github.com/jackpal/gateway v1.2.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/moby/sys/mount v0.3.5 // indirect
	github.com/moby/sys/mountinfo v0.7.2 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/nubificus/hedge_cli v0.0.3 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.1 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	github.com/vbatts/tar-split v0.11.3 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/xhit/go-str2duration/v2 v2.1.0 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.4 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260622175928-b703f567277d // indirect
	google.golang.org/grpc v1.81.1 // indirect
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af // indirect
	gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f // indirect
)

replace github.com/urunc-dev/urunc => github.com/nofireai/urunc v0.7.1-0.20260718005719-9ba1e785a2d2
