package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

type Limiter struct {
	globalSem chan struct{}
	mu        sync.Mutex
	perIP     map[string]chan struct{}
	perIPMax  int
}

func New(globalMax, perIPMax int) *Limiter {
	return &Limiter{
		globalSem: make(chan struct{}, globalMax),
		perIP:     make(map[string]chan struct{}),
		perIPMax:  perIPMax,
	}
}

func (l *Limiter) acquire(ip string) bool {
	// Try to acquire global slot (non-blocking).
	select {
	case l.globalSem <- struct{}{}:
	default:
		return false
	}

	// Try to acquire per-IP slot (non-blocking).
	ch := l.ipChan(ip)
	select {
	case ch <- struct{}{}:
		return true
	default:
		// Release the global slot we just acquired.
		<-l.globalSem
		return false
	}
}

func (l *Limiter) release(ip string) {
	ch := l.ipChan(ip)
	select {
	case <-ch:
	default:
	}
	select {
	case <-l.globalSem:
	default:
	}
}

func (l *Limiter) ipChan(ip string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, ok := l.perIP[ip]
	if !ok {
		ch = make(chan struct{}, l.perIPMax)
		l.perIP[ip] = ch
	}
	return ch
}

// Middleware wraps the given handler, rate-limiting POST and PATCH requests.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)
			return
		}

		ip := realIP(r)
		if !l.acquire(ip) {
			http.Error(w, "too many concurrent uploads", http.StatusTooManyRequests)
			return
		}
		defer l.release(ip)

		next.ServeHTTP(w, r)
	})
}

func realIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first (leftmost) IP in the chain.
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
