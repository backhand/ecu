package controlplane

import (
	"net/http"
	"net/url"
)

// SessionTransport resolves a session id to an http.RoundTripper that reaches
// that session's tool server. Component 2 ships directTransport (plain HTTP to
// the stored tool_endpoint); Component 3 will add a tunnelTransport implementing
// the same interface, swapped in without touching the handlers.
type SessionTransport interface {
	RoundTripper(sessionID string) (http.RoundTripper, bool)
}

// EndpointResolver maps a session id to the base URL of its tool server. ok is
// false when the session or its endpoint is unknown. directTransport depends
// only on this narrow function so the handlers never read tool_endpoint out of
// the store themselves — the endpoint string stays entirely on the transport
// side of the seam and can never leak into a client-facing response.
type EndpointResolver func(sessionID string) (endpoint string, ok bool)

// directTransport is the Component 2 SessionTransport. Given a session id it
// looks up the tool-server base URL and returns an http.RoundTripper that
// rewrites each outbound request's scheme and host to that endpoint before
// delegating to a single shared *http.Transport. The handler builds requests
// against a relative path ("/"+action) and never sees the endpoint, so the
// upstream address cannot leak through handler code.
//
// Component 3 will provide a tunnelTransport that implements the same
// SessionTransport interface (resolving the id to a yamux stream instead of a
// rewritten URL); because the handlers depend only on SessionTransport, that
// swap touches no handler code.
type directTransport struct {
	resolve EndpointResolver

	// shared is the underlying transport that actually performs network I/O;
	// reused across all sessions for connection pooling.
	shared *http.Transport
}

// newDirectTransport builds a directTransport backed by the given resolver and
// a shared *http.Transport cloned from http.DefaultTransport.
func newDirectTransport(resolve EndpointResolver) *directTransport {
	return &directTransport{
		resolve: resolve,
		shared:  http.DefaultTransport.(*http.Transport).Clone(),
	}
}

// RoundTripper returns a per-session http.RoundTripper for sessionID. ok is
// false when the session/endpoint is unknown (the handler then responds 404
// without ever having touched the endpoint string).
func (t *directTransport) RoundTripper(sessionID string) (http.RoundTripper, bool) {
	endpoint, ok := t.resolve(sessionID)
	if !ok || endpoint == "" {
		return nil, false
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" {
		return nil, false
	}
	return &endpointRoundTripper{base: base, next: t.shared}, true
}

// endpointRoundTripper rewrites a request's URL scheme/host (and Host header) to
// a fixed tool-server base, then delegates to next. The inbound request only
// carries a relative path like "/click"; this is where it is bound to the
// actual upstream, keeping that binding off the handler path.
type endpointRoundTripper struct {
	base *url.URL
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It clones the request shallowly,
// points it at the base endpoint while preserving the original path, and
// forwards it via the shared transport.
func (rt *endpointRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = rt.base.Scheme
	out.URL.Host = rt.base.Host
	// Preserve any base path prefix (e.g. http://host/v1) by joining it with
	// the request's relative path.
	out.URL.Path = singleJoin(rt.base.Path, req.URL.Path)
	out.Host = rt.base.Host
	// RequestURI must be empty for client requests.
	out.RequestURI = ""
	return rt.next.RoundTrip(out)
}

// singleJoin joins a base path and a request path with exactly one separating
// slash, tolerating empty/“/” bases.
func singleJoin(basePath, reqPath string) string {
	switch {
	case basePath == "" || basePath == "/":
		return reqPath
	case reqPath == "":
		return basePath
	}
	bEnds := basePath[len(basePath)-1] == '/'
	rStarts := reqPath[0] == '/'
	switch {
	case bEnds && rStarts:
		return basePath + reqPath[1:]
	case !bEnds && !rStarts:
		return basePath + "/" + reqPath
	default:
		return basePath + reqPath
	}
}
