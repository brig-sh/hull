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

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// gatewaySocketGreeting is written by a staged listener to every connection
// it accepts. Reading it back proves a dial reached that exact listener and
// not a replacement bound at the same path.
const gatewaySocketGreeting = "live-gateway"

// shortSocketDir returns a directory whose paths fit in sun_path.
//
// Deliberately not t.TempDir(): TMPDIR on darwin is already 49 bytes before
// the test name and the /001 element, and claimUnixSocket calls
// checkUnixSocketPath first, so an over-long path would fail on length
// before the dial policy is ever reached and every assertion below would
// pass for the wrong reason. oneshot_prewarm_test.go:73 does the same thing
// for the same reason.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hull-gw-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		// Restore search permission first: a case that denies it would
		// otherwise leave a directory the cleanup cannot walk.
		_ = os.Chmod(dir, 0o700)
		_ = os.RemoveAll(dir)
	})
	return dir
}

// socketPath joins name onto dir and asserts up front that the result is a
// path claimUnixSocket will actually try to dial, so a future TMPDIR change
// fails loudly here instead of quietly turning these into length tests.
func socketPath(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if len(p) > unixSocketPathMax {
		t.Fatalf("test socket path is %d bytes, over the %d-byte sun_path budget: %s", len(p), unixSocketPathMax, p)
	}
	return p
}

// serveGreeting answers every connection on l with gatewaySocketGreeting.
func serveGreeting(l net.Listener) {
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(gatewaySocketGreeting))
			_ = c.Close()
		}
	}()
}

// liveGatewaySocket stages a real listening socket at path, standing in for
// a gateway that is up and serving.
func liveGatewaySocket(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener at %s is %T, want *net.UnixListener", path, l)
	}
	t.Cleanup(func() { _ = ul.Close() })
	serveGreeting(ul)
	return ul
}

// staleSocketFile leaves a socket file at path with no listener behind it:
// what a crashed gateway leaves on disk. Dialling it is refused.
func staleSocketFile(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener at %s is %T, want *net.UnixListener", path, l)
	}
	ul.SetUnlinkOnClose(false)
	if err := ul.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket file must survive its listener for this case: %v", err)
	}
}

// socketInode identifies the file at path. A path that is unlinked and then
// re-bound keeps its name but gets a new inode, so comparing inodes catches
// a replacement that a plain existence check would miss.
func socketInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat of %s is %T, want *syscall.Stat_t", path, fi.Sys())
	}
	return uint64(st.Ino)
}

// closeReturnedListener disposes of whatever claimUnixSocket handed back
// without unlinking the path, so the state assertions that follow see what
// the call actually left on disk.
func closeReturnedListener(t *testing.T, l net.Listener) {
	t.Helper()
	if l == nil {
		return
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close()
}

// assertStagedListenerIntact is the observable that matters when
// claimUnixSocket refuses: the socket file is still the same file, and the
// process that was serving it is still reachable through it.
func assertStagedListenerIntact(t *testing.T, path string, wantInode uint64) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket %s was removed: %v", path, err)
	}
	if got := socketInode(t, path); got != wantInode {
		t.Errorf("socket %s was replaced: inode %d, want %d", path, got, wantInode)
	}
	assertGreets(t, path)
}

// assertGreets dials path and reads the greeting, proving a live listener is
// serving that path right now.
func assertGreets(t *testing.T, path string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, len(gatewaySocketGreeting))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read greeting from %s: %v", path, err)
	}
	if string(buf) != gatewaySocketGreeting {
		t.Errorf("greeting from %s = %q, want %q", path, buf, gatewaySocketGreeting)
	}
}

// Regression: claimUnixSocket must not take a socket another gateway is
// serving. The assertions are on state rather than on the error, so the test
// still fails if a future version returns an error but unlinks the socket on
// the way out.
func TestClaimUnixSocketRefusesALiveListener(t *testing.T) {
	path := socketPath(t, shortSocketDir(t), "live.sock")
	liveGatewaySocket(t, path)
	before := socketInode(t, path)

	l, err := claimUnixSocket(path)
	closeReturnedListener(t, l)
	if err == nil {
		t.Errorf("claimUnixSocket took a socket with a live listener")
	}
	assertStagedListenerIntact(t, path, before)
}

// Regression: a socket file whose listener is gone is the case the whole
// dial dance exists to recover from. Refusing every unreachable socket would
// be safe and useless: after a gateway crash the project could never come
// back up. The claimed listener must actually serve, not merely be non-nil.
func TestClaimUnixSocketClaimsAStaleSocketFile(t *testing.T) {
	path := socketPath(t, shortSocketDir(t), "stale.sock")
	staleSocketFile(t, path)

	l, err := claimUnixSocket(path)
	if err != nil {
		t.Fatalf("a refused socket is stale and must be claimable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	serveGreeting(l)
	assertGreets(t, path)
}

// Regression: the ordinary first start. ENOENT means nothing is at the path,
// so there is nothing to remove and the listener must simply be created.
func TestClaimUnixSocketCreatesAMissingSocket(t *testing.T) {
	path := socketPath(t, shortSocketDir(t), "fresh.sock")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path must not exist for this case: %v", err)
	}

	l, err := claimUnixSocket(path)
	if err != nil {
		t.Fatalf("claimUnixSocket on a free path: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	serveGreeting(l)
	assertGreets(t, path)
}

// Regression, and the reason this file exists: a dial that fails with
// EACCES is not proof that the socket is stale. The old code checked only
// err == nil and removed the file on every other outcome, so a live gateway
// whose socket mode had been changed was unlinked out from under itself and
// a second gateway bound the name. Unlink is governed by the containing
// directory, not by the file's own mode, so the removal succeeded: the
// socket survived only by luck on hosts where the directory also denied us.
//
// The state assertions carry this test. Under the old policy the returned
// error is nil AND the inode at the path is a different one AND the original
// listener is no longer reachable through its own address.
func TestClaimUnixSocketRefusesASocketItCannotDial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so no EACCES can be constructed")
	}
	path := socketPath(t, shortSocketDir(t), "denied.sock")
	liveGatewaySocket(t, path)
	before := socketInode(t, path)

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	// Precondition: this host must really deny the dial. If it does not,
	// skip with the reason rather than pretend the case was exercised.
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = conn.Close()
		t.Skip("this host dials a 0000-mode socket successfully; EACCES cannot be constructed here")
	} else if !errors.Is(err, syscall.EACCES) {
		t.Skipf("dial of a 0000-mode socket returned %v, not EACCES; the permission case cannot be constructed here", err)
	}

	l, err := claimUnixSocket(path)
	closeReturnedListener(t, l)
	if err == nil {
		t.Errorf("claimUnixSocket treated a permission-denied dial as proof the socket is stale")
	}

	// Restore access so the state can be observed at all.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restore mode on %s: %v", path, err)
	}
	assertStagedListenerIntact(t, path, before)
}

// Regression: ENOTSOCK is not staleness either. A regular file at the socket
// path is not something to delete on the way to binding a name; the old code
// removed it and listened. Nothing hull writes belongs there, which is
// exactly why the operator should be told instead of having it erased.
func TestClaimUnixSocketRefusesANonSocketPath(t *testing.T) {
	path := socketPath(t, shortSocketDir(t), "notasocket")
	const content = "not a socket, do not delete"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	l, err := claimUnixSocket(path)
	closeReturnedListener(t, l)
	if err == nil {
		t.Errorf("claimUnixSocket bound over a path holding a regular file")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the file at %s no longer reads back: %v", path, readErr)
	}
	if string(got) != content {
		t.Errorf("file at %s = %q, want %q", path, got, content)
	}
}
