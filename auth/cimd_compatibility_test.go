package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCIMDRedirectBudgetAndAuthMethodIntersection(t *testing.T) {
	s, _ := newTestServer(t)
	const clientID = "https://chatgpt.example/oauth/client-metadata.json"
	for _, tc := range []struct {
		name     string
		count    int
		methods  any
		singular string
		want     bool
	}{
		{"legacy Claude", 1, nil, "none", true},
		{"ChatGPT plural overrides preference", 32, []string{"none", "private_key_jwt"}, "private_key_jwt", true},
		{"plural only", 6, []string{"none"}, "", true},
		{"too many", 33, []string{"none"}, "none", false},
		{"no intersection", 1, []string{"private_key_jwt"}, "none", false},
		{"empty intersection", 1, []string{}, "none", false},
		{"explicit null", 1, json.RawMessage("null"), "none", false},
		{"invalid plural type", 1, "none", "none", false},
		{"legacy unsupported", 1, nil, "private_key_jwt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uris := make([]string, tc.count)
			for i := range uris {
				uris[i] = fmt.Sprintf("https://client.example/callback/%d", i)
			}
			doc := map[string]any{"client_id": clientID, "client_name": tc.name, "redirect_uris": uris, "token_endpoint_auth_method": tc.singular}
			if tc.methods != nil {
				doc["token_endpoint_auth_methods_supported"] = tc.methods
			}
			body, _ := json.Marshal(doc)
			s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			_, _, err := s.fetchCIMDDocument(context.Background(), clientID)
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v err=%v", err == nil, err)
			}
			if tc.count > 5 && validateRegistrationRedirectURIs(uris) == nil {
				t.Fatal("DCR inherited CIMD budget")
			}
		})
	}
}
