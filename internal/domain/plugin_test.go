package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsKnownCapability(t *testing.T) {
	if !IsKnownCapability(CapabilityStreamResolve) {
		t.Fatal("stream.resolve should be known")
	}
	if IsKnownCapability(Capability("git")) {
		t.Fatal("an unknown capability must not become known")
	}
}

func TestRPCErrorMapsSentinels(t *testing.T) {
	cases := []struct {
		code  int
		want  error
		known bool
	}{
		{RPCErrorCrashed, ErrPluginCrashed, true},
		{RPCErrorHandshakeTimeout, ErrPluginHandshakeTimeout, true},
		{RPCErrorIncompatibleVersion, ErrPluginIncompatibleAPIVersion, true},
		{RPCErrorMethodNotFound, ErrPluginMethodNotFound, true},
		{4200, nil, false},
	}
	for _, tc := range cases {
		err := &RPCError{Code: tc.code, Message: "test"}
		if tc.known && !errors.Is(err, tc.want) {
			t.Errorf("code %d should match %v", tc.code, tc.want)
		}
		if !tc.known && errors.Is(err, ErrPluginCrashed) {
			t.Errorf("code %d should not match a host sentinel", tc.code)
		}
		if got := RPCErrorCode(err); got != tc.code {
			t.Errorf("RPCErrorCode = %d, want %d", got, tc.code)
		}
	}
}

func TestRPCErrorCodeMapsHostSentinels(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrPluginCrashed, RPCErrorCrashed},
		{fmt.Errorf("call: %w", ErrPluginCrashed), RPCErrorCrashed},
		{ErrPluginHandshakeTimeout, RPCErrorHandshakeTimeout},
		{ErrPluginIncompatibleAPIVersion, RPCErrorIncompatibleVersion},
		{ErrPluginMethodNotFound, RPCErrorMethodNotFound},
		{ErrPluginHostClosed, 0},
	}
	for _, tc := range cases {
		if got := RPCErrorCode(tc.err); got != tc.want {
			t.Errorf("RPCErrorCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestRPCErrorWithoutMessage(t *testing.T) {
	err := &RPCError{Code: RPCErrorCrashed}
	if err.Error() != "plugin error -32000" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

// TestHasCapability pins the helper the provider check uses: providers are only
// meaningful next to stream.resolve.
func TestHasCapability(t *testing.T) {
	caps := []Capability{CapabilityAuth, CapabilityStreamResolve}
	if !HasCapability(caps, CapabilityStreamResolve) {
		t.Fatal("stream.resolve should be found")
	}
	if HasCapability(caps, CapabilityLyricFetch) {
		t.Fatal("a capability the plugin does not declare must not match")
	}
	if HasCapability(nil, CapabilityStreamResolve) {
		t.Fatal("no capabilities means nothing is declared")
	}
}

// TestFailureKindsExcludeDecline keeps the routing concept out of the wire
// contract: a plugin no longer declares "not my provider", because the app
// knows the owner from the manifest and never asks a plugin to decline.
func TestFailureKindsExcludeDecline(t *testing.T) {
	for _, kind := range []string{
		StreamErrorRateLimited, StreamErrorUnavailable, StreamErrorVIP, StreamErrorAuth,
	} {
		if kind == "unknown_source" {
			t.Fatalf("%q is a routing concept, not a failure kind", kind)
		}
	}
}

// TestPluginStatusValues pins the strings the frontend switches on.
func TestPluginStatusValues(t *testing.T) {
	if PluginStatusIdle != "idle" || PluginStatusRunning != "running" || PluginStatusFailed != "failed" {
		t.Fatalf("status values changed: %q %q %q", PluginStatusIdle, PluginStatusRunning, PluginStatusFailed)
	}
}
