package provider

import (
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v2"
)

// sampleCloudInitParams returns representative params for the render tests.
func sampleCloudInitParams() CloudInitParams {
	return CloudInitParams{
		ControlPlaneURL: "wss://ecu.example.com/agent/connect",
		TunnelToken:     "t_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ImageRef:        "ghcr.io/backhand/ecu-image:latest",
		ToolServerPort:  8000,
		Width:           1280,
		Height:          800,
		AgentBinaryURL:  "https://github.com/backhand/ecu/releases/latest/download/ecu-linux-amd64",
	}
}

// TestRenderCloudInitContents asserts the required substrings are present: the
// #cloud-config header (as the first line), the tunnel token, the control-plane
// URL, the image ref, the localhost port binding, WIDTH/HEIGHT, and the full
// agent invocation. These are the load-bearing properties of the cloud-init.
func TestRenderCloudInitContents(t *testing.T) {
	p := sampleCloudInitParams()
	out, err := RenderCloudInit(p)
	if err != nil {
		t.Fatalf("RenderCloudInit: %v", err)
	}

	// (b) first line is exactly #cloud-config.
	firstLine := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		firstLine = out[:i]
	}
	if firstLine != "#cloud-config" {
		t.Fatalf("first line = %q, want %q", firstLine, "#cloud-config")
	}

	// (c) required substrings.
	mustContain := []string{
		p.TunnelToken,
		p.ControlPlaneURL,
		p.ImageRef,
		p.AgentBinaryURL,
		"-p '127.0.0.1:8000:8000'", // localhost binding is the security property
		"WIDTH=1280",
		"HEIGHT=800",
		// Full agent invocation with all flags.
		"--agent --control-plane '" + p.ControlPlaneURL + "' --token '" + p.TunnelToken + "' --tool-server 'http://127.0.0.1:8000'",
		// systemd unit + enable.
		"ecu-agent.service",
		"systemctl enable --now ecu-agent.service",
		// docker install.
		"get.docker.com",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered cloud-init missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderCloudInitLocalhostBindingOnly is an explicit assertion of the
// headline security property: the tool port is published bound to 127.0.0.1
// ONLY, never on 0.0.0.0 / the public IP. The container binds 0.0.0.0 inside
// the box on purpose; the instance must publish it loopback-only.
func TestRenderCloudInitLocalhostBindingOnly(t *testing.T) {
	out, err := RenderCloudInit(sampleCloudInitParams())
	if err != nil {
		t.Fatalf("RenderCloudInit: %v", err)
	}
	if !strings.Contains(out, "127.0.0.1:8000:8000") {
		t.Fatalf("cloud-init does not bind the tool port to 127.0.0.1:\n%s", out)
	}
	// No publish of the tool port on all interfaces.
	if strings.Contains(out, "-p '0.0.0.0:8000") || strings.Contains(out, "-p '8000:8000'") || strings.Contains(out, "-p 8000:8000") {
		t.Fatalf("cloud-init publishes the tool port on a non-loopback address:\n%s", out)
	}
}

// TestRenderCloudInitValidYAML checks the rendered config parses as YAML (a
// nice-to-have beyond the Contains assertions). cloud-init is YAML with a
// leading #cloud-config comment, which YAML treats as a comment line.
func TestRenderCloudInitValidYAML(t *testing.T) {
	out, err := RenderCloudInit(sampleCloudInitParams())
	if err != nil {
		t.Fatalf("RenderCloudInit: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rendered cloud-init is not valid YAML: %v\n---\n%s", err, out)
	}
	// Sanity: the documented top-level keys are present.
	for _, key := range []string{"package_update", "write_files", "runcmd"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("rendered cloud-init missing top-level key %q; parsed keys: %v", key, keysOf(doc))
		}
	}
}

// TestRenderCloudInitDefaults verifies zero values get sane defaults (port
// 8000, 1280x800, the default image ref).
func TestRenderCloudInitDefaults(t *testing.T) {
	out, err := RenderCloudInit(CloudInitParams{
		ControlPlaneURL: "ws://127.0.0.1:8080/agent/connect",
		TunnelToken:     "t_abc",
		AgentBinaryURL:  "https://example.com/ecu",
		// ImageRef, ToolServerPort, Width, Height all zero -> defaults.
	})
	if err != nil {
		t.Fatalf("RenderCloudInit: %v", err)
	}
	for _, want := range []string{
		"127.0.0.1:8000:8000",
		"WIDTH=1280",
		"HEIGHT=800",
		defaultImageRef,
		"--tool-server 'http://127.0.0.1:8000'",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("defaulted cloud-init missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderCloudInitRequiresFields verifies the required-field guards.
func TestRenderCloudInitRequiresFields(t *testing.T) {
	cases := []struct {
		name string
		p    CloudInitParams
	}{
		{"no control-plane URL", CloudInitParams{TunnelToken: "t", AgentBinaryURL: "u"}},
		{"no token", CloudInitParams{ControlPlaneURL: "ws://x/agent/connect", AgentBinaryURL: "u"}},
		{"no agent binary URL", CloudInitParams{ControlPlaneURL: "ws://x/agent/connect", TunnelToken: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderCloudInit(tc.p); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestRenderCloudInitSample prints a full rendered sample so
// `go test -run CloudInit -v` shows exactly what an instance receives.
func TestRenderCloudInitSample(t *testing.T) {
	out, err := RenderCloudInit(sampleCloudInitParams())
	if err != nil {
		t.Fatalf("RenderCloudInit: %v", err)
	}
	t.Logf("rendered #cloud-config:\n%s", out)
}

// keysOf returns the keys of a map for error messages.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
