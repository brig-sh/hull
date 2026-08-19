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

package bootassets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// stubCosign replaces the two seams the checker runs through, so the whole
// decision table can be driven on a host with or without a real cosign.
func stubCosign(t *testing.T, found bool, out string, err error) {
	t.Helper()
	oldLook, oldRun := verifyLookPath, verifyRun
	t.Cleanup(func() { verifyLookPath, verifyRun = oldLook, oldRun })
	verifyLookPath = func(string) (string, error) {
		if !found {
			return "", errors.New("exec: \"cosign\": executable file not found in $PATH")
		}
		return "/usr/local/bin/cosign", nil
	}
	verifyRun = func(context.Context, string, ...string) (string, error) { return out, err }
}

// exitErr is a real *exec.ExitError carrying the given code.
//
// It runs a process to get one, because that is what the classifier reads and
// a hand-rolled errors.New("exit status 12") is precisely the fiction that let
// the old string-matching taxonomy look correct in tests while being wrong
// against the real binary.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("wanted an ExitError for code %d, got %v", code, err)
	}
	if ee.ExitCode() != code {
		t.Fatalf("exit code = %d, want %d", ee.ExitCode(), code)
	}
	return err
}

func testPolicy() VerifyPolicy {
	p := DefaultVerifyPolicy()
	p.Timeout = time.Second
	return p
}

// TestVerifyBundleVerifiesAPublishedBundle is the control. Without it, a
// checker that refused everything would look like it was catching something.
func TestVerifyBundleVerifiesAPublishedBundle(t *testing.T) {
	stubCosign(t, true, "Verification for ghcr.io/nofireai/hull-assets...\n", nil)
	res := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if res.Outcome != BundleVerified {
		t.Fatalf("outcome = %v, want BundleVerified (%s)", res.Outcome, res.Message())
	}
	if res.Digest != testDigest {
		t.Fatalf("Digest = %q, want the digest that was checked", res.Digest)
	}
	for _, mode := range []VerifyMode{VerifyOff, VerifyWarn, VerifyRequire} {
		if err := res.Err(mode); err != nil {
			t.Errorf("a verified bundle should pass under %s, got: %v", mode, err)
		}
	}
}

// TestVerifyBundleChecksTheDigestNotTheTag: the tag is what we asked for, the
// digest is what we got. If cosign were pointed at the tag, a tag that moved
// between the pull and the check would be verified while a different bundle
// sat on disk.
func TestVerifyBundleChecksTheDigestNotTheTag(t *testing.T) {
	var subject string
	oldLook, oldRun := verifyLookPath, verifyRun
	t.Cleanup(func() { verifyLookPath, verifyRun = oldLook, oldRun })
	verifyLookPath = func(string) (string, error) { return "/usr/local/bin/cosign", nil }
	verifyRun = func(_ context.Context, _ string, args ...string) (string, error) {
		subject = args[len(args)-1]
		return "", nil
	}
	testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if subject != DefaultRepo+"@"+testDigest {
		t.Fatalf("cosign was pointed at %q, want the digest-pinned reference", subject)
	}
}

// TestVerifyBundleFailsClosedOnASignatureThatDoesNotVerify is the finding this
// file exists for.
//
// hull-assets has been publishing keyless-signed bundles for a while, anchored
// to the workflow that builds them, and nothing on the consuming side looked at
// the signature -- a grep for cosign across hull returned nothing. A signature
// nobody checks is not a control; it is a claim in a README.
// The message here is cosign's real one, captured by running the real binary
// against a real signed bundle with a foreign identity regexp, and the exit
// code is the real 12. The string this test used to feed -- "no matching
// certificate identity found" -- is not something cosign emits; what it
// actually prints for a wrong identity begins "no matching signatures", which
// the old classifier read as "carries no signature" and allowed to boot.
func TestVerifyBundleFailsClosedOnASignatureThatDoesNotVerify(t *testing.T) {
	stubCosign(t, true,
		"Error: no matching signatures: none of the expected identities matched what was "+
			"in the certificate, got subjects [https://github.com/NOFireAI/hull-assets/"+
			".github/workflows/build-assets.yml@refs/heads/main] with issuer "+
			"https://token.actions.githubusercontent.com\n",
		exitErr(t, 12))
	res := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if res.Outcome != BundleFailed {
		t.Fatalf("outcome = %v, want BundleFailed (%s)", res.Outcome, res.Message())
	}
	// The default mode has to stop here. A check that only prints is the same
	// as no check, and this is the one case where we know something is wrong.
	if err := res.Err(VerifyWarn); err == nil {
		t.Fatal("a bundle whose signature did not verify was allowed under the default mode")
	}
	if err := res.Err(VerifyRequire); err == nil {
		t.Fatal("a bundle whose signature did not verify was allowed under require")
	}
	if err := res.Err(VerifyOff); err != nil {
		t.Fatalf("an operator who turned the check off should not be stopped: %v", err)
	}
	if res.Digest != testDigest {
		t.Errorf("a failure should still name the digest it checked, got %q", res.Digest)
	}
}

// TestVerifyBundleTellsAMissingToolFromAFailedSignature is the distinction that
// decides whether anybody reads the message. If "cosign is not installed" and
// "the signature did not verify" print the same way, people learn the line
// means nothing, and the day it means everything they scroll past it.
func TestVerifyBundleTellsAMissingToolFromAFailedSignature(t *testing.T) {
	stubCosign(t, false, "", nil)
	missing := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if missing.Outcome != BundleNoTooling {
		t.Fatalf("outcome = %v, want BundleNoTooling", missing.Outcome)
	}
	// Nothing was checked, so nothing failed: a host without cosign still boots
	// by default, and says so.
	if err := missing.Err(VerifyWarn); err != nil {
		t.Fatalf("a missing cosign should not stop the default mode: %v", err)
	}
	if err := missing.Err(VerifyRequire); err == nil {
		t.Fatal("require should refuse a host that cannot check anything")
	}

	stubCosign(t, true, "Error: no matching signatures\n", exitErr(t, 12))
	failed := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)

	if missing.Message() == failed.Message() {
		t.Fatal("a missing tool and an unverifiable bundle print the same line")
	}
	if !strings.Contains(missing.Message(), "cosign is not installed") {
		t.Errorf("the missing-tool message should say what to install, got: %s", missing.Message())
	}
	if strings.Contains(missing.Message(), "DID NOT VERIFY") {
		t.Errorf("a missing tool must not read as a failed signature, got: %s", missing.Message())
	}
}

// TestVerifyBundleTreatsAnUnsignedBundleAsUnchecked. cosign exits non-zero both
// for "there is no signature here" (10) and for "this signature is wrong"
// (12), and only the second is evidence of anything. The exit code is what
// separates them; the messages do not, because both begin "no matching".
func TestVerifyBundleTreatsAnUnsignedBundleAsUnchecked(t *testing.T) {
	stubCosign(t, true, "Error: no signatures found\n", exitErr(t, 10))
	res := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if res.Outcome != BundleUnsigned {
		t.Fatalf("outcome = %v, want BundleUnsigned (%s)", res.Outcome, res.Message())
	}
	if err := res.Err(VerifyWarn); err != nil {
		t.Fatalf("an unsigned bundle should be reported, not refused, by default: %v", err)
	}
	if err := res.Err(VerifyRequire); err == nil {
		t.Fatal("require should refuse a bundle carrying no signature")
	}
}

// TestVerifyBundleSkipsARepositoryWeDoNotPublish. There is no signature of ours
// on somebody else's bundle, so there is nothing here to check -- checkRef is
// what decides whether such a bundle is fetched at all.
func TestVerifyBundleSkipsARepositoryWeDoNotPublish(t *testing.T) {
	stubCosign(t, true, "", errors.New("cosign should not have been run"))
	res := testPolicy().VerifyBundle(t.Context(), "ghcr.io/somebody-else/hull-assets:darwin-arm64", testDigest)
	if res.Outcome != BundleNotOurs {
		t.Fatalf("outcome = %v, want BundleNotOurs (%s)", res.Outcome, res.Message())
	}
}

// TestVerifyBundleGivesUpOnACosignThatHangs runs a real subprocess, because the
// deadline has to be enforced where the process is started and a stubbed runner
// would prove nothing about that.
//
// brig's equivalent has no deadline at all: a cosign that never returns holds
// the boot open for as long as it likes, and one wedged for twenty seconds in
// testing is what put a timeout here.
func TestVerifyBundleGivesUpOnACosignThatHangs(t *testing.T) {
	dir := t.TempDir()
	hostile := filepath.Join(dir, "cosign")
	if err := os.WriteFile(hostile, []byte("#!/bin/sh\nsleep 120\n"), 0o755); err != nil {
		t.Fatalf("write fake cosign: %v", err)
	}
	p := DefaultVerifyPolicy()
	p.Cosign = hostile
	p.Timeout = 200 * time.Millisecond

	start := time.Now()
	res := p.VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("a hung cosign held the boot for %s", elapsed)
	}
	if res.Outcome != BundleUnavailable {
		t.Fatalf("outcome = %v, want BundleUnavailable (%s)", res.Outcome, res.Message())
	}
	// A deadline that fired proves nothing about the signature, so it must not
	// masquerade as a failed one -- a check that turns a slow network into a
	// boot failure is a check people switch off for good.
	if err := res.Err(VerifyWarn); err != nil {
		t.Fatalf("a timeout should be reported, not fatal, by default: %v", err)
	}
	if err := res.Err(VerifyRequire); err == nil {
		t.Fatal("require should refuse when nothing could be checked")
	}
	if strings.Contains(res.Message(), "DID NOT VERIFY") {
		t.Errorf("a timeout must not read as a failed signature, got: %s", res.Message())
	}
}

// TestParseVerifyModeFallsBackToTheSafeSetting. The failure mode of a typo here
// is invisible -- everything still boots, unverified -- so anything we do not
// recognise lands on warn rather than off.
func TestParseVerifyModeFallsBackToTheSafeSetting(t *testing.T) {
	for _, s := range []string{"off", "none", "0", " OFF "} {
		if got := ParseVerifyMode(s); got != VerifyOff {
			t.Errorf("ParseVerifyMode(%q) = %q, want off", s, got)
		}
	}
	for _, s := range []string{"require", "strict", "REQUIRE"} {
		if got := ParseVerifyMode(s); got != VerifyRequire {
			t.Errorf("ParseVerifyMode(%q) = %q, want require", s, got)
		}
	}
	for _, s := range []string{"", "warn", "of", "false", "no", "yes", "disabled"} {
		if got := ParseVerifyMode(s); got != VerifyWarn {
			t.Errorf("ParseVerifyMode(%q) = %q, want warn: a malformed value must not "+
				"be the thing that turns the check off", s, got)
		}
	}
}

// TestVerifyModeFromEnvReadsTheSetting keeps the environment variable wired to
// the mode, since that is the only way anybody changes it.
func TestVerifyModeFromEnvReadsTheSetting(t *testing.T) {
	t.Setenv(VerifyModeEnv, "")
	if got := VerifyModeFromEnv(); got != VerifyWarn {
		t.Fatalf("default mode = %q, want warn", got)
	}
	t.Setenv(VerifyModeEnv, "require")
	if got := VerifyModeFromEnv(); got != VerifyRequire {
		t.Fatalf("HULL_VERIFY=require gave %q", got)
	}
	t.Setenv(VerifyModeEnv, "off")
	if got := VerifyModeFromEnv(); got != VerifyOff {
		t.Fatalf("HULL_VERIFY=off gave %q", got)
	}
}
