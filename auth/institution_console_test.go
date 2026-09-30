// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
)

// institutionTestServer serves the institution routes over TLS so the Secure
// cookies travel as in a browser.
func institutionTestServer(t *testing.T, signupOpen bool) (*OAuthServer, *db.Store, *httptest.Server) {
	t.Helper()
	setTestSecret(t)
	s, store := newTestServer(t)
	keyring, err := db.NewIntegrationSecretKeyring("k1:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "k1")
	if err != nil {
		t.Fatal(err)
	}
	store.SetIntegrationSecretKeyring(keyring)
	options := InstitutionAccountOptions{SignupOpen: signupOpen}
	if signupOpen {
		options.SignupPlan = "plan_legacy"
	}
	if err := s.EnableInstitutionAccounts(options); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /learn", s.HandleLearnerHome)
	mux.HandleFunc("GET /learn/formations/{cohortID}", s.HandleLearnerFormation)
	mux.HandleFunc("GET /learn/login", s.HandleLearnerLoginGet)
	mux.HandleFunc("POST /learn/login", s.HandleLearnerLoginPost)
	mux.HandleFunc("POST /learn/logout", s.HandleLearnerLogoutPost)
	mux.HandleFunc("POST /learn/enrollment", s.HandleLearnerEnrollmentPost)
	mux.HandleFunc("GET /learn/mfa", s.HandleLearnerMFA)
	mux.HandleFunc("POST /learn/mfa", s.HandleLearnerMFA)
	mux.HandleFunc("GET /learn/mfa/setup", s.HandleLearnerMFA)
	mux.HandleFunc("POST /learn/mfa/setup", s.HandleLearnerMFA)
	mux.HandleFunc("GET /console/admissions", s.HandleConsoleAdmissions)
	mux.HandleFunc("POST /console/admissions", s.HandleConsoleAdmissions)
	mux.HandleFunc("GET /authorize", s.HandleAuthorizeGet)
	mux.HandleFunc("POST /authorize", s.HandleAuthorizePost)
	mux.HandleFunc("POST /token", s.HandleToken)
	mux.HandleFunc("GET /console/api-csrf", s.HandleConsoleAPICSRF)
	mux.Handle("/console/test-api", s.ConsoleAPIHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := GetPrincipal(r.Context()); !ok {
			t.Error("console API has no principal")
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.HandleFunc("GET /console", s.HandleConsoleHome)
	mux.HandleFunc("GET /console/login", s.HandleConsoleLoginGet)
	mux.HandleFunc("POST /console/login", s.HandleConsoleLoginPost)
	mux.HandleFunc("POST /console/logout", s.HandleConsoleLogoutPost)
	mux.HandleFunc("GET /console/mfa", s.HandleConsoleMFAGet)
	mux.HandleFunc("POST /console/mfa", s.HandleConsoleMFAPost)
	mux.HandleFunc("GET /console/mfa/setup", s.HandleConsoleMFASetupGet)
	mux.HandleFunc("POST /console/mfa/setup", s.HandleConsoleMFASetupPost)
	mux.HandleFunc("POST /console/members/invite", s.HandleConsoleInvitePost)
	mux.HandleFunc("POST /console/members/update", s.HandleConsoleMemberUpdatePost)
	mux.HandleFunc("POST /console/invitations/revoke", s.HandleConsoleInvitationRevokePost)
	mux.HandleFunc("GET /invite", s.HandleInvitationGet)
	mux.HandleFunc("POST /invite", s.HandleInvitationPost)
	mux.HandleFunc("GET /signup", s.HandleSignupGet)
	mux.HandleFunc("POST /signup", s.HandleSignupPost)
	mux.HandleFunc("GET /signup/complete", s.HandleSignupCompleteGet)
	mux.HandleFunc("POST /signup/complete", s.HandleSignupCompletePost)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return s, store, server
}

type testBrowser struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

func newTestBrowser(t *testing.T, server *httptest.Server) *testBrowser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := server.Client()
	client := &http.Client{Transport: shared.Transport, Jar: jar}
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host != strings.TrimPrefix(server.URL, "https://") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &testBrowser{t: t, server: server, client: client}
}

// local maps a link built on the public base URL to the test server.
func (b *testBrowser) local(link string) string {
	return strings.Replace(link, "https://test.example", b.server.URL, 1)
}

func (b *testBrowser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *testBrowser) get(path string) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.local(b.server.URL+path), nil)
	if strings.HasPrefix(path, "https://") {
		req, _ = http.NewRequest(http.MethodGet, b.local(path), nil)
	}
	return b.do(req)
}

func (b *testBrowser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

var (
	csrfPattern       = regexp.MustCompile(`name="csrf_token"\s+value="([^"]+)"`)
	tokenFieldPattern = regexp.MustCompile(`name="token" value="([^"]+)"`)
	secretPattern     = regexp.MustCompile(`<span class="mono">([A-Z2-7]{32})</span>`)
	credentialPattern = regexp.MustCompile(`name="credential_id" value="([^"]+)"`)
	recoveryPattern   = regexp.MustCompile(`<li>([a-z2-7]{5}-[a-z2-7]{5})</li>`)
)

func field(t *testing.T, pattern *regexp.Regexp, body string) string {
	t.Helper()
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("pattern %s not found in page: %.600s", pattern, body)
	}
	if len(match) > 1 {
		return match[1]
	}
	return match[0]
}

func testTOTP(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}

// enrollTOTP completes the setup page the browser was redirected to and
// returns the seed.
func enrollTOTP(t *testing.T, b *testBrowser, setupPage string) (string, []string) {
	t.Helper()
	secret := field(t, secretPattern, setupPage)
	resp, body := b.post("/console/mfa/setup", url.Values{
		"csrf_token":    {field(t, csrfPattern, setupPage)},
		"credential_id": {field(t, credentialPattern, setupPage)},
		"code":          {testTOTP(t, secret, time.Now().Add(-30*time.Second))},
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Save your recovery codes") {
		t.Fatalf("MFA setup = %d %.400s", resp.StatusCode, body)
	}
	var codes []string
	for _, match := range recoveryPattern.FindAllStringSubmatch(body, -1) {
		codes = append(codes, match[1])
	}
	return secret, codes
}

func TestInstitutionSignupConsoleInvitationsAndAuthorize(t *testing.T) {
	s, store, server := institutionTestServer(t, true)
	sender := s.emailSender.(*testEmailSender)
	owner := newTestBrowser(t, server)

	// Signup: nothing exists before the mailbox is verified.
	_, page := owner.get("/signup")
	resp, _ := owner.post("/signup", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "tenant_name": {"Acme Academy"},
		"tenant_slug": {"acme"}, "email": {"Owner@Acme.test"}, "accept_terms": {"yes"},
	})
	if resp.StatusCode != http.StatusAccepted || len(sender.signupLinks) != 1 || sender.signupTo[0] != "owner@acme.test" {
		t.Fatalf("signup = %d, links=%v", resp.StatusCode, sender.signupLinks)
	}
	if available, _ := store.TenantSlugAvailable(t.Context(), "acme"); !available {
		t.Fatal("tenant created before email verification")
	}
	_, page = owner.get(sender.signupLinks[0])
	resp, page = owner.post("/signup/complete", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "token": {field(t, tokenFieldPattern, page)},
		"password": {"owner-password-2026"}, "password_confirm": {"owner-password-2026"},
	})
	if resp.Request.URL.Path != "/console/mfa/setup" {
		t.Fatalf("signup completion landed on %s: %.300s", resp.Request.URL.Path, page)
	}
	// The console is closed until the second factor is enrolled.
	resp, page = owner.get("/console")
	if resp.Request.URL.Path != "/console/mfa/setup" {
		t.Fatalf("console before MFA landed on %s", resp.Request.URL.Path)
	}
	ownerSecret, codes := enrollTOTP(t, owner, page)
	if len(codes) != 10 {
		t.Fatalf("recovery codes = %v", codes)
	}

	// Invite a pedagogy manager who also learns, and a learner.
	_, page = owner.get("/console")
	if !strings.Contains(page, "owner@acme.test (you)") {
		t.Fatalf("members page: %.500s", page)
	}
	_, page = owner.post("/console/members/invite", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "emails": {"manager@acme.test"},
		"roles": {"pedagogy_manager", "learner"},
	})
	if strings.Contains(page, "/invite?token=") || len(sender.invitationLinks) != 1 {
		t.Fatalf("invitation link exposed to the inviter or not emailed: %v", sender.invitationTo)
	}
	managerLink := sender.invitationLinks[0]
	_, page = owner.post("/console/members/invite", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "emails": {"alice@acme.test, not-an-email"},
		"roles": {"learner"},
	})
	if !strings.Contains(page, "invalid email address") {
		t.Fatalf("invalid invitation list accepted: %.400s", page)
	}
	if resp, _ := owner.post("/console/members/invite", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "emails": {"alice@acme.test"}, "roles": {"learner"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("invite learner = %d", resp.StatusCode)
	}
	aliceLink := sender.invitationLinks[1]
	if len(sender.invitationLinks) != 2 {
		t.Fatalf("invitation emails = %v", sender.invitationTo)
	}

	// The learner joins and is sent to her AI client, not the console.
	alice := newTestBrowser(t, server)
	_, page = alice.get(aliceLink)
	resp, page = alice.post("/invite", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "token": {field(t, tokenFieldPattern, page)},
		"password": {"alice-password-2026"}, "password_confirm": {"alice-password-2026"},
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "https://test.example/mcp") {
		t.Fatalf("learner acceptance = %d %.400s", resp.StatusCode, page)
	}
	if resp, _ := alice.get("/console"); resp.Request.URL.Path != "/console/login" {
		t.Fatal("a learner reached the console")
	}

	// The manager joins, must enroll MFA, then signs in to an AI client with a code.
	manager := newTestBrowser(t, server)
	_, page = manager.get(managerLink)
	resp, page = manager.post("/invite", url.Values{
		"csrf_token": {field(t, csrfPattern, page)}, "token": {field(t, tokenFieldPattern, page)},
		"password": {"manager-password-2026"}, "password_confirm": {"manager-password-2026"},
	})
	if resp.Request.URL.Path != "/console/mfa/setup" {
		t.Fatalf("manager acceptance landed on %s", resp.Request.URL.Path)
	}
	managerSecret, _ := enrollTOTP(t, manager, page)
	_, page = manager.get("/console")
	if !strings.Contains(page, "does not manage members") {
		t.Fatalf("pedagogy manager member view: %.400s", page)
	}

	const clientID, redirectURI = "cid-console", "https://client.example/callback"
	seedClient(t, store, clientID, redirectURI)
	authorize := func(b *testBrowser, email, password, code string) (*http.Response, string) {
		t.Helper()
		verifier := "console-verifier-console-verifier-000000"
		sum := sha256.Sum256([]byte(verifier))
		query := url.Values{
			"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
			"resource": {testOAuthResource}, "state": {"st"}, "scope": {"learner"},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		}
		_, page := b.get("/authorize?" + query.Encode())
		form := url.Values{"csrf_token": {field(t, csrfPattern, page)}, "mode": {"login"},
			"email": {email}, "password": {password}, "approve_client": {"yes"}, "totp_code": {code}}
		for key, values := range query {
			form[key] = values
		}
		return b.post("/authorize", form)
	}
	if strings.Contains(page, `name="mode" value="register"`) {
		t.Fatal("register form offered")
	}
	resp, page = authorize(manager, "manager@acme.test", "manager-password-2026", "")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "authenticator app") {
		t.Fatalf("manager without code = %d %.300s", resp.StatusCode, page)
	}
	resp, _ = authorize(manager, "manager@acme.test", "manager-password-2026", testTOTP(t, managerSecret, time.Now().Add(30*time.Second)))
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), redirectURI) {
		t.Fatalf("manager with code = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = authorize(alice, "alice@acme.test", "alice-password-2026", "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("learner authorize = %d", resp.StatusCode)
	}
	resp, page = authorize(newTestBrowser(t, server), "mallory@acme.test", "whatever-password", "")
	if resp.StatusCode == http.StatusFound {
		t.Fatal("unknown account authorized")
	}
	registration := url.Values{"csrf_token": {field(t, csrfPattern, page)}, "mode": {"register"}, "email": {"new@acme.test"},
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"resource": {testOAuthResource}, "scope": {"learner"}}
	if _, page := newTestBrowser(t, server).post("/authorize", registration); strings.Contains(page, "Check your email") {
		t.Fatal("self-registration still open")
	}

	// Sign out, then back in: password, then the second factor.
	_, page = owner.get("/console")
	owner.post("/console/logout", url.Values{"csrf_token": {field(t, csrfPattern, page)}})
	if resp, _ := owner.get("/console"); resp.Request.URL.Path != "/console/login" {
		t.Fatal("session survived logout")
	}
	_, page = owner.get("/console/login")
	resp, page = owner.post("/console/login", url.Values{"csrf_token": {field(t, csrfPattern, page)},
		"email": {"owner@acme.test"}, "password": {"owner-password-2026"}})
	if resp.Request.URL.Path != "/console/mfa" {
		t.Fatalf("owner login landed on %s: %.300s", resp.Request.URL.Path, page)
	}
	resp, page = owner.post("/console/mfa", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "code": {"000000"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d", resp.StatusCode)
	}
	resp, page = owner.post("/console/mfa", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "code": {codes[0]}})
	if resp.Request.URL.Path != "/console" || !strings.Contains(page, "manager@acme.test") {
		t.Fatalf("recovery code sign-in landed on %s", resp.Request.URL.Path)
	}
	_ = ownerSecret

	// Five wrong codes spend the manager's second-factor budget: even a correct
	// code is then refused at /authorize, whatever the password resets.
	for i := 0; i < 5; i++ {
		authorize(manager, "manager@acme.test", "manager-password-2026", "000000")
	}
	resp, _ = authorize(manager, "manager@acme.test", "manager-password-2026", testTOTP(t, managerSecret, time.Now().Add(30*time.Second)))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second factor after exhausted budget = %d", resp.StatusCode)
	}

	// The owner has no learner profile; password recovery still works.
	resetBrowser := newTestBrowser(t, server)
	recoverAndReset(t, s, sender, "owner@acme.test", "owner-password-2027")
	_, page = resetBrowser.get("/console/login")
	resp, _ = resetBrowser.post("/console/login", url.Values{"csrf_token": {field(t, csrfPattern, page)},
		"email": {"owner@acme.test"}, "password": {"owner-password-2027"}})
	if resp.Request.URL.Path != "/console/mfa" {
		t.Fatalf("login with the reset password landed on %s", resp.Request.URL.Path)
	}

	// Suspending the manager ends the manager's console session.
	_, page = owner.get("/console/login")
	_, page = owner.post("/console/login", url.Values{"csrf_token": {field(t, csrfPattern, page)},
		"email": {"owner@acme.test"}, "password": {"owner-password-2027"}})
	resp, page = owner.post("/console/mfa", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "code": {codes[1]}})
	if resp.Request.URL.Path != "/console" {
		t.Fatalf("owner sign-in after reset landed on %s", resp.Request.URL.Path)
	}
	membershipID := regexp.MustCompile(`manager@acme\.test</td>\s*<td><form[^>]*>\s*<input[^>]*>\s*<input type="hidden" name="membership_id" value="([^"]+)"`).FindStringSubmatch(page)
	if membershipID == nil {
		t.Fatalf("manager row not found: %.2000s", page)
	}
	_, page = owner.post("/console/members/update", url.Values{"csrf_token": {field(t, csrfPattern, page)},
		"membership_id": {membershipID[1]}, "status": {"suspended"}, "roles": {"pedagogy_manager", "learner"}})
	if !strings.Contains(page, "Member updated.") {
		t.Fatalf("suspend manager: %.400s", page)
	}
	if resp, body := manager.get("/console"); resp.Request.URL.Path != "/console/login" {
		t.Fatalf("suspended manager kept the console: %s %d %.300s", resp.Request.URL.Path, resp.StatusCode, body)
	}
}

func TestInstitutionSignupClosedInOperatorMode(t *testing.T) {
	_, _, server := institutionTestServer(t, false)
	browser := newTestBrowser(t, server)
	if resp, _ := browser.get("/signup"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("signup page in operator mode = %d", resp.StatusCode)
	}
	if resp, _ := browser.get("/signup/complete?token=x"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("signup completion in operator mode = %d", resp.StatusCode)
	}
}

func TestInvitationLinkRejectsForgedToken(t *testing.T) {
	_, _, server := institutionTestServer(t, false)
	browser := newTestBrowser(t, server)
	resp, body := browser.get("/invite?token=forged")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid or expired") {
		t.Fatalf("forged invitation = %d %.200s", resp.StatusCode, body)
	}
}

func recoverAndReset(t *testing.T, s *OAuthServer, sender *testEmailSender, email, password string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /recover", s.HandleRecoverGet)
	mux.HandleFunc("POST /recover", s.HandleRecoverPost)
	mux.HandleFunc("GET /reset-password", s.HandleResetPasswordGet)
	mux.HandleFunc("POST /reset-password", s.HandleResetPasswordPost)
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	recovery := newTestBrowser(t, server)
	_, page := recovery.get("/recover")
	before := len(sender.resetLinks)
	resp, _ := recovery.post("/recover", url.Values{"csrf_token": {field(t, csrfPattern, page)}, "email": {email}})
	if resp.StatusCode != http.StatusAccepted || len(sender.resetLinks) != before+1 {
		t.Fatalf("recover %s = %d, links=%d", email, resp.StatusCode, len(sender.resetLinks))
	}
	link := sender.resetLinks[before]
	_, page = recovery.get(link)
	resp, page = recovery.post("/reset-password", url.Values{"csrf_token": {field(t, csrfPattern, page)},
		"token": {field(t, tokenFieldPattern, page)}, "password": {password}, "password_confirm": {password}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Password updated") {
		t.Fatalf("reset password = %d %.300s", resp.StatusCode, page)
	}
	if resp, _ := recovery.get(link); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset link reused = %d", resp.StatusCode)
	}
}
