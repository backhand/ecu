package controlplane

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// readZipEntries reads a zip archive's bytes into a name -> content map, failing
// the test on any read error. It is the shared helper the skill-zip tests use to
// assert which files are present and what the baked ones contain.
func readZipEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	out := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %q: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

// TestBuildSkillZip checks the archive contents and the baked-in substitutions:
// the runtime skill files are present, the test file is filtered out, the
// client's two sentinels carry the supplied URL/key, and SKILL.md's invisible
// sentinel has been replaced with a note mentioning the host.
func TestBuildSkillZip(t *testing.T) {
	const baseURL = "https://cp.example.com"
	const apiKey = "ecu_testkey123"

	data, err := buildSkillZip(baseURL, apiKey)
	if err != nil {
		t.Fatalf("buildSkillZip: %v", err)
	}
	entries := readZipEntries(t, data)

	// The runtime skill files must be present under ecu-computer-use/.
	for _, want := range []string{
		"ecu-computer-use/SKILL.md",
		"ecu-computer-use/ecu_client.py",
		"ecu-computer-use/mcp_server.py",
		"ecu-computer-use/ecu_cli.py",
	} {
		if _, ok := entries[want]; !ok {
			t.Errorf("zip is missing expected entry %q", want)
		}
	}
	// The test file must be filtered out (downloaded skill is for running).
	if _, ok := entries["ecu-computer-use/test_ecu_client.py"]; ok {
		t.Errorf("zip should not contain test_ecu_client.py")
	}

	// ecu_client.py: the sentinels carry the baked values, and the empty form is
	// gone (proving the substitution actually happened, not just appended).
	client := string(entries["ecu-computer-use/ecu_client.py"])
	if !strings.Contains(client, `_BAKED_URL = "https://cp.example.com"`) {
		t.Errorf("ecu_client.py missing baked _BAKED_URL; got:\n%s", excerptBaked(client))
	}
	if !strings.Contains(client, `_BAKED_API_KEY = "ecu_testkey123"`) {
		t.Errorf("ecu_client.py missing baked _BAKED_API_KEY; got:\n%s", excerptBaked(client))
	}
	if strings.Contains(client, `_BAKED_URL = ""`) {
		t.Errorf("ecu_client.py still contains the empty _BAKED_URL sentinel")
	}

	// SKILL.md: the invisible sentinel is replaced with a note mentioning the host.
	skillMD := string(entries["ecu-computer-use/SKILL.md"])
	if strings.Contains(skillMD, "<!-- ECU_PRECONFIGURED -->") {
		t.Errorf("SKILL.md still contains the ECU_PRECONFIGURED sentinel")
	}
	if !strings.Contains(skillMD, "cp.example.com") {
		t.Errorf("SKILL.md note does not mention the control-plane host")
	}
}

// excerptBaked returns just the _BAKED_* lines of a client source, for readable
// failure messages without dumping the whole 800-line file.
func excerptBaked(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "_BAKED_") {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestSkillIndexPublic verifies the landing page is served at the root with NO
// Authorization (it is auth-exempt) as HTML containing the download control.
func TestSkillIndexPublic(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / Content-Type = %q, want text/html...", ct)
	}
	if !strings.Contains(rec.Body.String(), "Download") {
		t.Fatalf("landing page body does not contain \"Download\"")
	}
}

// TestSkillDownloadAuth verifies /skill.zip is gated by the API key: no
// Authorization is 401, and a valid key yields a zip whose baked API key is the
// authenticated key itself (proving the key the middleware validated is what
// gets baked in).
func TestSkillDownloadAuth(t *testing.T) {
	srv := NewServer(newTestStore(t), "")

	// No Authorization -> 401 (the download stays behind the middleware).
	rec := doRequest(t, srv, http.MethodGet, "/skill.zip", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /skill.zip without auth: status = %d, want 401", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())

	// Valid key -> 200 zip; the seeded active key is "k_active".
	rec2 := doRequest(t, srv, http.MethodGet, "/skill.zip", "Bearer k_active")
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /skill.zip with key: status = %d, want 200", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("GET /skill.zip Content-Type = %q, want application/zip", ct)
	}

	// The baked API key in the downloaded client must equal the key we used.
	entries := readZipEntries(t, rec2.Body.Bytes())
	client := string(entries["ecu-computer-use/ecu_client.py"])
	if !strings.Contains(client, `_BAKED_API_KEY = "k_active"`) {
		t.Fatalf("downloaded skill did not bake the authenticated key; got:\n%s", excerptBaked(client))
	}
}
