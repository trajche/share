package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// Limiter caps concurrent POST/PATCH requests globally and per client IP.
type Limiter struct {
	mu        sync.Mutex
	global    int
	perIP     map[string]int
	globalMax int
	perIPMax  int
	trusted   []netip.Prefix
}

// New returns a limiter. trusted lists proxies whose X-Forwarded-For header
// is believed; requests from any other peer are keyed by their own address.
func New(globalMax, perIPMax int, trusted []netip.Prefix) *Limiter {
	return &Limiter{
		perIP:     make(map[string]int),
		globalMax: globalMax,
		perIPMax:  perIPMax,
		trusted:   trusted,
	}
}

func (l *Limiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global >= l.globalMax || l.perIP[ip] >= l.perIPMax {
		return false
	}
	l.global++
	l.perIP[ip]++
	return true
}

func (l *Limiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.global--
	// Drop idle entries so the map does not grow with every client ever seen.
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

// tracked returns the number of IPs with requests in flight (for tests).
func (l *Limiter) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.perIP)
}

// Middleware wraps the given handler, rate-limiting POST and PATCH requests.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)
			return
		}

		ip := l.ClientIP(r)
		if !l.acquire(ip) {
			http.Error(w, "too many concurrent uploads", http.StatusTooManyRequests)
			return
		}
		defer l.release(ip)

		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the address of the client that made r. X-Forwarded-For
// is only consulted when the direct peer is a trusted proxy, and is read
// right to left (skipping further trusted proxies) because only the entries
// appended by our own proxies can be believed; the leftmost value is
// whatever the client chose to send.
func (l *Limiter) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !l.isTrusted(peer) {
		return peer.String()
	}

	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // malformed: stop at the last address we could trust
		}
		client = addr.Unmap()
		if !l.isTrusted(client) {
			break
		}
	}
	return client.String()
}

func (l *Limiter) isTrusted(addr netip.Addr) bool {
	for _, p := range l.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
