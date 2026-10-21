// Package edge serves each service's HTTPS address and proxies it to
// whichever revision is live.
//
// Every service is its own tailnet node with its own certificate. The node
// stays up across deploys; only the proxy's upstream changes, through one
// atomic pointer store. Requests that already loaded the old upstream finish
// on it, which is why replaced revisions drain before they stop.
//
// Beside HTTPS, a node can serve raw TCP and UDP ports (see raw.go), which
// switch the same way: new connections and flows go to the live revision,
// and ones already open stay on the revision they started on.
package edge

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
)

// Identity is the tailnet user behind a request.
type Identity struct {
	Login string
	Name  string
}

// WhoIs identifies the sender of a request by its remote address. A tagged
// device has no user identity and returns the zero Identity.
type WhoIs func(ctx context.Context, remoteAddr string) (Identity, error)

type anonymousKey struct{}
type identityKey struct{}

// Anonymous marks a Funnel connection: public traffic that never gets
// identity headers.
func Anonymous(ctx context.Context) context.Context {
	return context.WithValue(ctx, anonymousKey{}, true)
}

// Target is a reverse proxy to one loopback upstream, and to the upstreams
// that serve path prefixes beside it.
type Target struct {
	proxy  *httputil.ReverseProxy
	routes []route // longest prefix first
}

type route struct {
	prefix string
	target *Target
}

// NewTarget accepts only numeric loopback HTTP origins: pods publish on
// 127.0.0.1, and nothing can turn the edge into a proxy to other hosts.
// protocol is what the upstream speaks: "" or spec.ProtocolHTTP for
// HTTP/1.1, or spec.ProtocolH2C for HTTP/2 without TLS.
func NewTarget(rawURL, dnsName, protocol string) (*Target, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("upstream must be a loopback HTTP origin")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return nil, errors.New("upstream must be a numeric loopback address with a port")
	}
	if dnsName == "" || strings.ContainsAny(dnsName, "/\r\n") {
		return nil, errors.New("invalid edge DNS name")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 128
	// Generous, so slow synchronous handlers (long LLM calls, say) can finish.
	transport.ResponseHeaderTimeout = 10 * time.Minute
	switch protocol {
	case "", spec.ProtocolHTTP:
	case spec.ProtocolH2C:
		// Every request goes upstream as HTTP/2, so gRPC's trailers and
		// bidirectional streams work whichever protocol the caller used.
		var only http.Protocols
		only.SetUnencryptedHTTP2(true)
		transport.Protocols = &only
	default:
		return nil, errors.New("unknown upstream protocol " + protocol)
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // stream responses as they are written
		ErrorLog:      log.New(log.Writer(), "edge: ", log.LstdFlags),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = dnsName
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", dnsName)
			// Rewrite runs after hop-by-hop headers are removed, so a forged
			// Connection header cannot strip the identity set here.
			for key := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(key), "tailscale-user-") {
					delete(pr.Out.Header, key)
				}
			}
			if id, ok := pr.In.Context().Value(identityKey{}).(Identity); ok && id.Login != "" {
				pr.Out.Header.Set("Tailscale-User-Login", id.Login)
				pr.Out.Header.Set("Tailscale-User-Name", id.Name)
			}
		},
	}
	return &Target{proxy: proxy}, nil
}

// Route sends requests whose path is prefix, or below it, to to. The longest
// matching prefix wins. Routes are set up before the target is switched in.
func (t *Target) Route(prefix string, to *Target) {
	t.routes = append(t.routes, route{prefix, to})
	sort.SliceStable(t.routes, func(i, j int) bool { return len(t.routes[i].prefix) > len(t.routes[j].prefix) })
}

// pick returns the target serving path.
func (t *Target) pick(path string) *Target {
	for _, r := range t.routes {
		if path == r.prefix || strings.HasPrefix(path, r.prefix+"/") {
			return r.target
		}
	}
	return t
}

// Edge is the HTTP handler for one service.
type Edge struct {
	upstream atomic.Pointer[Target]
	whoIs    WhoIs
}

func New(whoIs WhoIs) *Edge { return &Edge{whoIs: whoIs} }

// Switch points new requests at target, or at nothing (503) when nil.
func (e *Edge) Switch(target *Target) { e.upstream.Store(target) }

func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := e.upstream.Load()
	if target == nil {
		http.Error(w, "no live revision", http.StatusServiceUnavailable)
		return
	}
	if anonymous, _ := r.Context().Value(anonymousKey{}).(bool); !anonymous {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		id, err := e.whoIs(ctx, r.RemoteAddr)
		cancel()
		if err != nil {
			http.Error(w, "cannot identify tailnet caller", http.StatusBadGateway)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
	}
	target.pick(r.URL.Path).proxy.ServeHTTP(w, r)
}
