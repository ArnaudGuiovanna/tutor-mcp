package main

import (
	"log/slog"
	"net/http"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Guards are owned by the caller: main shares these exact instances and their
// backend/lifecycle with the administrative routes.
type mcpHTTPGuards struct {
	IP, Learner *auth.RateLimiter
	Concurrency *auth.PrincipalConcurrencyLimiter
	// NativeOnly requires a current key-authenticated, revocable session.
	// It prevents historical password/OAuth grants from entering a key-only
	// institution even when their membership is otherwise still active.
	NativeOnly bool
}

func initialMCPOAuthScope(granular bool) string {
	if granular {
		return models.OAuthScopeLearnerRead
	}
	return models.OAuthScopeLearner
}

// newMCPHTTPHandler is the production HTTP assembly, including tool middleware
// registered by newTutorServer. It does not create or own rate-limit guards.
func newMCPHTTPHandler(deps *tools.Deps, maxBodyBytes int64, timeout time.Duration, guards mcpHTTPGuards) http.Handler {
	server := newTutorServer(deps, timeout)
	raw := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true, // public reverse proxy
		MaxRequestBodyBytes:          maxBodyBytes,
		PropagateRequestCancellation: true,
	})
	cop := http.NewCrossOriginProtection()
	cop.AddTrustedOrigin("https://claude.ai")
	// Cross-origin rejection precedes buffering; OAuth preflight and the SDK
	// consume the same bounded bytes only after both identity guards.
	handler := cop.Handler(mcpRequestBodyLimitMiddleware(maxBodyBytes,
		tools.OAuthScopeHTTPMiddleware(deps.BaseURL, maxBodyBytes, deps.OAuthGranularScopes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("Tutor-Conversation-ID")
			if len(id) > 128 {
				http.Error(w, "invalid conversation", 400)
				return
			}
			if id != "" {
				r = r.WithContext(storeport.WithConversation(r.Context(), id))
			}
			raw.ServeHTTP(w, r)
		}))))
	authenticated := auth.LearnerRateLimitMiddleware(guards.Learner, guards.Concurrency.Middleware(tools.ToolPolicyHTTPMiddleware(handler)))
	var protected http.Handler
	if guards.NativeOnly {
		validator, ok := deps.Store.(auth.NativePrincipalValidator)
		if !ok {
			panic("native institution store does not validate key sessions")
		}
		protected = auth.BearerMiddlewareWithNativePrincipalValidator(deps.BaseURL, initialMCPOAuthScope(deps.OAuthGranularScopes), validator, authenticated)
	} else {
		protected = auth.BearerMiddlewareWithPrincipalValidator(deps.BaseURL, initialMCPOAuthScope(deps.OAuthGranularScopes), deps.Store, authenticated)
	}
	protected = auth.RateLimitMiddleware(guards.IP, protected)
	// Ordinary endpoints retain the server WriteTimeout. Only this response's
	// deadline is cleared; the outer logging writer exposes Unwrap.
	return withoutWriteTimeout(protected)
}

func wrapHTTPHandler(logger *slog.Logger, baseURL string, next http.Handler) http.Handler {
	return observability.HTTPHandler(recoveryMiddleware(logger, requestLogger(logger, securityHeaders(baseURL,
		corsMiddleware([]string{"https://claude.ai", baseURL}, nil, next)))))
}
