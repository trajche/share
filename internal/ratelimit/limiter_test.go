package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
)

// blockingHandler holds requests open until release is closed.
type blockingHandler struct {
	started chan struct{}
	release chan struct{}
}

func (h *blockingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.started <- struct{}{}
	<-h.release
}

func request(method, ip string) *http.Request {
	r := httptest.NewRequest(method, "/files/", nil)
	r.RemoteAddr = ip + ":1234"
	return r
}

func TestPerIPLimit(t *testing.T) {
	l := New(10, 2, nil)
	h := &blockingHandler{started: make(chan struct{}, 10), release: make(chan struct{})}
	mw := l.Middleware(h)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mw.ServeHTTP(httptest.NewRecorder(), request(http.MethodPatch, "1.1.1.1"))
		}()
		<-h.started
	}

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, request(http.MethodPatch, "1.1.1.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("third concurrent request from same IP: status %d, want 429", rec.Code)
	}

	// Another IP is unaffected.
	go mw.ServeHTTP(httptest.NewRecorder(), request(http.MethodPost, "2.2.2.2"))
	<-h.started

	close(h.release)
	wg.Wait()

	// Slots are released afterwards.
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec = httptest.NewRecorder()
	l.Middleware(h2).ServeHTTP(rec, request(http.MethodPatch, "1.1.1.1"))
	if rec.Code != http.StatusOK {
		t.Errorf("after release: status %d, want 200", rec.Code)
	}
}

func TestGlobalLimit(t *testing.T) {
	l := New(1, 5, nil)
	h := &blockingHandler{started: make(chan struct{}, 1), release: make(chan struct{})}
	mw := l.Middleware(h)

	go mw.ServeHTTP(httptest.NewRecorder(), request(http.MethodPost, "1.1.1.1"))
	<-h.started

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, request(http.MethodPost, "2.2.2.2"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429", rec.Code)
	}
	close(h.release)
}

func TestReadsAreNotLimited(t *testing.T) {
	l := New(0, 0, nil)
	mw := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, request(m, "1.1.1.1"))
		if rec.Code != http.StatusOK {
			t.Errorf("%s limited: status %d", m, rec.Code)
		}
	}
}

func TestIdleIPsAreForgotten(t *testing.T) {
	l := New(10, 2, nil)
	mw := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for i := range 100 {
		mw.ServeHTTP(httptest.NewRecorder(), request(http.MethodPost, "10.0.0."+strconv.Itoa(i)))
	}
	if n := l.tracked(); n != 0 {
		t.Errorf("%d idle IPs still tracked", n)
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	l := New(1, 1, trusted)

	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct client ignores spoofed XFF", "203.0.113.9:5555", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted proxy, single hop", "127.0.0.1:5555", []string{"198.51.100.7"}, "198.51.100.7"},
		{"client-supplied prefix is ignored", "127.0.0.1:5555", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7"},
		{"skips chained trusted proxies", "127.0.0.1:5555", []string{"1.2.3.4, 198.51.100.7, 10.1.2.3"}, "198.51.100.7"},
		{"multiple header lines", "127.0.0.1:5555", []string{"1.2.3.4", "198.51.100.7"}, "198.51.100.7"},
		{"malformed hop stops the walk", "127.0.0.1:5555", []string{"198.51.100.7, garbage"}, "127.0.0.1"},
		{"no header from proxy", "127.0.0.1:5555", nil, "127.0.0.1"},
		{"ipv4-mapped ipv6 peer", "[::ffff:203.0.113.9]:5555", nil, "203.0.113.9"},
		{"ipv6 client", "127.0.0.1:5555", []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/files/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := l.ClientIP(r); got != c.want {
			t.Errorf("%s: ClientIP = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSpoofedXFFCannotDodgePerIPLimit(t *testing.T) {
	l := New(10, 1, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	h := &blockingHandler{started: make(chan struct{}, 1), release: make(chan struct{})}
	mw := l.Middleware(h)

	first := request(http.MethodPost, "127.0.0.1")
	first.Header.Set("X-Forwarded-For", "1.1.1.1, 198.51.100.7")
	go mw.ServeHTTP(httptest.NewRecorder(), first)
	<-h.started

	// Same real client behind the proxy, different forged leftmost value.
	second := request(http.MethodPost, "127.0.0.1")
	second.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.7")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429", rec.Code)
	}
	close(h.release)
}
