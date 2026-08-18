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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// This file answers the question the provenance record cannot: was the bundle
// genuine when it arrived?
//
// hull-assets signs every published bundle with keyless cosign, so the
// signature is bound to the workflow, in the repository, that built it, and is
// recorded in Sigstore's public transparency log. That has been true and
// verified on all three platform tags for a while -- and until now nothing on
// the consuming side looked at it. A signature nobody checks is decoration,
// and worse than none, because it is quoted as though it were a control.
//
// It shells out to cosign rather than linking sigstore, which is what brig
// does in internal/verify for the same reason: linking it pulls in around two
// hundred modules for one subprocess we run at most once per download, and
// hull's dependency surface is itself part of what a supply-chain check is
// supposed to protect.

// VerifyMode is how strict the check is.
type VerifyMode string

const (
	// VerifyOff skips the check entirely.
	VerifyOff VerifyMode = "off"
	// VerifyWarn refuses a bundle that claims to be ours and fails
	// verification, and reports -- without stopping -- the cases where nothing
	// could be checked: no cosign on the host, an unsigned bundle, a cosign
	// that did not finish. The default.
	VerifyWarn VerifyMode = "warn"
	// VerifyRequire refuses anything that was not positively verified,
	// including a host with no cosign installed.
	VerifyRequire VerifyMode = "require"
)

// VerifyModeEnv names the setting, in brig's style (BRIG_VERIFY).
const VerifyModeEnv = "HULL_VERIFY"

// ParseVerifyMode reads a mode, falling back to VerifyWarn for anything it does
// not recognise.
//
// The fallback is the safe end on purpose. A typo in this variable -- HULL_VERIFY=of,
// HULL_VERIFY=false -- must not be the thing that silently turns the check off,
// because the failure is invisible: everything still boots, and it boots
// unverified.
func ParseVerifyMode(s string) VerifyMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "0":
		return VerifyOff
	case "require", "strict":
		return VerifyRequire
	default:
		return VerifyWarn
	}
}

// VerifyModeFromEnv is the mode this process runs under.
func VerifyModeFromEnv() VerifyMode {
	return ParseVerifyMode(os.Getenv(VerifyModeEnv))
}

// VerifyPolicy is what counts as a bundle of ours, and how to check it.
type VerifyPolicy struct {
	// Repo is the repository we publish under. A reference anywhere else has
	// no signature of ours to check.
	Repo string
	// Identity is the certificate identity regexp: the workflow, in the
	// repository, allowed to have signed a bundle.
	Identity string
	// Issuer is the OIDC issuer that vouched for that identity.
	Issuer string
	// Cosign is the binary to run.
	Cosign string
	// Timeout bounds that binary's run.
	Timeout time.Duration
}

// DefaultVerifyPolicy matches what NOFireAI/hull-assets publishes.
//
// The identity is anchored at both ends and names the workflow file and the
// branch, so a signature from another workflow in the same repository -- or
// from the same workflow run off a branch or a fork -- does not satisfy it.
func DefaultVerifyPolicy() VerifyPolicy {
	return VerifyPolicy{
		Repo:     DefaultRepo,
		Identity: `^https://github\.com/NOFireAI/hull-assets/\.github/workflows/build-assets\.yml@refs/heads/main$`,
		Issuer:   "https://token.actions.githubusercontent.com",
		Cosign:   "cosign",
		// cosign talks to Fulcio and Rekor, so it is a network operation and
		// needs room -- but bounded room. brig's equivalent has no deadline at
		// all, and a cosign that hangs blocks the boot behind it for as long as
		// it likes; one wedged for twenty seconds in testing is what put this
		// number here. A deadline that fires is reported as "could not check",
		// not as "failed": a flaky network must not be a boot failure, and an
		// attacker who can wedge our cosign already runs code as this user.
		Timeout: 30 * time.Second,
	}
}

// VerifyOutcome is what the check concluded. These are kept distinct because
// collapsing them is how a real failure gets read as a missing tool and
// ignored -- "cosign is not installed" and "the signature did not verify" are
// not the same sentence and must never print the same way.
type VerifyOutcome int

const (
	// BundleVerified: ours, signed by the publishing workflow, and it checks out.
	BundleVerified VerifyOutcome = iota
	// BundleNotOurs: published somewhere we do not publish, so there is no
	// signature of ours to check.
	BundleNotOurs
	// BundleNoTooling: cosign is not installed, so nothing could be checked.
	BundleNoTooling
	// BundleUnsigned: it is ours and carries no signature at all. Older
	// bundles predate signing.
	BundleUnsigned
	// BundleUnavailable: cosign was there and did not finish -- a timeout, or
	// a transparency log we could not reach.
	BundleUnavailable
	// BundleFailed: it claims to be ours, a signature is present, and it does
	// not verify. This is the one that stops a boot.
	BundleFailed
)

// VerifyResult carries the outcome and the detail worth printing.
type VerifyResult struct {
	Outcome VerifyOutcome
	// Ref is what was checked, digest-pinned where a digest was known.
	Ref string
	// Digest is the manifest digest the signature was checked against, and is
	// what gets recorded in the provenance so a later cached boot can be tied
	// back to this check.
	Digest string
	// Detail is cosign's own reason, trimmed to the useful line.
	Detail string
}

// Message is the line to show, phrased for the outcome.
func (r VerifyResult) Message() string {
	switch r.Outcome {
	case BundleVerified:
		return fmt.Sprintf("boot assets %s: signature verified", r.Ref)
	case BundleNotOurs:
		return fmt.Sprintf("boot assets %s are not published by hull, so there is no signature "+
			"of ours to check; be sure you trust where they came from", r.Ref)
	case BundleNoTooling:
		return fmt.Sprintf("cannot check the signature on the boot assets %s: cosign is not "+
			"installed (`brew install cosign`). The kernel your sandboxes boot is being taken "+
			"unverified", r.Ref)
	case BundleUnsigned:
		return fmt.Sprintf("boot assets %s carry no signature; hull publishes signed bundles, so "+
			"this is either an older one or not what we published", r.Ref)
	case BundleUnavailable:
		return fmt.Sprintf("could not finish checking the signature on the boot assets %s: %s. "+
			"Nothing failed verification -- nothing was verified", r.Ref, r.Detail)
	default:
		return fmt.Sprintf("boot assets %s claim to be published by hull, but the signature DID "+
			"NOT VERIFY: %s", r.Ref, r.Detail)
	}
}

// Err returns the error this outcome amounts to under a mode, or nil.
//
// The default is warn, and under warn exactly one outcome is fatal: a bundle
// from our repository whose signature is present and wrong. Everything else is
// "could not check", which is a different claim and must not be dressed up as
// this one -- a check that fails the build on a flaky network is a check people
// switch off.
func (r VerifyResult) Err(mode VerifyMode) error {
	switch mode {
	case VerifyOff:
		return nil
	case VerifyRequire:
		if r.Outcome == BundleVerified {
			return nil
		}
		return fmt.Errorf("%s=require and the boot assets were not verified: %s",
			VerifyModeEnv, r.Message())
	default:
		if r.Outcome == BundleFailed {
			return fmt.Errorf("refusing to install boot assets: %s. Nothing on this host will "+
				"tell you again -- work out where %s came from before setting %s=off",
				r.Message(), r.Ref, VerifyModeEnv)
		}
		return nil
	}
}

// VerifyBundle checks one resolved bundle against the policy.
//
// digest is the manifest digest the pull resolved, and when it is known the
// check is made against repo@digest rather than the tag: a tag can move between
// the pull and the verification, and then what was verified is not what was
// written to disk.
func (p VerifyPolicy) VerifyBundle(ctx context.Context, ref, digest string) VerifyResult {
	subject := ref
	if digest != "" {
		if parsed, err := name.ParseReference(ref); err == nil {
			subject = parsed.Context().Name() + "@" + digest
		}
	}
	res := VerifyResult{Ref: subject, Digest: digest}

	if !sameRepo(repoOf(ref), p.Repo) {
		res.Outcome = BundleNotOurs
		return res
	}
	if _, err := verifyLookPath(p.Cosign); err != nil {
		res.Outcome = BundleNoTooling
		return res
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultVerifyPolicy().Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := verifyRun(ctx, p.Cosign,
		"verify",
		"--certificate-identity-regexp", p.Identity,
		"--certificate-oidc-issuer", p.Issuer,
		subject,
	)
	switch {
	case err == nil:
		res.Outcome = BundleVerified
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Outcome = BundleUnavailable
		res.Detail = fmt.Sprintf("cosign did not finish within %s", timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		res.Outcome = BundleUnavailable
		res.Detail = "the run was cancelled before cosign finished"
	default:
		res.Outcome = outcomeForExit(exitCodeOf(err))
		res.Detail = firstVerifyLine(out, err)
	}
	return res
}

// cosign's exit codes. They are stable, documented, and -- unlike the message
// text -- they distinguish the two cases this decision turns on.
const (
	cosignNoSignatures = 10 // nothing is signed here
	cosignTagNotFound  = 11 // the reference does not resolve
	cosignVerifyFailed = 12 // signature material is present and does not check out
)

// outcomeForExit maps a cosign exit code to an outcome.
//
// This used to read cosign's stderr, and the first substring it looked for was
// "no matching signatures" -- which is exactly what cosign prints when the
// signature is real and the IDENTITY IS WRONG. An attacker's bundle, signed by
// their own workflow with a perfectly valid Fulcio certificate, was therefore
// classified "carries no signature", and under the default mode that boots.
// The same went for content substitution: a changed digest has no signature of
// its own, so it landed in the same bucket. Signature stripping, substitution
// and wrong-signer -- the three attacks this file exists to stop -- all reached
// the boot, while the one outcome that refuses was reserved for cases cosign
// does not actually produce. The test that covered it fed a string cosign
// never emits.
//
// An unrecognised non-zero code is "could not check", not "checked and failed".
// cosign exits 1 for its own network, auth and TUF problems, and by the time we
// run it the registry has already answered us once, so this is a narrow class.
// Refusing on it would fail closed on the one condition the mode knob exists to
// let people ride out; it stays Unavailable, and `require` still refuses it.
func outcomeForExit(code int) VerifyOutcome {
	switch code {
	case cosignNoSignatures:
		return BundleUnsigned
	case cosignTagNotFound, cosignVerifyFailed:
		return BundleFailed
	default:
		return BundleUnavailable
	}
}

// exitCodeOf returns the process exit code, or -1 if the error is not one.
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// repoOf is the repository part of a reference, or the reference itself if it
// does not parse -- in which case sameRepo will reject it anyway.
func repoOf(ref string) string {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ref
	}
	return parsed.Context().Name()
}


// verifyLookPath and verifyRun are variables so the decision table can be
// driven in tests on a host that has no cosign, or one that has a working one.
var verifyLookPath = exec.LookPath

var verifyRun = func(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Killing the process is not enough to get control back. Capturing output
	// into a buffer means Wait blocks until the pipe's write end is closed
	// everywhere, and a child that outlived the signal -- or a grandchild that
	// inherited the descriptor -- holds it open indefinitely. Without this the
	// deadline above would fire and the boot would still be stuck, which is
	// the exact failure the deadline exists to prevent.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return out.String(), err
}

func firstVerifyLine(out string, err error) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// cosign prints a banner about the transparency log before the real
		// reason; the reason is the first line that is not part of it.
		if line == "" || strings.HasPrefix(line, "Verification for ") ||
			strings.HasPrefix(line, "The following checks") ||
			strings.HasPrefix(line, "  - ") {
			continue
		}
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "no detail"
}
