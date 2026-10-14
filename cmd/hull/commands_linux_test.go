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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// A fresh store is a 0700 directory: its mode is the only thing keeping other
// accounts out of the guest's environment and the block image.
func TestEnsureStoreCreatesAPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "store")
	if err := ensureStore(dir, ""); err != nil {
		t.Fatalf("ensureStore on a case-sensitive filesystem: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("store is %v, want a 0700 directory", fi.Mode())
	}
}

// A store on a case-folding filesystem would unpack colliding names over each
// other, so it is refused by name; a probe that cannot answer is an error, not
// a pass.
func TestEnsureStoreRefusesACaseInsensitiveDirectory(t *testing.T) {
	dir := t.TempDir()
	err := ensurePlainStore(dir, func(string) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Errorf("a case-insensitive store was not refused: %v", err)
	}
	probeErr := errors.New("probe failed")
	if err := ensurePlainStore(dir, func(string) (bool, error) { return false, probeErr }); !errors.Is(err, probeErr) {
		t.Errorf("a failed probe was not reported: %v", err)
	}
}

// compose is registered on Linux only to refuse with the reason, rather than
// answering "No help topic", and it stays out of the help listing.
func TestLinuxComposeRefusesWithAReason(t *testing.T) {
	c := linuxCompose()
	if !c.Hidden {
		t.Error("compose is listed in the Linux help")
	}
	if c.Before == nil {
		t.Fatal("compose has no refusing Before hook")
	}
	if _, err := c.Before(context.Background(), c); !errors.Is(err, errComposeMacOnly) {
		t.Errorf("compose Before = %v, want errComposeMacOnly", err)
	}
	found := false
	for _, pc := range platformCommands() {
		if pc.Name == "compose" {
			found = pc.Before != nil
		}
	}
	if !found {
		t.Error("the Linux command list does not carry the refusing compose")
	}
}

// A run Linux cannot deliver is refused before the store is opened, so before
// anything is pulled: the image reference here does not resolve, and the
// store directory must not even be created.
func TestLinuxRunRefusesBeforeTheStoreIsOpened(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--hypervisor", "qemu", "--net", "none"}, "--hypervisor hvi"},
		{[]string{"--rootfs-type", "virtiofs", "--net", "none"}, "--rootfs-type block"},
		{[]string{"--net", "shared"}, "--gateway-sock"},
	} {
		storeDir := filepath.Join(t.TempDir(), "store")
		app := &cli.Command{
			Name:     "hull",
			Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir", Value: storeDir}},
			Commands: []*cli.Command{runCommand()},
		}
		argv := append([]string{"hull", "run"}, tc.args...)
		argv = append(argv, "registry.invalid/never-pulled:1")
		err := app.Run(context.Background(), argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want a refusal naming %s", tc.args, err, tc.want)
		}
		if _, statErr := os.Stat(storeDir); !os.IsNotExist(statErr) {
			t.Errorf("%v: the store was touched before the refusal: %v", tc.args, statErr)
		}
	}
}
