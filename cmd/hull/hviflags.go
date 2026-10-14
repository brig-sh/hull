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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// hviBootTakesControlFlags reports whether the hvi at binary accepts
// --control-sock, --profile-out and --checkpoint-root on `boot`.
//
// hvi refuses a boot argument it does not know and exits before the guest
// starts, so passing the three to an hvi from before them (the release hull
// pins, for one) fails every run, not just checkpoints. Asked of the binary
// that will run, once per run: hull is shipped beside one hvi but a store can
// outlive it, and PATH or the urunc config can name another.
//
// The probe is `boot --checkpoint-root` with no value. --checkpoint-root is
// the newest of the three, so an hvi that knows it knows the others; it fails
// while parsing the arguments, before any hypervisor, sandbox or file is
// touched. An hvi without it answers `unknown boot arg "--checkpoint-root"`
// (src/main.rs), one with it that the flag needs a directory. Any other
// answer is taken as "supported": the boot itself then fails with hvi's own
// reason, which says more than a guess here would.
func hviBootTakesControlFlags(binary string) (bool, error) {
	return hviBootKnowsFlag(binary, "--checkpoint-root")
}

// hviBootTakesProfile reports whether the hvi at binary accepts --profile
// on `boot`. Asked the same way: `boot --profile` with no name fails while
// parsing, and an hvi from before machine profiles answers `unknown boot arg
// "--profile"` (its --profile-out is a different argument, matched whole).
//
// Unlike the control flags, a run that asked for a profile does not go on
// without it: an hvi that cannot take --profile would boot the native
// machine, whose checkpoints restore only on the same kind of host.
func hviBootTakesProfile(binary string) (bool, error) {
	return hviBootKnowsFlag(binary, "--profile")
}

// hviBootKnowsFlag runs `binary boot FLAG` with no value and reports
// whether hvi named the flag unknown.
func hviBootKnowsFlag(binary, flag string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	probe := exec.CommandContext(ctx, binary, "boot", flag)
	probe.Stdout = &out
	probe.Stderr = &out
	err := probe.Run()
	if ctx.Err() != nil {
		return false, fmt.Errorf("%s boot did not answer a probe within 5s", binary)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return false, fmt.Errorf("run %s: %w", binary, err)
	}
	return !strings.Contains(out.String(), fmt.Sprintf("unknown boot arg %q", flag)), nil
}
