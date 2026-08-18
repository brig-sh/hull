// Copyright (c) 2023-2026, Nubificus LTD
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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/bootassets"
)

// What an absent provenance record means, in each mode.
//
// The first version of this refused outright for anything inside hull's own
// asset directory, and CI caught it inside the hour: the runner's assets
// predate the record, so every real VM boot stopped. ErrNoProvenance's own
// comment says an absent record is a migration rather than an attack, and that
// decision should not have been overridden without doing the migration.
//
// The finding this is answering was narrower: HULL_VERIFY=require never covered
// a boot at all. So require refuses, and the default warns.
func TestMissingProvenanceIsFatalOnlyUnderRequire(t *testing.T) {
	storeDir := t.TempDir()
	assetDir := filepath.Join(storeDir, "assets")
	if err := os.MkdirAll(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(assetDir, "Image")
	if err := os.WriteFile(kernel, []byte("KERNEL BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A staged copy, as the boot path would make.
	instanceDir := t.TempDir()
	staged, err := stageHostBootFile(kernel, instanceDir, stagedHostKernelName)
	if err != nil {
		t.Fatal(err)
	}
	// No provenance.json is written: this is the migration case.

	t.Run("default warns and boots", func(t *testing.T) {
		t.Setenv(bootassets.VerifyModeEnv, "")
		if err := verifyStagedBootAsset(storeDir, kernel, staged); err != nil {
			t.Errorf("an asset with no record stopped a default run: %v", err)
		}
	})

	t.Run("warn warns and boots", func(t *testing.T) {
		t.Setenv(bootassets.VerifyModeEnv, "warn")
		if err := verifyStagedBootAsset(storeDir, kernel, staged); err != nil {
			t.Errorf("an asset with no record stopped a warn run: %v", err)
		}
	})

	t.Run("require refuses", func(t *testing.T) {
		t.Setenv(bootassets.VerifyModeEnv, "require")
		err := verifyStagedBootAsset(storeDir, kernel, staged)
		if err == nil {
			t.Fatal("HULL_VERIFY=require booted an asset with no provenance record; " +
				"require not covering the boot path is the whole finding")
		}
		if !strings.Contains(err.Error(), bootassets.VerifyModeEnv) {
			t.Errorf("the refusal does not name the setting that caused it: %v", err)
		}
	})

	t.Run("off boots", func(t *testing.T) {
		t.Setenv(bootassets.VerifyModeEnv, "off")
		if err := verifyStagedBootAsset(storeDir, kernel, staged); err != nil {
			t.Errorf("an operator who turned the check off was stopped anyway: %v", err)
		}
	})
}

// A record that DISAGREES with the bytes is a different case from one that is
// absent, and must stop the boot in every mode that checks at all.
func TestATamperedBootAssetIsRefused(t *testing.T) {
	storeDir := t.TempDir()
	assetDir := filepath.Join(storeDir, "assets")
	if err := os.MkdirAll(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(assetDir, "Image")
	if err := os.WriteFile(kernel, []byte("REAL KERNEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write the record by hand rather than adding an exported test hook to the
	// package: production surface for a test is how the last round's seam got
	// into a daemon's code path.
	sum := sha256.Sum256([]byte("REAL KERNEL"))
	rec := fmt.Sprintf(`{"ref":"ghcr.io/x/y:z","digest":"sha256:deadbeef",`+
		`"files":{"Image":{"sha256":%q,"size":%d}}}`,
		hex.EncodeToString(sum[:]), len("REAL KERNEL"))
	if err := os.WriteFile(filepath.Join(assetDir, "provenance.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}

	instanceDir := t.TempDir()
	staged, err := stageHostBootFile(kernel, instanceDir, stagedHostKernelName)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the staged copy, standing in for bytes that do not match.
	if err := os.WriteFile(staged, []byte("ATTACKER KERNEL"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"", "warn", "require"} {
		t.Setenv(bootassets.VerifyModeEnv, mode)
		if err := verifyStagedBootAsset(storeDir, kernel, staged); err == nil {
			t.Errorf("mode %q booted a kernel that disagrees with its record", mode)
		}
	}
}
