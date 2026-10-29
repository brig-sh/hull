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
	"bufio"
	"fmt"
	"io"
	"os"
	"time"
)

func readLogs(out io.Writer, logFile string, follow bool, tail int) error {
	file, err := os.Open(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s", logFile)
		}
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if !follow {
		// Non-follow mode: read all or tail lines
		if tail < 0 {
			// Read all
			_, err := io.Copy(out, file)
			return err
		}

		// Read last N lines
		lines, err := readLastLines(file, tail)
		if err != nil {
			return err
		}
		for _, line := range lines {
			_, _ = fmt.Fprintln(out, line)
		}
		return nil
	}

	// Follow mode: stream log updates
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		_, _ = fmt.Fprintln(out, scanner.Text())
	}

	// Continue scanning for new data
	for {
		// Seek to end and wait for new data
		stat, err := file.Stat()
		if err != nil {
			return fmt.Errorf("failed to stat log file: %w", err)
		}

		// Create a new scanner to read new lines
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			_, _ = fmt.Fprintln(out, scanner.Text())
		}

		// Check if file still exists
		if _, err := os.Stat(logFile); err != nil {
			// File deleted or instance removed
			return nil
		}

		// Check if there's new content
		newStat, _ := os.Stat(logFile)
		if newStat.Size() > stat.Size() {
			// File grew, continue scanning
			continue
		}

		// Wait a bit before checking again
		time.Sleep(100 * time.Millisecond)
	}
}

func readLastLines(file *os.File, n int) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Return last n lines
	if len(lines) <= n {
		return lines, nil
	}

	return lines[len(lines)-n:], nil
}
