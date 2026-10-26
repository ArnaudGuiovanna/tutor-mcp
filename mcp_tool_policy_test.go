package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/models"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tutorPolicyTransport struct {
	base   http.RoundTripper
	policy string
}

func (transport tutorPolicyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set(tools.TutorToolPolicyHeader, transport.policy)
	return transport.base.RoundTrip(copy)
}

// Exercise the production HTTP assembly and SDK, not just the policy's pure
// decision helper. A persisted provider approval cannot bypass this layer.
func TestMCPHTTPToolPolicyRestrictsDiscoveryAndDirectInvocation(t *testing.T) {
	configureMCPHTTPTestJWT(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			fixture := newMCPHTTPFixture(t, backend)
			actor := fixture.seedTenant(t, "tool-policy-"+backend, 1)[0]
			server := startMCPHTTPFixture(t, fixture)
			token, err := auth.GenerateJWTForPrincipalAndScope(mcpHTTPTestIssuer, auth.MCPResource(mcpHTTPTestIssuer), "tool-policy-client", actor, models.OAuthScopeLearner)
			if err != nil {
				t.Fatal(err)
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(transport.CloseIdleConnections)
			httpClient := &http.Client{Transport: tutorPolicyTransport{
				base:   bearerRoundTripper{base: transport, token: token},
				policy: `{"default":"deny","tools":{"get_pending_alerts":"allow"}}`,
			}, Timeout: 30 * time.Second}
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "policy-regression", Version: "test"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: httpClient}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			listed, err := session.ListTools(ctx, nil)
			if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "get_pending_alerts" {
				t.Fatalf("HTTP policy did not restrict real discovery: tools=%+v err=%v", listed, err)
			}
			allowed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_pending_alerts", Arguments: map[string]any{}})
			if err != nil || allowed == nil || allowed.IsError {
				t.Fatalf("allowed read could not reach institution authorization: result=%+v err=%v", allowed, err)
			}
			for _, name := range []string{"init_domain", "delete_domain"} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if err != nil || result == nil || !result.IsError {
					t.Fatalf("denied direct invocation %q was not blocked: result=%+v err=%v", name, result, err)
				}
			}
		})
	}
}
