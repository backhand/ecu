package controlplane

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/backhand/ecu/skill"
)

// skillIndexHTML is the browser-facing landing page served at the root. It is a
// fully self-contained document (no external assets) that collects an API key
// and downloads the preconfigured skill zip; see skill_index.html and
// handleSkillIndex.
//
//go:embed skill_index.html
var skillIndexHTML []byte

// skillRoot is the directory inside skill.Files that holds the canonical skill
// tree. Every zip entry is written under this path so the downloaded archive
// unpacks to an ecu-computer-use/ folder, exactly like the repo copy.
const skillRoot = "ecu-computer-use"

// buildSkillZip renders the canonical ecu-computer-use skill into a .zip with
// baseURL and apiKey baked in, ready to download. It walks the embedded skill
// tree, drops the test file(s), substitutes the two preconfiguration sentinels
// into ecu_client.py and SKILL.md, and writes everything verbatim otherwise.
//
// The bake is a pair of plain string substitutions on known sentinel lines (see
// the substitution helpers below), so the served copy differs from the repo
// copy only in those lines: ecu_client.py's empty _BAKED_URL/_BAKED_API_KEY
// gain the real values (its fallback chain then uses them when no constructor
// arg or env var is set), and SKILL.md's invisible sentinel comment becomes a
// short "already configured" note. Entry order and timestamps are fixed so the
// same inputs always produce byte-identical archives.
func buildSkillZip(baseURL, apiKey string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err := fs.WalkDir(skill.Files, skillRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Directories carry no bytes; the entries' path prefixes recreate the
		// tree on extraction, so we never write explicit directory entries.
		if d.IsDir() {
			return nil
		}
		// Drop test files (e.g. test_ecu_client.py): the downloaded skill is for
		// running, not for developing, and the test pulls in test-only deps.
		if strings.HasPrefix(d.Name(), "test_") {
			return nil
		}

		data, err := fs.ReadFile(skill.Files, p)
		if err != nil {
			return err
		}
		// Apply the preconfiguration substitution to the two files that carry a
		// sentinel; everything else is embedded verbatim.
		switch {
		case strings.HasSuffix(p, "/ecu_client.py"):
			data = substituteClient(data, baseURL, apiKey)
		case strings.HasSuffix(p, "/SKILL.md"):
			data = substituteSkillMD(data, baseURL)
		}

		// Build a deterministic header (fixed method + timestamp) so identical
		// inputs yield byte-identical archives.
		hdr := &zip.FileHeader{
			Name:     p,
			Method:   zip.Deflate,
			Modified: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = entry.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// substituteClient bakes baseURL and apiKey into ecu_client.py by replacing its
// two empty sentinel assignments. Each is replaced once; the canonical lines are
// exactly `_BAKED_URL = ""` and `_BAKED_API_KEY = ""`, and the values are
// Python-string-escaped so any quote/backslash in a key can't break the source.
func substituteClient(data []byte, baseURL, apiKey string) []byte {
	out := bytes.Replace(
		data,
		[]byte(`_BAKED_URL = ""`),
		[]byte(`_BAKED_URL = "`+pyEsc(baseURL)+`"`),
		1,
	)
	out = bytes.Replace(
		out,
		[]byte(`_BAKED_API_KEY = ""`),
		[]byte(`_BAKED_API_KEY = "`+pyEsc(apiKey)+`"`),
		1,
	)
	return out
}

// substituteSkillMD replaces SKILL.md's invisible preconfiguration sentinel with
// a one-line note telling the reader the skill is already wired to baseURL, so
// the manual ECU_URL / ECU_API_KEY setup that follows can be skipped. In the
// repo copy the sentinel renders as nothing; only the downloaded copy shows the
// note.
func substituteSkillMD(data []byte, baseURL string) []byte {
	note := "> **Preconfigured copy.** This skill is already wired to " +
		"`" + baseURL + "`" +
		" with your API key baked into " + "`ecu_client.py`" +
		", so you can skip the ECU_URL / ECU_API_KEY setup below — it works " +
		"as-is. (Setting those env vars still overrides the baked values.)"
	return bytes.Replace(data, []byte("<!-- ECU_PRECONFIGURED -->"), []byte(note), 1)
}

// pyEsc escapes a string for embedding inside a Python double-quoted literal.
// Backslashes are escaped first (so the backslashes we add for quotes are not
// themselves doubled), then double quotes. This keeps a baked URL or key with
// odd characters from terminating the literal or injecting into the source.
func pyEsc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// handleSkillIndex serves the public landing page at the root. It carries no
// secrets — it only collects an API key client-side and fetches /skill.zip with
// it — so authMiddleware exempts it (see auth.go); the download itself stays
// behind the key check.
func (s *Server) handleSkillIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(skillIndexHTML)
}

// handleSkillDownload builds and returns the preconfigured skill zip for the
// authenticated key. The API-key middleware already validated the caller, so the
// Authorization header is guaranteed present and valid here; we re-extract the
// key (it is the value to bake in) and keep a defensive 401 in case this handler
// is ever reached off the middleware path. The baked base URL is the configured
// public base when set, else inferred from the request (forwarded scheme/TLS +
// Host) so a dev or un-proxied deployment still produces a working zip.
func (s *Server) handleSkillDownload(w http.ResponseWriter, r *http.Request) {
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		// Defensive: the middleware guarantees a valid key on this route, so this
		// only fires if the handler is wired up without it.
		writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
		return
	}

	baseURL := s.skillBaseURL(r)
	zipBytes, err := buildSkillZip(baseURL, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build skill archive")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="ecu-computer-use.zip"`)
	_, _ = w.Write(zipBytes)
}

// skillBaseURL is the control-plane base URL to bake into a downloaded skill. It
// prefers the explicitly configured public base (publicBaseURL); when that is
// empty (e.g. dev, or no public base configured) it reconstructs one from the
// request: the scheme from X-Forwarded-Proto (set by the TLS-terminating proxy),
// else https when the request itself arrived over TLS, else http; and the host
// from r.Host. This mirrors how the skill's own ECU_URL is shaped.
func (s *Server) skillBaseURL(r *http.Request) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
