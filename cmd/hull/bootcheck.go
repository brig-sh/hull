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
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/pkg/ociclient"
)

// checkBootPair refuses a container boot given only one of its kernel and
// initrd.
func checkBootPair(bootKernel, bootInitrd string) error {
	if (bootKernel == "") != (bootInitrd == "") {
		return fmt.Errorf("%s and %s must be set together", ociclient.BootKernelAnnotation, ociclient.BootInitrdAnnotation)
	}
	return nil
}

func verifyContainerBootAssets(bootKernel, bootInitrd string) error {
	paths := make([]string, 0, 2)
	for _, p := range []string{bootKernel, bootInitrd} {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve boot asset %s: %w", p, err)
		}
		paths = append(paths, abs)
	}
	if len(paths) == 0 {
		return nil
	}
	err := bootassets.VerifyFiles(paths...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bootassets.ErrNoProvenance):
		log.Warnf("The kernel and initrd for this run (%s) have no provenance record beside "+
			"them, so nothing vouches for their contents; they are being booted as given",
			strings.Join(paths, ", "))
		return nil
	default:
		return fmt.Errorf("refusing to boot this kernel: %w", err)
	}
}
