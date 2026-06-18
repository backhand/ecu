// Package tlsboot wires the control plane's automatic-TLS ("ECU_TLS=auto")
// path: it resolves the certificate hostname, builds a golang.org/x/crypto
// autocert manager (Let's Encrypt over the HTTP-01 challenge), and serves the
// control-plane handler over HTTPS on :443 while serving the ACME challenge and
// an HTTP->HTTPS redirect on :80.
//
// The design separates a PURE, testable core from the network edges so the
// interesting logic is unit-tested without a live network or a real ACME
// server:
//
//   - DashEncodeIP and ResolveHostname are pure (the latter takes the public-IP
//     detector as an injected PublicIPFunc), so hostname resolution — including
//     the nip.io fallback — is fully covered offline.
//   - DetectPublicIP is the real, network-backed PublicIPFunc used in
//     production; it is intentionally NOT unit-tested (it talks to the network).
//   - Serve composes them with autocert and the stdlib http servers.
//
// nip.io caveat: when no explicit ECU_HOSTNAME is set, resolution falls back to
// "<dashed-public-ip>.nip.io", a public wildcard-DNS service that resolves any
// "a-b-c-d.nip.io" to a.b.c.d. This makes the try-it / first-boot path work with
// zero DNS setup, but nip.io is a SHARED domain: Let's Encrypt enforces
// per-registered-domain certificate rate limits, so heavy use of *.nip.io can
// hit those limits. It is a convenience for trying ECU out, NOT a long-lived
// production hostname — point a real DNS A record at the box and set
// ECU_HOSTNAME for anything durable.
package tlsboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// Default bind addresses for the autocert path. HTTPS serves the control-plane
// handler; HTTP serves the ACME HTTP-01 challenge and redirects everything else
// to HTTPS. They are overridable via Options.HTTPSAddr / Options.HTTPAddr (the
// only reason to override is a test on ephemeral ports; production always uses
// the well-known ports because Let's Encrypt validates HTTP-01 on :80).
const (
	defaultHTTPSAddr = ":443"
	defaultHTTPAddr  = ":80"
	// tlsCacheDirPerm is the permission for the autocert on-disk cache. 0700:
	// the cache holds the ACME account key and issued certificate private keys,
	// so it must not be world- or group-readable.
	tlsCacheDirPerm = 0o700
	// shutdownTimeout bounds graceful drain of the two servers on ctx
	// cancellation. Mirrors the control plane's own 10s shutdown budget in
	// cmd/ecu/main.go.
	shutdownTimeout = 10 * time.Second
	// detectTimeout bounds the total public-IP detection across all endpoints.
	detectTimeout = 5 * time.Second
)

// PublicIPFunc detects the instance's public IPv4. Injected so resolution is
// testable without network.
type PublicIPFunc func(ctx context.Context) (string, error)

// Options configures Serve.
type Options struct {
	// Mode is the ECU_TLS value; Serve requires it to be "auto" (callers gate on
	// Enabled). Carried so Serve can re-assert the precondition.
	Mode string
	// Hostname is the ECU_HOSTNAME value (may be empty). When empty, Serve falls
	// back to the public-IP nip.io host (see ResolveHostname).
	Hostname string
	// CacheDir is the ECU_TLS_CACHE_DIR value: where autocert persists certs and
	// its account key. Created with 0700 if absent.
	CacheDir string
	// DetectIP is the public-IP detector used for the nip.io fallback. nil means
	// the production DetectPublicIP.
	DetectIP PublicIPFunc
	// HTTPSAddr / HTTPAddr override the bind addresses. Empty means the
	// well-known defaults (:443 / :80). Overridable only so a test could bind
	// ephemeral ports; production leaves them empty.
	HTTPSAddr string
	HTTPAddr  string
}

// Enabled reports whether the TLS mode opts into the autocert path. It is true
// iff mode is "auto" (case-insensitive, surrounding whitespace ignored), so the
// default "off" — and any unrecognized value — keeps the plain-HTTP path.
func Enabled(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "auto")
}

// DashEncodeIP validates that ip is a textual IPv4 address and returns its
// dash-encoded nip.io host, e.g. "203.0.113.7" -> "203-0-113-7.nip.io". Only
// IPv4 is supported: nip.io's dash form is IPv4-only, and a dashed IPv6 host
// would not resolve. An IPv6 address or any non-IP string yields an error.
func DashEncodeIP(ip string) (string, error) {
	trimmed := strings.TrimSpace(ip)
	parsed := net.ParseIP(trimmed)
	if parsed == nil {
		return "", fmt.Errorf("tlsboot: %q is not a valid IP address", ip)
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", fmt.Errorf("tlsboot: %q is not an IPv4 address (nip.io dash encoding is IPv4-only)", ip)
	}
	// Re-render from the parsed 4-byte form rather than string-substituting the
	// input, so any accepted-but-noncanonical textual form normalizes (e.g. the
	// host is always built from a.b.c.d as parsed by net.ParseIP).
	dashed := fmt.Sprintf("%d-%d-%d-%d", v4[0], v4[1], v4[2], v4[3])
	return dashed + ".nip.io", nil
}

// ResolveHostname resolves the hostname autocert should obtain a certificate
// for. If explicit (the ECU_HOSTNAME value) is non-empty it wins verbatim
// (trimmed) and detect is NOT called. Otherwise detect is invoked and its
// returned public IPv4 is dash-encoded into "<dashed>.nip.io".
//
// Returns a clear error if detection fails or yields something that is not a
// valid IPv4.
//
// nip.io caveat (see the package doc): the fallback uses the SHARED nip.io
// wildcard-DNS domain so the first-boot / try-it path needs zero DNS setup, but
// Let's Encrypt's per-registered-domain rate limits apply to *.nip.io. It is a
// convenience, not a long-lived production hostname — set ECU_HOSTNAME with a
// real DNS A record for anything durable.
func ResolveHostname(ctx context.Context, explicit string, detect PublicIPFunc) (string, error) {
	if h := strings.TrimSpace(explicit); h != "" {
		return h, nil
	}
	if detect == nil {
		return "", errors.New("tlsboot: no ECU_HOSTNAME set and no public-IP detector provided")
	}
	ip, err := detect(ctx)
	if err != nil {
		return "", fmt.Errorf("tlsboot: detecting public IP for nip.io fallback: %w", err)
	}
	host, err := DashEncodeIP(ip)
	if err != nil {
		return "", fmt.Errorf("tlsboot: public-IP detector returned an unusable address: %w", err)
	}
	return host, nil
}

// publicIPEndpoints are well-known plaintext IPv4 echo services queried over
// HTTPS by DetectPublicIP, tried in order. Each returns the caller's public IP
// as a bare string body.
var publicIPEndpoints = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
}

// DetectPublicIP is the production PublicIPFunc: it queries a couple of
// well-known plaintext IP-echo endpoints over HTTPS (in order, first valid IPv4
// wins) under a short overall timeout, returning the detected public IPv4.
//
// It is deliberately simple and resilient — any endpoint that errors, returns
// non-2xx, or returns a non-IPv4 body is skipped and the next is tried. It is
// NOT unit-tested (it talks to the network); the pure resolution logic that
// consumes it is tested via an injected stub PublicIPFunc.
func DetectPublicIP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	client := &http.Client{}
	var lastErr error
	for _, endpoint := range publicIPEndpoints {
		ip, err := fetchIP(ctx, client, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		return ip, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no endpoints configured")
	}
	return "", fmt.Errorf("tlsboot: could not detect public IPv4 from any endpoint: %w", lastErr)
}

// fetchIP GETs endpoint and returns the trimmed body if it is a valid IPv4.
func fetchIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	// Cap the read: a valid response is a short IP string; anything large is a
	// misbehaving endpoint.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("%s: %q is not an IPv4 address", endpoint, ip)
	}
	return ip, nil
}

// newManager builds an autocert.Manager pinned to a single host. It accepts the
// Let's Encrypt TOS (Prompt), whitelists exactly host (HostPolicy) so the
// manager will only ever request a certificate for the resolved hostname, and
// caches issued certs + the account key on disk (Cache). The cache directory is
// created 0700 first because autocert.DirCache does not create it and the cache
// holds private key material.
func newManager(host, cacheDir string) (*autocert.Manager, error) {
	if err := os.MkdirAll(cacheDir, tlsCacheDirPerm); err != nil {
		return nil, fmt.Errorf("tlsboot: creating TLS cache dir %q: %w", cacheDir, err)
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(host),
		Cache:      autocert.DirCache(cacheDir),
	}, nil
}

// Serve runs the automatic-TLS path until ctx is cancelled. It:
//
//  1. re-asserts Mode is "auto" (callers gate on Enabled, but Serve is the only
//     entry point so it validates defensively);
//  2. resolves the certificate hostname (ECU_HOSTNAME, else the nip.io fallback
//     via the public-IP detector) — wrapping a resolution failure in a clear,
//     actionable error;
//  3. builds the autocert manager pinned to that host;
//  4. serves handler over HTTPS on HTTPSAddr using manager.TLSConfig(), and
//     serves manager.HTTPHandler(nil) on HTTPAddr — which answers the HTTP-01
//     challenge and 301-redirects every other HTTP request to HTTPS.
//
// On ctx cancellation both servers are gracefully shut down with a bounded
// timeout. Serve returns the first non-ErrServerClosed error from either
// server (or nil on clean shutdown).
//
// Note: Mode "auto" IGNORES ECU_LISTEN — autocert must bind the well-known
// :443/:80 ports for Let's Encrypt's HTTP-01 validation, so the listen address
// does not apply. Behind a k3s Ingress (which terminates TLS) run ECU_TLS=off
// instead.
func Serve(ctx context.Context, handler http.Handler, opts Options) error {
	if !Enabled(opts.Mode) {
		return fmt.Errorf("tlsboot: Serve called with TLS mode %q, want \"auto\"", opts.Mode)
	}

	detect := opts.DetectIP
	if detect == nil {
		detect = DetectPublicIP
	}
	host, err := ResolveHostname(ctx, opts.Hostname, detect)
	if err != nil {
		return fmt.Errorf("ECU_TLS=auto but no TLS hostname could be resolved: set ECU_HOSTNAME or ensure public IP detection works: %w", err)
	}

	manager, err := newManager(host, opts.CacheDir)
	if err != nil {
		return err
	}

	httpsAddr := opts.HTTPSAddr
	if httpsAddr == "" {
		httpsAddr = defaultHTTPSAddr
	}
	httpAddr := opts.HTTPAddr
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}

	log.Printf("ecu: TLS mode=auto (autocert, HTTP-01); certificate hostname=%q; serving HTTPS %s, HTTP-01+redirect %s", host, httpsAddr, httpAddr)
	if strings.HasSuffix(host, ".nip.io") {
		log.Printf("ecu: NOTE: using nip.io fallback host %q — shared domain, subject to Let's Encrypt per-domain rate limits; for production set ECU_HOSTNAME to a real DNS name", host)
	}

	httpsServer := &http.Server{
		Addr:      httpsAddr,
		Handler:   handler,
		TLSConfig: manager.TLSConfig(),
	}
	// manager.HTTPHandler(nil) answers the ACME HTTP-01 challenge at
	// /.well-known/acme-challenge/ and 301-redirects all other HTTP traffic to
	// HTTPS (the default fallback when the supplied handler is nil).
	httpServer := &http.Server{
		Addr:    httpAddr,
		Handler: manager.HTTPHandler(nil),
	}

	// One buffered slot per server so a send never leaks a goroutine even if we
	// return on ctx.Done() without draining.
	serveErr := make(chan error, 2)
	go func() {
		// Certificates come from manager.TLSConfig(); the empty cert/key args are
		// correct for autocert-managed TLS.
		serveErr <- httpsServer.ListenAndServeTLS("", "")
	}()
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// A server returned before shutdown. ErrServerClosed is benign (the other
		// goroutine's Shutdown can race to it); anything else is fatal. Either way,
		// tear the sibling down so we do not leak it.
		shutdownBoth(httpsServer, httpServer)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownBoth(httpsServer, httpServer)
		return nil
	}
}

// shutdownBoth gracefully shuts down both servers with a single bounded
// timeout. Errors are ignored: Shutdown only fails when the deadline elapses,
// which we cannot meaningfully act on during teardown.
func shutdownBoth(httpsServer, httpServer *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = httpsServer.Shutdown(shutdownCtx)
	_ = httpServer.Shutdown(shutdownCtx)
}
