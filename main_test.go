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

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
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
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	protected := auth.BearerMiddleware(issuer, withoutWriteTimeout(mcpHandler))
	httpServer := httptest.NewServer(protected)
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
