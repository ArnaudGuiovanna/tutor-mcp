// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	tutortools "tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestShouldPrintVersion(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-version"}, {"version"}} {
		if !shouldPrintVersion(args) {
			t.Fatalf("shouldPrintVersion(%v) = false", args)
		}
	}
	if shouldPrintVersion(nil) || shouldPrintVersion([]string{"--version", "extra"}) || shouldPrintVersion([]string{"--help"}) {
		t.Fatal("shouldPrintVersion accepted an unsupported argument shape")
	}
}

func TestVersionLine(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	defer func() {
		version, commit, buildDate = oldVersion, oldCommit, oldBuildDate
	}()
	version = "v0.4.1"
	commit = "abc1234"
	buildDate = "2026-05-14T16:00:00Z"

	got := versionLine()
	for _, want := range []string{"tutor-mcp", "v0.4.1", "abc1234", "2026-05-14T16:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionLine() = %q, missing %q", got, want)
		}
	}
	if mcpVersion() != "0.4.1" {
		t.Fatalf("mcpVersion() = %q", mcpVersion())
	}
}

type readinessTestPinger struct {
	err             error
	deadlinePresent bool
}

func (p *readinessTestPinger) Ping(ctx context.Context) error {
	_, p.deadlinePresent = ctx.Deadline()
	return p.err
}

func TestLiveAndReadyHealthContracts(t *testing.T) {
	live := httptest.NewRecorder()
	livenessHandler(live, httptest.NewRequest(http.MethodGet, "/live", nil))
	if live.Code != http.StatusOK || live.Body.String() != `{"status":"live"}` {
		t.Fatalf("live response status=%d body=%q", live.Code, live.Body.String())
	}

	pinger := &readinessTestPinger{err: context.DeadlineExceeded}
	ready := httptest.NewRecorder()
	readinessHandler(pinger)(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || !pinger.deadlinePresent {
		t.Fatalf("ready response status=%d deadline=%v body=%q", ready.Code, pinger.deadlinePresent, ready.Body.String())
	}
	pinger.err = nil
	ready = httptest.NewRecorder()
	readinessHandler(pinger)(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if ready.Code != http.StatusOK || ready.Body.String() != `{"status":"ready"}` {
		t.Fatalf("ready success status=%d body=%q", ready.Code, ready.Body.String())
	}
}

func TestRecoveryMiddlewareDoesNotLogPanicValueOrStack(t *testing.T) {
	const secret = "panic-secret-reset-token"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := recoveryMiddleware(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(secret)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "goroutine ") {
		t.Fatalf("panic log disclosed sensitive value or raw stack: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "stack_fingerprint") {
		t.Fatalf("panic log missing correlation fingerprint: %s", logs.String())
	}
}

func TestPrivacySafeLogAttrPseudonymizesLearningIdentifiers(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{
		ReplaceAttr: newPrivacySafeLogAttr(),
	}))
	logger.Info("interaction", "learner", "learner-secret", "concept", "sensitive-topic", "status", "ok")
	logged := out.String()
	for _, secret := range []string{"learner-secret", "sensitive-topic"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("sensitive structured attribute leaked in log: %q", logged)
		}
	}
	if !strings.Contains(logged, "learner=anon:") || !strings.Contains(logged, "concept=anon:") {
		t.Fatalf("expected correlatable pseudonyms, got %q", logged)
	}
	if !strings.Contains(logged, "status=ok") {
		t.Fatalf("non-sensitive operational attribute was changed: %q", logged)
	}
}

type streamingTestWriter struct {
	header        http.Header
	status        int
	body          strings.Builder
	flushed       bool
	writeDeadline time.Time
}

type singleConnListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	conns := make(chan net.Conn, 1)
	conns <- conn
	return &singleConnListener{conns: conns, closed: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *singleConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return testAddr("pipe") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func (w *streamingTestWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *streamingTestWriter) WriteHeader(status int) { w.status = status }

func (w *streamingTestWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *streamingTestWriter) Flush() { w.flushed = true }

func (w *streamingTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

func TestMCPMiddlewarePreservesStreamingCapabilities(t *testing.T) {
	base := &streamingTestWriter{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("request logger hid http.Flusher from MCP handler")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("flush through middleware: %v", err)
		}
		_, _ = w.Write([]byte("event: ping\n\n"))
	})
	handler := recoveryMiddleware(logger, requestLogger(logger,
		securityHeaders("https://tutor.example",
			corsMiddleware([]string{"https://claude.ai"}, nil,
				withoutWriteTimeout(inner)))))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	handler.ServeHTTP(base, req)

	if !base.flushed {
		t.Fatal("underlying writer was not flushed")
	}
	if !base.writeDeadline.IsZero() {
		t.Fatalf("MCP write deadline = %v, want disabled zero deadline", base.writeDeadline)
	}
	if base.body.String() != "event: ping\n\n" {
		t.Fatalf("stream body = %q", base.body.String())
	}
}

func TestWithoutWriteTimeoutKeepsSSEAlivePastServerDeadline(t *testing.T) {
	const writeTimeout = 25 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: first\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("first flush: %v", err)
			return
		}
		time.Sleep(4 * writeTimeout)
		_, _ = w.Write([]byte("event: second\n\n"))
		_ = http.NewResponseController(w).Flush()
	})
	handler := recoveryMiddleware(logger, requestLogger(logger,
		securityHeaders("https://tutor.example",
			corsMiddleware(nil, nil, withoutWriteTimeout(stream)))))

	clientConn, serverConn := net.Pipe()
	listener := newSingleConnListener(serverConn)
	server := &http.Server{Handler: handler, WriteTimeout: writeTimeout}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	})

	req, err := http.NewRequest(http.MethodGet, "http://tutor.example/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Close = true
	if err := req.Write(clientConn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, event := range []string{"event: first", "event: second"} {
		if !strings.Contains(string(body), event) {
			t.Fatalf("stream body %q missing %q", body, event)
		}
	}
}

func TestCORSAllowsMCP20260728RequestHeaders(t *testing.T) {
	handler := corsMiddleware([]string{"https://claude.ai"}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight reached the MCP handler")
	}))
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", strings.Join([]string{
		"Authorization",
		"Content-Type",
		"Mcp-Protocol-Version",
		"Mcp-Session-Id",
		"Last-Event-ID",
		"Mcp-Method",
		"Mcp-Name",
		"Mcp-Param-Region",
	}, ", "))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://claude.ai" {
		t.Fatalf("allow origin=%q", got)
	}
	allowed := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
	for _, header := range []string{
		"authorization", "content-type", "mcp-protocol-version", "mcp-session-id",
		"last-event-id", "mcp-method", "mcp-name", "mcp-param-region",
	} {
		if !strings.Contains(allowed, header) {
			t.Errorf("allow headers %q missing %q", allowed, header)
		}
	}
}

func TestCORSDoesNotReflectUnknownRequestHeader(t *testing.T) {
	handler := corsMiddleware([]string{"https://claude.ai"}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "Authorization, X-Untrusted-Header")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if got := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")); strings.Contains(got, "x-untrusted-header") {
		t.Fatalf("unknown request header was reflected: %q", got)
	}
}

func TestMCPToolCallDeadlineOnlyBoundsToolCalls(t *testing.T) {
	const timeout = 25 * time.Millisecond
	deadlineSeen := false
	handler := mcpToolCallDeadlineMiddleware(timeout)(func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			t.Fatalf("method=%q", method)
		}
		_, deadlineSeen = ctx.Deadline()
		<-ctx.Done()
		return nil, ctx.Err()
	})

	started := time.Now()
	result, err := handler(context.Background(), "tools/call", nil)
	if err != nil {
		t.Fatalf("deadline middleware returned protocol error: %v", err)
	}
	if !deadlineSeen {
		t.Fatal("tool handler context had no deadline")
	}
	toolResult, ok := result.(*mcp.CallToolResult)
	if !ok || !toolResult.IsError || !strings.Contains(callToolText(toolResult), "server deadline") {
		t.Fatalf("deadline result=%#v", result)
	}
	if elapsed := time.Since(started); elapsed < timeout || elapsed > time.Second {
		t.Fatalf("deadline elapsed=%v, want [%v, 1s]", elapsed, timeout)
	}

	listHandler := mcpToolCallDeadlineMiddleware(timeout)(func(ctx context.Context, method string, _ mcp.Request) (mcp.Result, error) {
		if method != "tools/list" {
			t.Fatalf("method=%q", method)
		}
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("non-tool-call method received a tool deadline")
		}
		return &mcp.ListToolsResult{}, nil
	})
	if _, err := listHandler(context.Background(), "tools/list", nil); err != nil {
		t.Fatal(err)
	}

	// A shorter upstream deadline belongs to the caller/transport and must not
	// be mislabeled as the server's configured tool deadline.
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err = handler(parent, "tools/call", nil)
	if result != nil || err != context.DeadlineExceeded {
		t.Fatalf("inherited deadline result=%#v err=%v", result, err)
	}
}

func TestMCPToolCallDeadlineReleasesPrincipalConcurrencySlot(t *testing.T) {
	const timeout = 25 * time.Millisecond
	limiter, err := auth.NewPrincipalConcurrencyLimiter(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	methodHandler := mcpToolCallDeadlineMiddleware(timeout)(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, err := methodHandler(r.Context(), "tools/call", nil)
		if err != nil {
			http.Error(w, "protocol error", http.StatusInternalServerError)
			return
		}
		if toolResult, ok := result.(*mcp.CallToolResult); ok && toolResult.IsError {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(app)
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		return req.WithContext(context.WithValue(req.Context(), auth.LearnerIDKey, "learner-1"))
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request())
		firstDone <- rec
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first call did not enter handler")
	}

	concurrent := httptest.NewRecorder()
	handler.ServeHTTP(concurrent, request())
	if concurrent.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent status=%d, want 429", concurrent.Code)
	}
	select {
	case first := <-firstDone:
		if first.Code != http.StatusGatewayTimeout {
			t.Fatalf("timed-out status=%d, want 504", first.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out call did not return")
	}

	after := httptest.NewRecorder()
	handler.ServeHTTP(after, request())
	if after.Code == http.StatusTooManyRequests {
		t.Fatal("concurrency slot remained occupied after deadline")
	}
}

func TestStatusRecorderKeepsFirstStatusAndUnwraps(t *testing.T) {
	base := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: base, status: http.StatusOK}

	rec.WriteHeader(http.StatusAccepted)
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.status != http.StatusAccepted {
		t.Fatalf("recorded status = %d, want first status 202", rec.status)
	}
	if rec.Unwrap() != base {
		t.Fatal("Unwrap did not return underlying writer")
	}
}

func TestMCPRequestBodyLimitRejectsDeclaredContentLength(t *testing.T) {
	const maxBytes = int64(16)
	called := false
	handler := mcpRequestBodyLimitMiddleware(maxBytes, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", int(maxBytes)+1)))
	if req.ContentLength <= maxBytes {
		t.Fatalf("test request Content-Length=%d, want greater than %d", req.ContentLength, maxBytes)
	}
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d; body=%q", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
	if called {
		t.Fatal("oversized request reached the downstream handler")
	}
}

func TestMCPRequestBodyLimitRejectsChunkedBody(t *testing.T) {
	const maxBytes = int64(16)
	called := make(chan struct{}, 1)
	handler := mcpRequestBodyLimitMiddleware(maxBytes, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called <- struct{}{}
	}))
	sawChunked := make(chan bool, 1)
	serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawChunked <- len(r.TransferEncoding) == 1 && r.TransferEncoding[0] == "chunked"
		handler.ServeHTTP(w, r)
	})
	clientConn, serverConn := net.Pipe()
	listener := newSingleConnListener(serverConn)
	server := &http.Server{Handler: serverHandler}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	})

	// Wrapping the reader hides its size from net/http, forcing HTTP/1.1
	// chunked transfer instead of a Content-Length header.
	payload := strings.Repeat("x", int(maxBytes)+1)
	req, err := http.NewRequest(http.MethodPost, "http://tutor.example/mcp", io.NopCloser(strings.NewReader(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != 0 {
		t.Fatalf("test request ContentLength=%d, want 0 (unknown)", req.ContentLength)
	}
	req.Close = true
	if err := req.Write(clientConn); err != nil {
		t.Fatalf("write chunked request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read chunked response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, want %d; body=%q", resp.StatusCode, http.StatusRequestEntityTooLarge, body)
	}
	if !<-sawChunked {
		t.Fatal("request did not reach the server with Transfer-Encoding: chunked")
	}
	select {
	case <-called:
		t.Fatal("oversized chunked request reached the downstream handler")
	default:
	}
}

func TestMCPRequestBodyLimitRestoresAcceptedBody(t *testing.T) {
	const body = "exactly-16-bytes"
	called := false
	handler := mcpRequestBodyLimitMiddleware(int64(len(body)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("restored body=%q, want %q", got, body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("restored ContentLength=%d, want %d", r.ContentLength, len(body))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))

	handler.ServeHTTP(recorder, req)

	if !called {
		t.Fatal("accepted request did not reach downstream handler")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusNoContent)
	}
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func newIPv4HTTPTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on IPv4 loopback: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func callToolText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// This is the release-level contract behind the product promise: an ordinary
// MCP client can initialize over Streamable HTTP, retrieve the tutor prompt,
// discover the tools, and complete a state-changing pedagogical loop through
// the same authenticated handler used in production.
func TestStreamableHTTPClientCompletesTutoringLoop(t *testing.T) {
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatalf("load JWT secret: %v", err)
	}

	database, err := db.OpenDB(filepath.Join(t.TempDir(), "tutor.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	store := db.NewStore(database)
	var serverLogs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&serverLogs, nil))
	const issuer = "https://tutor.example"
	learner, err := store.CreateLearner(
		context.Background(),
		"black-box@example.test",
		"not-used-by-this-token-test",
		"learn fractions",
		"",
	)
	if err != nil {
		t.Fatalf("create learner: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tutor-mcp",
		Version: "test",
	}, nil)
	tutortools.RegisterTools(server, &tutortools.Deps{
		Store:   store,
		Logger:  logger,
		BaseURL: issuer,
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	protected := auth.BearerMiddleware(issuer, withoutWriteTimeout(mcpHandler))
	httpServer := newIPv4HTTPTestServer(t, protected)
	t.Cleanup(httpServer.Close)

	token, err := auth.GenerateJWT(issuer, learner.ID)
	if err != nil {
		t.Fatalf("generate JWT: %v", err)
	}
	httpClient := &http.Client{Transport: bearerRoundTripper{
		base:  http.DefaultTransport,
		token: token,
	}}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "generic-mcp-client",
		Version: "test",
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   httpServer.URL + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated MCP protocol = %q, want 2026-07-28", got)
	}
	t.Cleanup(func() { _ = session.Close() })

	prompt, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "tutor_mcp"})
	if err != nil {
		t.Fatalf("get tutor prompt: %v", err)
	}
	if len(prompt.Messages) == 0 {
		t.Fatal("tutor_mcp prompt returned no instructions")
	}

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	required := map[string]bool{
		"init_domain":        false,
		"get_next_activity":  false,
		"record_interaction": false,
	}
	for _, tool := range listed.Tools {
		if _, ok := required[tool.Name]; ok {
			required[tool.Name] = true
		}
	}
	for name, found := range required {
		if !found {
			t.Fatalf("required tutoring tool %q not advertised", name)
		}
	}

	initResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "init_domain",
		Arguments: map[string]any{
			"name":          "Mathematics",
			"concepts":      []string{"fractions"},
			"prerequisites": map[string][]string{},
		},
	})
	if err != nil || initResult.IsError {
		t.Fatalf("init_domain: result=%q err=%v logs=%q", callToolText(initResult), err, serverLogs.String())
	}
	activity, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_next_activity",
		Arguments: map[string]any{},
	})
	if err != nil || activity.IsError {
		t.Fatalf("get_next_activity: result=%q err=%v", callToolText(activity), err)
	}
	recorded, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "record_interaction",
		Arguments: map[string]any{
			"concept":               "fractions",
			"activity_type":         "RECALL_EXERCISE",
			"success":               true,
			"response_time_seconds": 4.0,
			"confidence":            0.85,
			"notes":                 "",
		},
	})
	if err != nil || recorded.IsError {
		t.Fatalf("record_interaction: result=%q err=%v", callToolText(recorded), err)
	}

	domain, err := store.GetDomainByLearner(ctx, learner.ID)
	if err != nil {
		t.Fatalf("read persisted domain: %v", err)
	}
	state, err := store.GetConceptStateInDomain(ctx, learner.ID, domain.ID, "fractions")
	if err != nil {
		t.Fatalf("read persisted concept state: %v", err)
	}
	if state.Reps != 1 || state.PMastery <= 0.1 {
		t.Fatalf("pedagogical evidence was not applied: reps=%d mastery=%v", state.Reps, state.PMastery)
	}
}

func TestStatelessMCPLegacyProtocolWorksAcrossRoundRobinNodes(t *testing.T) {
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	const issuer = "https://tutor.example"
	server := mcp.NewServer(&mcp.Implementation{Name: "round-robin", Version: "test"}, nil)
	newNode := func() *httptest.Server {
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
			Stateless: true, DisableLocalhostProtection: true,
		})
		return newIPv4HTTPTestServer(t, auth.BearerMiddleware(issuer, handler))
	}
	nodeA, nodeB := newNode(), newNode()
	t.Cleanup(nodeA.Close)
	t.Cleanup(nodeB.Close)
	token, err := auth.GenerateJWT(issuer, "round-robin-learner")
	if err != nil {
		t.Fatal(err)
	}

	postRPC := func(endpoint, protocol, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, endpoint+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if protocol != "" {
			req.Header.Set("MCP-Protocol-Version", protocol)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	const legacy = "2025-03-26"
	initialize := postRPC(nodeA.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"legacy-client","version":"1"}}}`)
	initializeBody, _ := io.ReadAll(initialize.Body)
	_ = initialize.Body.Close()
	if initialize.StatusCode != http.StatusOK || !bytes.Contains(initializeBody, []byte(`"protocolVersion":"`+legacy+`"`)) {
		t.Fatalf("legacy initialize status=%d body=%s", initialize.StatusCode, initializeBody)
	}
	if sessionID := initialize.Header.Get("Mcp-Session-Id"); sessionID != "" {
		t.Fatalf("stateless node issued session ID %q", sessionID)
	}

	// The next request intentionally hits another node and carries no session
	// identifier. A stateful regression would fail here or demand affinity.
	list := postRPC(nodeB.URL, legacy, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listBody, _ := io.ReadAll(list.Body)
	_ = list.Body.Close()
	if list.StatusCode != http.StatusOK || !bytes.Contains(listBody, []byte(`"tools"`)) {
		t.Fatalf("round-robin tools/list status=%d body=%s", list.StatusCode, listBody)
	}
}
