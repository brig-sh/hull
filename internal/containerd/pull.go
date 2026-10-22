// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package containerd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	clog "github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("subsystem", "containerd")

// NormalizeRef turns an image reference as a user writes it, such as
// "busybox", into the full name containerd stores it under,
// "docker.io/library/busybox:latest". containerd's resolver needs the
// registry host spelled out, and `ctr` and nerdctl store images under the
// same full names, so all three find each other's images.
func NormalizeRef(ref string) (string, error) {
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	return named.String(), nil
}

// Pull fetches the image ref names for platform into containerd's content
// store, unpacks it into the default snapshotter, and describes it as that
// platform's image. Progress, when not nil, receives a report of the
// download as it goes.
//
// The pull runs in hull's process, through containerd's client, rather than
// through containerd's transfer service, which would fetch from the daemon.
// The daemon has none of the user's registry credentials; hull has them
// through the Docker credential helpers, as on macOS.
//
// A platform must match exactly, variant included, so an index with no
// image for it fails the pull rather than falling back to a compatible one,
// as the macOS pull does.
func Pull(ctx context.Context, c *client.Client, ref string, platform ocispec.Platform, progress io.Writer) (Image, error) {
	name, err := NormalizeRef(ref)
	if err != nil {
		return Image{}, err
	}
	matcher := platforms.OnlyStrict(platform)
	opts := []client.RemoteOpt{
		client.WithResolver(newResolver(authn.DefaultKeychain)),
		client.WithPlatformMatcher(matcher),
		client.WithPullUnpack,
	}
	prog := newPullProgress(progress)
	if prog != nil {
		opts = append(opts, client.WithImageHandlerWrapper(prog.wrap))
	}
	stopWatching := prog.watch(ctx, c.ContentStore())
	img, err := c.Pull(quietInfo(ctx), name, opts...)
	// Stop the redraws before the final line, so none lands after it. A
	// failed pull ends its line too, so the error starts on a line of its
	// own.
	stopWatching()
	prog.done(err == nil)
	if err != nil {
		return Image{}, fmt.Errorf("failed to pull image %s: %w", ref, err)
	}
	return DescribePlatform(ctx, c.ContentStore(), img.Metadata(), matcher)
}

// quietInfo returns ctx with a logger for containerd's client that drops
// info messages unless hull logs at debug level. The client logs a failed
// registry request at info, and the same failure comes back as the pull's
// error, so without this it is printed twice. Warnings and errors still
// show.
func quietInfo(ctx context.Context) context.Context {
	std := logrus.StandardLogger()
	if std.GetLevel() != logrus.InfoLevel {
		return ctx
	}
	quiet := logrus.New()
	quiet.SetOutput(std.Out)
	quiet.SetFormatter(std.Formatter)
	quiet.SetLevel(logrus.WarnLevel)
	return clog.WithLogger(ctx, logrus.NewEntry(quiet))
}

// newResolver returns a registry resolver that authenticates with the
// credentials keychain holds, the Docker config and its credential helpers
// for authn.DefaultKeychain.
func newResolver(keychain authn.Keychain) remotes.Resolver {
	creds := &registryCreds{keychain: keychain}
	secure := registryClient(false)
	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthClient(secure),
		docker.WithAuthCreds(creds.userSecret),
	)
	tlsOnly := docker.ConfigureDefaultRegistries(
		docker.WithAuthorizer(authorizer),
		docker.WithClient(secure),
	)
	withFallback := docker.ConfigureDefaultRegistries(
		docker.WithAuthorizer(authorizer),
		docker.WithClient(registryClient(true)),
	)
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: func(host string) ([]docker.RegistryHost, error) {
			configure := tlsOnly
			if allowsPlainHTTP(host) {
				configure = withFallback
			}
			hosts, err := configure(host)
			if err != nil {
				return nil, err
			}
			// A registry token is a bearer token the Docker config holds
			// for the registry itself, and the macOS pull sends it on every
			// request. containerd's credentials callback can only return a
			// username and secret, so it goes out as a header of the host
			// instead. containerd's authorizer replaces Authorization only
			// after the registry answers 401, which a valid token never
			// draws, and net/http drops the header on a redirect to another
			// host, such as a blob store.
			cfg, err := creds.lookup(host)
			if err != nil {
				return nil, err
			}
			if cfg.RegistryToken != "" {
				for i := range hosts {
					hosts[i].Header = http.Header{"Authorization": {"Bearer " + cfg.RegistryToken}}
				}
			}
			return hosts, nil
		},
	})
}

// registryClient pins registry traffic to HTTP/1.1. Large layers pulled from
// ghcr over Go's HTTP/2 stack fail part-way with a stream PROTOCOL_ERROR; see
// registryTransport in pkg/ociclient, which the macOS pull uses for the same
// reason. Setting TLSClientConfig already keeps net/http from negotiating
// HTTP/2; the ALPN list says so outright.
//
// With httpFallback, a request whose TLS handshake fails is retried over
// plain HTTP, for the registries allowsPlainHTTP names.
func registryClient(httpFallback bool) *http.Client {
	var t http.RoundTripper = docker.DefaultHTTPTransport(&tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if httpFallback {
		t = docker.NewHTTPFallback(t)
	}
	return &http.Client{Transport: t}
}

// allowsPlainHTTP reports whether a registry may be reached over plain HTTP
// when HTTPS fails. The macOS pull goes through go-containerregistry, which
// tries HTTPS and then HTTP for loopback addresses, localhost, RFC 1918
// addresses and .local names. The same rule applies here, taken from the
// same code, so a local registry that works on one OS works on the other.
func allowsPlainHTTP(host string) bool {
	reg, err := name.NewRegistry(host)
	if err != nil {
		return false
	}
	return reg.Scheme() == "http"
}

// registryCreds looks up registry credentials in a go-containerregistry
// keychain, once per registry for the life of a resolver. containerd asks for
// a host's configuration on every request, and a credential helper that runs
// each time can be slow or prompt the user.
//
// A credential helper that fails, as Docker's does in a headless session
// with a locked keychain, does not fail the pull: the requests go out with
// no credentials, so a public image still pulls. The macOS pull retries the
// same way, and on the same test of the error. Any other keychain error,
// such as a Docker config that does not parse, still fails the pull.
type registryCreds struct {
	keychain authn.Keychain

	mu   sync.Mutex
	cfgs map[string]*authn.AuthConfig // by go-containerregistry's registry name
}

// lookup returns the credentials for host, which are empty when the keychain
// has none.
func (r *registryCreds) lookup(host string) (*authn.AuthConfig, error) {
	// containerd sends Docker Hub's pulls to registry-1.docker.io, but
	// `docker login` files Hub's credentials under index.docker.io.
	if host == "registry-1.docker.io" {
		host = name.DefaultRegistry
	}
	reg, err := name.NewRegistry(host)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", host, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.cfgs[reg.RegistryStr()]
	if ok {
		return cfg, nil
	}
	auth, err := r.keychain.Resolve(reg)
	if err == nil {
		cfg, err = auth.Authorization()
	}
	if err != nil {
		if !strings.Contains(err.Error(), "error getting credentials") {
			return nil, fmt.Errorf("credentials for %s: %w", host, err)
		}
		log.Debugf("credential store unavailable, pulling from %s anonymously: %v", host, err)
		cfg = &authn.AuthConfig{}
	}
	if r.cfgs == nil {
		r.cfgs = map[string]*authn.AuthConfig{}
	}
	r.cfgs[reg.RegistryStr()] = cfg
	return cfg, nil
}

// userSecret is the credentials callback containerd's authorizer takes,
// which is given a registry host and returns a username and secret. An empty
// username with a secret makes the secret an identity token, which is how
// containerd takes the token `docker login` stores for registries that hand
// one out.
func (r *registryCreds) userSecret(host string) (string, string, error) {
	cfg, err := r.lookup(host)
	if err != nil {
		return "", "", err
	}
	if cfg.IdentityToken != "" {
		return "", cfg.IdentityToken, nil
	}
	return cfg.Username, cfg.Password, nil
}
