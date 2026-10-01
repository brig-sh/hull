//go:build darwin

// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// The absence sentence is the only one callers match. An unreadable record
// has to keep its own error, wrapped with the name, and still be the
// store's unreadable sentinel underneath.
func TestInstanceReadErrorReservesNotFoundForAbsence(t *testing.T) {
	missing := instanceReadError("gone", store.ErrInstanceNotFound)
	if missing.Error() != "instance not found: gone" {
		t.Fatalf("absence: %v", missing)
	}
	if errors.Is(missing, store.ErrInstanceStateUnreadable) {
		t.Fatalf("absence reported as unreadable: %v", missing)
	}

	broken := instanceReadError("kept", fmt.Errorf("%w: state file is missing", store.ErrInstanceStateUnreadable))
	if strings.Contains(broken.Error(), "instance not found") || !errors.Is(broken, store.ErrInstanceStateUnreadable) {
		t.Fatalf("unreadable record: %v", broken)
	}
	if !strings.Contains(broken.Error(), "kept") {
		t.Fatalf("unreadable error dropped the name: %v", broken)
	}
}
