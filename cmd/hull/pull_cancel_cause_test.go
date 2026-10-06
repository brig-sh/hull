// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/brig-sh/hull/pkg/ociclient"
)

func TestResolveImageDigestPreservesCancellationCause(t *testing.T) {
	s, _, dockerConfig := runCancellationStore(t)
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	cause := errors.New("distinct pull cancellation cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	_, err := resolveImageDigest(ctx, ociclient.New(s), s,
		"127.0.0.1/testimage:latest", pullAlways, ociclient.DefaultPlatform)
	if !errors.Is(err, cause) {
		t.Fatalf("pull lost the cancellation cause: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pull did not report context cancellation: %v", err)
	}
}
