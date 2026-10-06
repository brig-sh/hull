// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"fmt"
	"strings"
)

// Labels describe the caller's configuration, not the image's. Keep them out
// of OCI annotations, which merge image-supplied values and affect guest boot.
func parseLabelEntries(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --label %q, expected KEY=VALUE", entry)
		}
		labels[key] = value
	}
	return labels, nil
}
