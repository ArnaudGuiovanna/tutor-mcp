package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrincipalConcurrencyLimiterBoundsGlobalAndPrincipalWork(t *testing.T) {
	limiter, err := NewPrincipalConcurrencyLimiter(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 3)
	release := make(chan struct{})
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- GetLearnerID(r.Context())
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	serveAsync := func(principal string) <-chan *httptest.ResponseRecorder {
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req = req.WithContext(context.WithValue(req.Context(), LearnerIDKey, principal))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			result <- rec
		}()
		return result
	}

	var active []<-chan *httptest.ResponseRecorder
	active = append(active, serveAsync("learner-a"), serveAsync("learner-a"))
	for range 2 {
		<-entered
	}
	rejectedA := serveAsync("learner-a")
	if rec := <-rejectedA; rec.Code != http.StatusTooManyRequests {
		t.Fatalf("per-principal overflow status=%d", rec.Code)
	}
	active = append(active, serveAsync("learner-b"))
	<-entered
	rejectedGlobal := serveAsync("learner-c")
	if rec := <-rejectedGlobal; rec.Code != http.StatusTooManyRequests {
		t.Fatalf("global overflow status=%d", rec.Code)
	}

	close(release)
	for _, result := range active {
		if rec := <-result; rec.Code != http.StatusNoContent {
			t.Fatalf("active response status=%d", rec.Code)
		}
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.inFlight != 0 || len(limiter.byPrincipal) != 0 {
		t.Fatalf("leaked limiter state: in_flight=%d principals=%d", limiter.inFlight, len(limiter.byPrincipal))
	}
}

func TestPrincipalConcurrencyLimiterValidation(t *testing.T) {
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {2, 3}} {
		if _, err := NewPrincipalConcurrencyLimiter(limits[0], limits[1]); err == nil {
			t.Fatalf("limits %v accepted", limits)
		}
	}
}
