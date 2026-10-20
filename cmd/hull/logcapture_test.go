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
	"testing"

	"github.com/sirupsen/logrus"
)

// captureWarnings collects everything logged while fn runs.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := logrus.StandardLogger().Out
	prevLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.DebugLevel)
	t.Cleanup(func() {
		logrus.SetOutput(prev)
		logrus.SetLevel(prevLevel)
	})
	fn()
	return buf.String()
}
