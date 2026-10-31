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

//go:build darwin

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// eventsDirName is the instance subdirectory that holds hvi's event ledgers.
const eventsDirName = "events"

// createEventsLedger creates the empty ledger file for one hvi boot of the
// instance at instanceDir, and returns its path.
//
// Each boot gets a file of its own, named by its start time in nanoseconds, so
// a restart never writes into the previous boot's ledger. The file is created
// exclusively with mode 0600, because hvi would create it under the process
// umask, and the ledger records the guest's disk offsets, packet addresses and
// the names it resolves. The directory is 0700 and must not be a symbolic link.
//
// # Errors
//
// Errors if the directory cannot be made or is not a plain directory, or if the
// file cannot be created, including when one by that name already exists.
func createEventsLedger(instanceDir string, boot time.Time) (string, error) {
	dir := filepath.Join(instanceDir, eventsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("could not create the events directory %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("could not stat the events directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("the events directory %s is not a directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not restrict the events directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, strconv.FormatInt(boot.UnixNano(), 10)+".ndjson")
	// O_EXCL also refuses a symbolic link planted at the path.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("could not create the events ledger %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("could not create the events ledger %s: %w", path, err)
	}
	return path, nil
}
