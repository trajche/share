package ratelimit

import (
	"net/http"
	"net/http/httptest"
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
	l := New(10, 2)
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
	l := New(1, 5)
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
	l := New(0, 0)
	mw := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, request(m, "1.1.1.1"))
		if rec.Code != http.StatusOK {
			t.Errorf("%s limited: status %d", m, rec.Code)
		}
	}
}
