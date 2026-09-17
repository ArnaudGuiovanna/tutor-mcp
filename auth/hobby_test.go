package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"tutor-mcp/models"
)

func TestHobbyInvitationFormDoesNotCreateAccountOnOpen(t *testing.T) {
	s, store := newTestServer(t)
	ctx := context.Background()
	if err := store.EnsureInstallation(ctx, "hobby"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHobbyAccounts(); err != nil {
		t.Fatal(err)
	}
	s.emailSender = nil // no SMTP or email fallback exists in this profile
	raw, err := store.CreateHobbyLink(ctx, "invite", "")
	if err != nil {
		t.Fatal(err)
	}
	open := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.HandleHobbyAccountGet(rec, httptest.NewRequest("GET", "/account/invite?token="+raw, nil))
		if rec.Code != 200 {
			t.Fatalf("GET status=%d", rec.Code)
		}
		return rec.Result().Cookies()[0].Value
	}
	csrf := open()
	users, err := store.ListHobbyUsers(ctx)
	if err != nil || len(users) != 0 {
		t.Fatal("GET created an account")
	}
	invalid := postAccountForm(t, "/account/invite", csrf, url.Values{"token": {raw}}, s.HandleHobbyAccountPost)
	if invalid.Code != 400 || !store.HobbyLinkValid(ctx, raw, "invite") {
		t.Fatal("invalid form consumed invitation")
	}
	csrf = open()
	result := postAccountForm(t, "/account/invite", csrf, url.Values{"token": {raw}, "login_name": {"Alice"}, "password": {"a-long-password"}, "password_confirm": {"a-long-password"}}, s.HandleHobbyAccountPost)
	if result.Code != 200 {
		t.Fatalf("POST status=%d", result.Code)
	}
	user, err := store.GetUserByLoginName(ctx, "alice")
	if err != nil || user.EmailVerifiedAt != nil {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	var consents int
	if err := store.RawDB().QueryRow(`SELECT COUNT(*) FROM learner_approved_clients`).Scan(&consents); err != nil || consents != 0 {
		t.Fatal("account creation granted OAuth consent")
	}
	if store.HobbyLinkValid(ctx, raw, "invite") {
		t.Fatal("invitation remains valid")
	}
}

// These are protocol fixtures, not claims of a real Claude/ChatGPT/Hermes
// session. CIMD fetches stay inside the fixture transport.
func TestHobbyOAuthClientFixtures(t *testing.T) {
	for _, fixture := range []string{"Hermes DCR", "Hermes CIMD", "Claude CIMD", "ChatGPT CIMD"} {
		t.Run(fixture, func(t *testing.T) {
			setTestSecret(t)
			s, store := newTestServer(t)
			ctx := context.Background()
			if err := store.EnsureInstallation(ctx, "hobby"); err != nil {
				t.Fatal(err)
			}
			if err := s.EnableHobbyAccounts(); err != nil {
				t.Fatal(err)
			}
			s.emailSender = nil
			password := "correct-password-123"
			hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
			raw, _ := store.CreateHobbyLink(ctx, "invite", "")
			id, err := store.AcceptHobbyInvite(ctx, raw, "alice", string(hash))
			if err != nil {
				t.Fatal(err)
			}
			redirect := "https://client.example/callback"
			clientID := ""
			if fixture == "Hermes DCR" {
				rec := httptest.NewRecorder()
				s.HandleRegister(rec, httptest.NewRequest("POST", "/register", strings.NewReader(`{"client_name":"Hermes fixture","redirect_uris":["`+redirect+`"]}`)))
				var doc map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &doc)
				if rec.Code != 201 {
					t.Fatalf("DCR=%d %s", rec.Code, rec.Body.String())
				}
				clientID = doc["client_id"].(string)
			} else if fixture == "Hermes CIMD" {
				// Snapshot of the public document named in #172, fetched 2026-09-17.
				// Its ten loopback callbacks exceeded the former DCR-based limit.
				clientID = "https://nousresearch.github.io/hermes-agent/docs/oauth/client-metadata.json"
				redirect = "http://127.0.0.1:27890/callback"
				data, err := os.ReadFile("testdata/hermes-client-metadata.json")
				if err != nil {
					t.Fatal(err)
				}
				s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.String() != clientID {
						t.Errorf("unexpected CIMD URL: %s", req.URL)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				})}
			} else {
				clientID = "https://client.example/oauth/metadata.json"
				s.cimdHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					doc := map[string]any{"client_id": clientID, "client_name": fixture, "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"}
					if fixture == "ChatGPT CIMD" {
						uris := []string{redirect}
						for i := 1; i < 32; i++ {
							uris = append(uris, fmt.Sprintf("https://client.example/callback/%d", i))
						}
						doc["redirect_uris"] = uris
						doc["token_endpoint_auth_method"] = "private_key_jwt"
						doc["token_endpoint_auth_methods_supported"] = []string{"none", "private_key_jwt"}
					}
					data, _ := json.Marshal(doc)
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				})}
			}
			verifier := strings.Repeat("v", 43)
			digest := sha256.Sum256([]byte(verifier))
			form := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "resource": {testOAuthResource}, "response_type": {"code"}, "scope": {"learner"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}, "state": {"original-state"}, "mode": {"login"}, "email": {"ALICE"}, "password": {password}, "approve_client": {"yes"}}
			login := func(approve bool) *httptest.ResponseRecorder {
				get := httptest.NewRecorder()
				s.HandleAuthorizeGet(get, httptest.NewRequest("GET", "/authorize?"+form.Encode(), nil))
				if get.Code != 200 || strings.Contains(get.Body.String(), `<input id="login-email"`) {
					t.Fatalf("login page %d does not use identifiers", get.Code)
				}
				csrf := get.Result().Cookies()[0].Value
				form.Set("csrf_token", csrf)
				if approve {
					form.Set("approve_client", "yes")
				} else {
					form.Del("approve_client")
				}
				req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
				rec := httptest.NewRecorder()
				s.HandleAuthorizePost(rec, req)
				return rec
			}
			for range 20 {
				s.loginFailures.RecordContext(ctx, "alice")
			}
			if rec := login(false); rec.Code == http.StatusFound {
				t.Fatal("consent omitted but authorization granted")
			}
			rec := login(true)
			if rec.Code != http.StatusFound {
				t.Fatalf("correct credential blocked without SMTP: %d", rec.Code)
			}
			location, _ := url.Parse(rec.Header().Get("Location"))
			code := location.Query().Get("code")
			if code == "" || location.Query().Get("state") != "original-state" {
				t.Fatal("missing bound authorization response")
			}
			exchange := func(values url.Values) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/token", strings.NewReader(values.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				rec := httptest.NewRecorder()
				s.HandleToken(rec, req)
				return rec
			}
			values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirect}, "resource": {testOAuthResource}, "code_verifier": {"wrong"}}
			if rec := exchange(values); rec.Code == 200 {
				t.Fatal("invalid PKCE accepted")
			}
			values.Set("code_verifier", verifier)
			tokens := exchange(values)
			if tokens.Code != 200 {
				t.Fatalf("exchange=%d %s", tokens.Code, tokens.Body.String())
			}
			var issued map[string]any
			_ = json.Unmarshal(tokens.Body.Bytes(), &issued)
			if fixture == "Hermes DCR" {
				// The code exchange makes this DCR client permanent. Re-registering
				// equivalent metadata must reuse it, including after process restart.
				replay := httptest.NewRecorder()
				s.HandleRegister(replay, httptest.NewRequest("POST", "/register", strings.NewReader(`{"client_name":"Hermes fixture","redirect_uris":["`+redirect+`"]}`)))
				var registration map[string]any
				_ = json.Unmarshal(replay.Body.Bytes(), &registration)
				if replay.Code != 200 || registration["client_id"] != clientID || registration["client_id_expires_at"] != float64(0) {
					t.Fatal("activated DCR client could not be replayed")
				}
			}
			if rec := exchange(values); rec.Code == 200 {
				t.Fatal("code replay accepted")
			}
			refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {issued["refresh_token"].(string)}, "client_id": {clientID}, "resource": {testOAuthResource}}
			rotated := exchange(refresh)
			if rotated.Code != 200 {
				t.Fatalf("refresh=%d %s", rotated.Code, rotated.Body.String())
			}
			var next map[string]any
			_ = json.Unmarshal(rotated.Body.Bytes(), &next)
			if next["refresh_token"] == issued["refresh_token"] {
				t.Fatal("refresh did not rotate")
			}
			principal, err := store.GetPrincipalForLearner(ctx, id, []string{models.OAuthScopeLearner})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DisableHobbyUser(ctx, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := store.ValidatePrincipal(ctx, principal); err == nil {
				t.Fatal("disabled access survived")
			}
			refresh.Set("refresh_token", next["refresh_token"].(string))
			if rec := exchange(refresh); rec.Code == 200 {
				t.Fatal("disabled refresh survived")
			}
		})
	}
}
