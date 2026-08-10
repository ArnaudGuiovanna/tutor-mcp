// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"tutor-mcp/auth"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpWWWAuthenticateMetaKey = "mcp/www_authenticate"

// insufficientOAuthScopeResult is the MCP/ChatGPT-compatible fallback for
// transports that do not surface an HTTP authentication challenge to the
// client. It is also returned by the receiving middleware before replay-key
// reservation or tool execution.
func insufficientOAuthScopeResult(ctx context.Context, baseURL string, required []string, granular bool) *mcp.CallToolResult {
	requested := required
	if !granular {
		requested = []string{models.OAuthScopeLearner}
	}
	result, _ := errorResult("insufficient OAuth scope: " + strings.Join(requested, " "))
	result.Meta = mcp.Meta{
		mcpWWWAuthenticateMetaKey: []string{insufficientOAuthScopeChallenge(ctx, baseURL, required, granular)},
	}
	return result
}

func insufficientOAuthScopeChallenge(ctx context.Context, baseURL string, required []string, granular bool) string {
	challengeScopes := []string{models.OAuthScopeLearner}
	if granular {
		challengeScopes = oauthScopeUnion(auth.GetOAuthScope(ctx), required)
	}
	parts := []string{
		`Bearer error="insufficient_scope"`,
		`error_description="Additional OAuth scope required"`,
		"scope=" + strconv.Quote(strings.Join(challengeScopes, " ")),
	}
	if trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/"); trimmed != "" {
		parts = append(parts, "resource_metadata="+strconv.Quote(trimmed+"/.well-known/oauth-protected-resource"))
	}
	return strings.Join(parts, ", ")
}

// oauthScopeUnion returns a stable, least set for a step-up request. The held
// granular capabilities are retained so a client asking for the challenge's
// complete scope set cannot accidentally exchange one permission for another.
func oauthScopeUnion(granted string, required []string) []string {
	read, write := false, false
	switch granted {
	case models.OAuthScopeLearner, models.OAuthScopeLearnerReadWrite:
		read, write = true, true
	case models.OAuthScopeLearnerRead:
		read = true
	case models.OAuthScopeLearnerWrite:
		write = true
	}
	for _, scope := range required {
		switch scope {
		case models.OAuthScopeLearnerRead:
			read = true
		case models.OAuthScopeLearnerWrite:
			write = true
		}
	}
	union := make([]string, 0, 2)
	if read {
		union = append(union, models.OAuthScopeLearnerRead)
	}
	if write {
		union = append(union, models.OAuthScopeLearnerWrite)
	}
	return union
}

type oauthScopeHTTPCall struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Meta map[string]any `json:"_meta,omitempty"`
		Name string         `json:"name"`
	} `json:"params"`
}

type oauthScopeHTTPResponse struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      json.RawMessage     `json:"id"`
	Result  *mcp.CallToolResult `json:"result"`
}

type boundedMCPRequestBodyKey struct{}

// WithBoundedMCPRequestBody records bytes already validated by the outer MCP
// body-limit middleware. OAuthScopeHTTPMiddleware can inspect them without a
// second read/copy while leaving r.Body intact for the SDK.
func WithBoundedMCPRequestBody(r *http.Request, body []byte) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), boundedMCPRequestBodyKey{}, body))
}

// OAuthScopeHTTPMiddleware provides the standards-level 403 challenge for a
// single JSON-RPC tools/call request. It deliberately does not preempt batches
// or malformed requests; the SDK remains responsible for protocol errors and
// the receiving middleware remains the authorization fallback. The body is
// independently bounded so a future wiring change cannot turn this preflight
// into an unbounded read.
func OAuthScopeHTTPMiddleware(baseURL string, maxBodyBytes int64, granular bool, next http.Handler) http.Handler {
	if maxBodyBytes <= 0 {
		panic("OAuth scope preflight body limit must be positive")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, alreadyBounded := r.Context().Value(boundedMCPRequestBodyKey{}).([]byte)
		if !alreadyBounded {
			if r.ContentLength > maxBodyBytes {
				_ = r.Body.Close()
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			var err error
			body, err = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		if int64(len(body)) > maxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		var call oauthScopeHTTPCall
		if err := json.Unmarshal(body, &call); err != nil ||
			call.JSONRPC != "2.0" || call.Method != "tools/call" || call.Params.Name == "" ||
			!validOAuthPreflightRequestID(call.ID) || auth.GetLearnerID(r.Context()) == "" ||
			!validOAuthPreflightTransportHeaders(r, &call) {
			next.ServeHTTP(w, r)
			return
		}

		required, known := requiredOAuthScopesForTool(call.Params.Name)
		if !known || !oauthToolAvailable(call.Params.Name) {
			next.ServeHTTP(w, r)
			return
		}
		if hasRequiredOAuthScopes(r.Context(), required) {
			next.ServeHTTP(w, r)
			return
		}

		result := insufficientOAuthScopeResult(r.Context(), baseURL, required, granular)
		challenge := insufficientOAuthScopeChallenge(r.Context(), baseURL, required, granular)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(oauthScopeHTTPResponse{
			JSONRPC: "2.0",
			ID:      call.ID,
			Result:  result,
		})
	})
}

func validOAuthPreflightRequestID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var id any
	if err := decoder.Decode(&id); err != nil {
		return false
	}
	switch id.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func validOAuthPreflightTransportHeaders(r *http.Request, call *oauthScopeHTTPCall) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return false
	}
	jsonAccepted, streamAccepted := false, false
	for _, value := range r.Header.Values("Accept") {
		for _, raw := range strings.Split(value, ",") {
			base, _, _ := strings.Cut(strings.TrimSpace(raw), ";")
			switch strings.ToLower(strings.TrimSpace(base)) {
			case "application/json", "application/*":
				jsonAccepted = true
			case "text/event-stream", "text/*":
				streamAccepted = true
			case "*/*":
				jsonAccepted, streamAccepted = true, true
			}
		}
	}
	if !jsonAccepted || !streamAccepted {
		return false
	}

	protocolVersion := r.Header.Get("Mcp-Protocol-Version")
	if protocolVersion != "" && protocolVersion < "2026-07-28" {
		supported := false
		for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
			if protocolVersion == version {
				supported = true
				break
			}
		}
		if !supported {
			return false
		}
	}
	if protocolVersion >= "2026-07-28" {
		metaVersion, _ := call.Params.Meta[mcp.MetaKeyProtocolVersion].(string)
		return r.Header.Get("Mcp-Method") == call.Method &&
			r.Header.Get("Mcp-Name") == call.Params.Name &&
			metaVersion != "" && metaVersion == protocolVersion
	}
	return true
}
