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

import "testing"

// The outcome is decided by cosign's exit code, not by its prose.
//
// Every message below was produced by running the real cosign against the real
// registry, and every code alongside it is the code that run exited with:
//
//	good identity   exit=0
//	WRONG identity  exit=12  Error: no matching signatures: none of the expected...
//	no such tag     exit=11  Error: image tag not found: GET https://ghcr.io/v2/...
//	unsigned image  exit=10  Error: no signatures found
//	no such repo    exit=1   Error: GET https://ghcr.io/token?scope=...
//
// The first two lines are the whole point. A wrong identity and a missing
// signature both begin "no matching", so the message cannot separate them --
// and the classifier used to try, matching "no matching signatures" first and
// calling the wrong-identity case unsigned. Unsigned boots under the default
// mode, so a bundle signed by anybody at all was accepted.
func TestOutcomeIsDecidedByExitCodeNotMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		out   string
		want  VerifyOutcome
		boots bool // under the default (warn) mode
	}{
		{
			name: "wrong identity, real signature",
			code: 12,
			out: "Error: no matching signatures: none of the expected identities matched " +
				"what was in the certificate, got subjects [https://github.com/attacker/evil/" +
				".github/workflows/build.yml@refs/heads/main]\n",
			want:  BundleFailed,
			boots: false,
		},
		{
			name:  "no signature at all",
			code:  10,
			out:   "Error: no signatures found\n",
			want:  BundleUnsigned,
			boots: true,
		},
		{
			name:  "reference does not resolve",
			code:  11,
			out:   "Error: image tag not found: GET https://ghcr.io/v2/nofireai/hull-assets/...\n",
			want:  BundleFailed,
			boots: false,
		},
		{
			name:  "cosign could not do its job",
			code:  1,
			out:   "Error: GET https://ghcr.io/token?scope=repository%3A...: DENIED\n",
			want:  BundleUnavailable,
			boots: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCosign(t, true, tc.out, exitErr(t, tc.code))
			res := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
			if res.Outcome != tc.want {
				t.Errorf("exit %d classified %v, want %v (%s)", tc.code, res.Outcome, tc.want, res.Message())
			}
			if got := res.Err(VerifyWarn) == nil; got != tc.boots {
				t.Errorf("exit %d boots-under-default = %v, want %v", tc.code, got, tc.boots)
			}
			// require never accepts anything but a positive verification.
			if tc.want != BundleVerified {
				if err := res.Err(VerifyRequire); err == nil {
					t.Errorf("exit %d was allowed under require", tc.code)
				}
			}
		})
	}
}

// A signature that is present and wrong must stop the boot in the mode people
// actually run, which is the one they never set.
func TestAWrongSignerIsRefusedByDefault(t *testing.T) {
	stubCosign(t, true,
		"Error: no matching signatures: none of the expected identities matched what was "+
			"in the certificate, got subjects [https://github.com/attacker/evil/.github/"+
			"workflows/build.yml@refs/heads/main] with issuer "+
			"https://token.actions.githubusercontent.com\n",
		exitErr(t, 12))

	res := testPolicy().VerifyBundle(t.Context(), DefaultRepo+":darwin-arm64", testDigest)
	if err := res.Err(VerifyWarn); err == nil {
		t.Fatalf("a bundle signed by someone else booted under the default mode: %s", res.Message())
	}
}
