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
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/brig-sh/hull/internal/pullprogress"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// pullProgress counts how far a pull through containerd has got, for the
// progress line the macOS pull also draws.
//
// The macOS pull counts bytes as it extracts them. Here the daemon unpacks
// and reports nothing to the client while it does, so the count is of bytes
// downloaded, compressed, the same unit as the image size beside it. The
// daemon downloads several layers at once, so the line counts the layers
// finished rather than naming the one in flight.
type pullProgress struct {
	line *pullprogress.Line

	mu       sync.Mutex
	layers   map[digest.Digest]int64  // size of each layer of the manifest pulled
	refs     map[string]digest.Digest // the ingest ref each layer downloads under
	total    int64
	finished map[digest.Digest]bool
	inFlight map[digest.Digest]int64 // bytes of each layer written to the content store so far
}

// newPullProgress returns a reporter writing to w, or nil for a nil w. A nil
// *pullProgress is safe to use and reports nothing.
func newPullProgress(w io.Writer) *pullProgress {
	if w == nil {
		return nil
	}
	return &pullProgress{
		line:     pullprogress.New(w),
		layers:   map[digest.Digest]int64{},
		refs:     map[string]digest.Digest{},
		finished: map[digest.Digest]bool{},
		inFlight: map[digest.Digest]int64{},
	}
}

// wrap is an images.HandlerWrapper. It learns the layers from the manifest's
// children and marks a layer finished when the fetch handler returns for
// it. The unpacker fetches layers through this same handler, so they pass
// through here even though it takes them out of the manifest's children.
func (p *pullProgress) wrap(h images.Handler) images.Handler {
	return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		children, err := h.Handle(ctx, desc)
		if err != nil {
			return children, err
		}
		switch {
		case images.IsManifestType(desc.MediaType):
			p.mu.Lock()
			for _, child := range children {
				if images.IsLayerType(child.MediaType) {
					p.layers[child.Digest] = child.Size
					p.refs[remotes.MakeRefKey(ctx, child)] = child.Digest
				}
			}
			p.total = 0
			for _, size := range p.layers {
				p.total += size
			}
			p.mu.Unlock()
		case images.IsLayerType(desc.MediaType):
			p.mu.Lock()
			p.finished[desc.Digest] = true
			p.draw()
			p.mu.Unlock()
		}
		return children, nil
	})
}

// watch redraws the line at the line's refresh interval until the returned
// stop is called. A layer being downloaded is an ingest in the content store,
// and its offset there is how far the download has got. The ingest is found
// by its ref, the key the fetch handler names it by: the content store does
// not report which digest an ingest expects.
func (p *pullProgress) watch(ctx context.Context, cs content.Store) (stop func()) {
	if p == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(p.line.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			statuses, err := cs.ListStatuses(ctx)
			if err != nil {
				continue
			}
			p.mu.Lock()
			clear(p.inFlight)
			for _, s := range statuses {
				d, ours := p.refs[s.Ref]
				if ours {
					p.inFlight[d] = s.Offset
				}
			}
			if len(p.layers) > 0 {
				p.draw()
			}
			p.mu.Unlock()
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// downloaded must be called with the lock held. A finished layer counts at
// its full size, never at the offset the last poll read for it: the poll may
// have caught it part-way, and it finished since.
func (p *pullProgress) downloaded() int64 {
	var n int64
	for d, size := range p.layers {
		if p.finished[d] {
			n += size
			continue
		}
		n += p.inFlight[d]
	}
	return n
}

// draw must be called with the lock held. It always forces the draw past the
// line's rate limit. A finished layer must show, and watch's ticker already
// paces its own draws: a tick that lands a little short of the interval
// after a layer's draw would otherwise be dropped, skipping a whole refresh.
func (p *pullProgress) draw() {
	p.line.Draw(fmt.Sprintf("pull: %d/%d layers, %s of %s downloaded",
		len(p.finished), len(p.layers), pullprogress.HumanBytes(p.downloaded()), pullprogress.HumanBytes(p.total)), true)
}

// done ends the line with a final count. A pull that fetched no layer,
// because the image was already unpacked, printed nothing and prints nothing
// here.
//
// complete says the pull succeeded, and then every layer counts as in place.
// The handler never sees some of them finish: containerd's unpacker does not
// download a layer whose snapshot already exists, such as a base layer an
// image shares with one pulled before. The bytes count only what this pull
// fetched.
func (p *pullProgress) done(complete bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	finished := len(p.finished)
	if complete {
		finished = len(p.layers)
	}
	p.line.Done(fmt.Sprintf("pull: %d/%d layers, %s downloaded",
		finished, len(p.layers), pullprogress.HumanBytes(p.downloaded())))
}
