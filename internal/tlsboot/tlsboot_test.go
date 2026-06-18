package tlsboot

import (
	"context"
	"errors"
	"testing"
)

// TestTLSDashEncodeIP covers the IPv4 -> nip.io dash encoding and the
// rejection of IPv6 / garbage inputs. Dash encoding is the heart of the nip.io
// fallback, so both the happy path and every reject path are pinned.
func TestTLSDashEncodeIP(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"203.0.113.7", "203-0-113-7.nip.io"},
		{"127.0.0.1", "127-0-0-1.nip.io"},
		{"10.20.30.40", "10-20-30-40.nip.io"},
		{"255.255.255.255", "255-255-255-255.nip.io"},
		{"  192.168.1.1  ", "192-168-1-1.nip.io"}, // surrounding whitespace tolerated
	}
	for _, c := range valid {
		got, err := DashEncodeIP(c.in)
		if err != nil {
			t.Errorf("DashEncodeIP(%q) error = %v, want nil", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("DashEncodeIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	invalid := []string{
		"2001:db8::1", // IPv6 — dash encoding is IPv4-only
		"::1",         // IPv6 loopback
		"not-an-ip",   // garbage
		"",            // empty
		"203.0.113",   // too few octets
		"999.0.0.1",   // out-of-range octet (not a valid IP)
	}
	for _, in := range invalid {
		if got, err := DashEncodeIP(in); err == nil {
			t.Errorf("DashEncodeIP(%q) = %q, want error", in, got)
		}
	}
}

// TestTLSResolveHostnameExplicitWins verifies that a non-empty ECU_HOSTNAME is
// returned verbatim (trimmed) and that the public-IP detector is NOT called in
// that case — asserted via a detector that fails AND records invocation.
func TestTLSResolveHostnameExplicitWins(t *testing.T) {
	called := false
	detect := func(context.Context) (string, error) {
		called = true
		return "", errors.New("detector should not be called when ECU_HOSTNAME is set")
	}

	got, err := ResolveHostname(context.Background(), "  ecu.example.com  ", detect)
	if err != nil {
		t.Fatalf("ResolveHostname returned error: %v", err)
	}
	if got != "ecu.example.com" {
		t.Fatalf("ResolveHostname = %q, want trimmed explicit %q", got, "ecu.example.com")
	}
	if called {
		t.Fatalf("detector was called even though an explicit hostname was supplied")
	}
}

// TestTLSResolveHostnameNipIoFallback verifies the empty-hostname path: the
// injected detector's IPv4 is dash-encoded into a nip.io host.
func TestTLSResolveHostnameNipIoFallback(t *testing.T) {
	detect := func(context.Context) (string, error) { return "203.0.113.7", nil }

	got, err := ResolveHostname(context.Background(), "", detect)
	if err != nil {
		t.Fatalf("ResolveHostname returned error: %v", err)
	}
	if want := "203-0-113-7.nip.io"; got != want {
		t.Fatalf("ResolveHostname = %q, want nip.io fallback %q", got, want)
	}
}

// TestTLSResolveHostnameDetectError verifies that a detector error is surfaced
// (wrapped) rather than swallowed when no explicit hostname is set.
func TestTLSResolveHostnameDetectError(t *testing.T) {
	sentinel := errors.New("no network")
	detect := func(context.Context) (string, error) { return "", sentinel }

	_, err := ResolveHostname(context.Background(), "", detect)
	if err == nil {
		t.Fatalf("ResolveHostname returned nil error, want the detector error surfaced")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("ResolveHostname error = %v, want it to wrap the detector error %v", err, sentinel)
	}
}

// TestTLSResolveHostnameDetectInvalidIP verifies that a detector returning a
// non-IPv4 value yields an error (the IPv6/garbage case routed through
// detection rather than DashEncodeIP directly).
func TestTLSResolveHostnameDetectInvalidIP(t *testing.T) {
	detect := func(context.Context) (string, error) { return "2001:db8::1", nil }

	if got, err := ResolveHostname(context.Background(), "", detect); err == nil {
		t.Fatalf("ResolveHostname = %q, want error for non-IPv4 detected address", got)
	}
}

// TestTLSResolveHostnameNoDetector verifies that an empty hostname with a nil
// detector errors rather than panicking.
func TestTLSResolveHostnameNoDetector(t *testing.T) {
	if got, err := ResolveHostname(context.Background(), "", nil); err == nil {
		t.Fatalf("ResolveHostname = %q, want error when no hostname and no detector", got)
	}
}

// TestTLSEnabled pins the mode gate: only "auto" (case-insensitive, trimmed)
// enables the autocert path; "off" and the empty/default value do not.
func TestTLSEnabled(t *testing.T) {
	enabled := []string{"auto", "AUTO", " auto ", "Auto", "\tauto\n"}
	for _, m := range enabled {
		if !Enabled(m) {
			t.Errorf("Enabled(%q) = false, want true", m)
		}
	}
	disabled := []string{"off", "", "OFF", "on", "true", "1", "automatic", "  "}
	for _, m := range disabled {
		if Enabled(m) {
			t.Errorf("Enabled(%q) = true, want false", m)
		}
	}
}

// TestTLSServeRejectsNonAutoMode verifies Serve defensively rejects a mode that
// is not "auto" (callers gate on Enabled, but Serve re-asserts). No network is
// touched because the precondition fails first.
func TestTLSServeRejectsNonAutoMode(t *testing.T) {
	err := Serve(context.Background(), nil, Options{Mode: "off"})
	if err == nil {
		t.Fatalf("Serve with mode=off returned nil error, want a precondition error")
	}
}
