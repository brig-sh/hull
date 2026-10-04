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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/telemetry"
)

// A wrapper that lists metrics in its suppress list sends them itself, and
// then nothing here samples the VMM.
func TestSamplerStaysOffWhenMetricsAreSuppressed(t *testing.T) {
	t.Setenv(telemetry.EnvDisabled, "")
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv(telemetry.EnvSuppress, "metrics")
	dir := t.TempDir()
	if err := telemetry.SetConsent(dir, true); err != nil {
		t.Fatal(err)
	}
	prev := telemetryClient
	t.Cleanup(func() { telemetryClient = prev })
	telemetryClient = telemetry.Init(telemetry.Config{StoreDir: dir, Version: "0.0.0-test"})
	if !telemetryClient.Enabled() {
		t.Fatal("test telemetry client is disabled; it would assert nothing")
	}

	instanceDir := t.TempDir()
	done := make(chan struct{})
	defer close(done)
	startVMMMetricsSampler(os.Getpid(), "qemu", 1, time.Now(), instanceDir, done)

	if _, err := os.Stat(filepath.Join(instanceDir, ".metrics.lock")); !os.IsNotExist(err) {
		t.Fatal("a sampler started for metrics the wrapper suppressed")
	}
}
