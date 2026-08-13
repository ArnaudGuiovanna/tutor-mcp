package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestRegistrationPreHijackCannotActivateFromMailboxClick(t *testing.T) {
	setTestSecret(t)
	s, dbStore := newTestServer(t)
	const attackerRedirect = "https://attacker.example/callback"

	// The attacker first creates a public DCR client and controls both its
	// redirect and the PKCE verifier.
	registerReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
		`{"client_name":"Victim Support Portal","redirect_uris":["`+attackerRedirect+`"]}`,
	))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	s.HandleRegister(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("malicious DCR status=%d body=%q", registerRec.Code, registerRec.Body.String())
	}
	var registration map[string]any
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	clientID, _ := registration["client_id"].(string)
	if clientID == "" {
		t.Fatal("malicious DCR response omitted client_id")
	}
	verifier := strings.Repeat("v", 43)
	challengeDigest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])

	// The attacker starts registration with the victim's email and submits a
	// password/consent. Those fields must have no authority before mailbox proof.
	authorizeForm := url.Values{
		"csrf_token": {"attacker-csrf"}, "mode": {"register"},
		"client_id": {clientID}, "redirect_uri": {attackerRedirect},
		"response_type": {"code"}, "resource": {testOAuthResource},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"email": {"victim@example.com"}, "password": {"attacker-password-123"},
		"password_confirm": {"attacker-password-123"}, "approve_client": {"yes"},
	}
	authorizeReq := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(authorizeForm.Encode()))
	authorizeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorizeReq.AddCookie(&http.Cookie{Name: "csrf_token", Value: "attacker-csrf"})
	authorizeRec := httptest.NewRecorder()
	s.HandleAuthorizePost(authorizeRec, authorizeReq)
	if authorizeRec.Code != http.StatusAccepted {
		t.Fatalf("attacker registration status=%d body=%q", authorizeRec.Code, authorizeRec.Body.String())
	}

	learner, err := dbStore.GetLearnerByEmail(context.Background(), "victim@example.com")
	if err != nil || learner.EmailVerifiedAt != nil {
		t.Fatalf("pending victim identity=%+v err=%v", learner, err)
	}
	if bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("attacker-password-123")) == nil {
		t.Fatal("attacker-selected password is usable on the pending identity")
	}
	if approved, err := dbStore.IsClientApproved(context.Background(), learner.ID, clientID, attackerRedirect); err != nil || approved {
		t.Fatalf("attacker-selected consent persisted: approved=%v err=%v", approved, err)
	}

	sender := s.emailSender.(*testEmailSender)
	if len(sender.verificationLinks) != 1 || sender.verificationTo[0] != "victim@example.com" {
		t.Fatalf("verification deliveries=%v recipients=%v", sender.verificationLinks, sender.verificationTo)
	}
	verificationURL, err := url.Parse(sender.verificationLinks[0])
	if err != nil {
		t.Fatal(err)
	}
	rawToken := verificationURL.Query().Get("token")
	getVerification := func() (string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.HandleVerifyEmailGet(rec, httptest.NewRequest(http.MethodGet, verificationURL.String(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("verification GET status=%d body=%q", rec.Code, rec.Body.String())
		}
		var csrf string
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == accountCSRFCookieName {
				csrf = cookie.Value
			}
		}
		if csrf == "" {
			t.Fatal("verification GET omitted CSRF cookie")
		}
		return csrf, rec.Body.String()
	}

	// The old one-click confirmation shape is rejected. Merely opening the link
	// and posting its hidden fields cannot activate the account or mint a code.
	csrf, page := getVerification()
	if !strings.Contains(page, "Victim Support Portal") || !strings.Contains(page, clientID) ||
		!strings.Contains(page, "https://attacker.example") {
		t.Fatalf("verification page hid malicious client identity/destination: %q", page)
	}
	clickOnly := postAccountForm(t, "/verify-email", csrf, url.Values{"token": {rawToken}}, s.HandleVerifyEmailPost)
	if clickOnly.Code != http.StatusBadRequest {
		t.Fatalf("click-only activation status=%d, want 400", clickOnly.Code)
	}
	learner, _ = dbStore.GetLearnerByID(context.Background(), learner.ID)
	if learner.EmailVerifiedAt != nil {
		t.Fatal("click-only confirmation activated the victim identity")
	}
	var codeCount int
	if err := dbStore.RawDB().QueryRow(`SELECT COUNT(*) FROM oauth_codes WHERE learner_id = ?`, learner.ID).Scan(&codeCount); err != nil || codeCount != 0 {
		t.Fatalf("click-only confirmation created %d codes: %v", codeCount, err)
	}

	// A fresh owner-selected credential alone is also insufficient: consent for
	// the exact DCR client and destination is an independent required action.
	csrf, _ = getVerification()
	withoutConsent := postAccountForm(t, "/verify-email", csrf, url.Values{
		"token": {rawToken}, "password": {"owner-password-123"},
		"password_confirm": {"owner-password-123"},
	}, s.HandleVerifyEmailPost)
	if withoutConsent.Code != http.StatusBadRequest {
		t.Fatalf("activation without consent status=%d, want 400", withoutConsent.Code)
	}

	// Only the mailbox holder's fresh password plus explicit, displayed consent
	// can activate and resume the bound resource/redirect/PKCE request.
	csrf, _ = getVerification()
	activated := postAccountForm(t, "/verify-email", csrf, url.Values{
		"token": {rawToken}, "password": {"owner-password-123"},
		"password_confirm": {"owner-password-123"}, "approve_client": {"yes"},
	}, s.HandleVerifyEmailPost)
	if activated.Code != http.StatusFound {
		t.Fatalf("owner activation status=%d body=%q", activated.Code, activated.Body.String())
	}
	callback, err := url.Parse(activated.Header().Get("Location"))
	if err != nil || callback.Scheme+"://"+callback.Host != "https://attacker.example" || callback.Query().Get("code") == "" {
		t.Fatalf("activation callback=%q err=%v", activated.Header().Get("Location"), err)
	}
	code := callback.Query().Get("code")
	learner, err = dbStore.GetLearnerByID(context.Background(), learner.ID)
	if err != nil || learner.EmailVerifiedAt == nil ||
		bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("owner-password-123")) != nil ||
		bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("attacker-password-123")) == nil {
		t.Fatalf("owner credential did not exclusively activate account: learner=%+v err=%v", learner, err)
	}
	if approved, err := dbStore.IsClientApproved(context.Background(), learner.ID, clientID, attackerRedirect); err != nil || !approved {
		t.Fatalf("owner-approved client was not persisted: approved=%v err=%v", approved, err)
	}

	// The code remains bound to the original resource, redirect, and PKCE. This
	// succeeds only after the explicit activation above, proving the security
	// gate without weakening the continuation protocol.
	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID},
		"redirect_uri": {attackerRedirect}, "resource": {testOAuthResource},
		"code": {code}, "code_verifier": {verifier},
	}
	tokenReq := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(tokenForm.Encode()))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRec := httptest.NewRecorder()
	s.HandleToken(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK || !strings.Contains(tokenRec.Body.String(), `"access_token"`) {
		t.Fatalf("bound code exchange status=%d body=%q", tokenRec.Code, tokenRec.Body.String())
	}
}

func accountCSRF(t *testing.T, s *OAuthServer, path string, handler http.HandlerFunc) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%q", path, rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == accountCSRFCookieName {
			return cookie.Value
		}
	}
	t.Fatalf("GET %s did not set account CSRF cookie", path)
	return ""
}

func postAccountForm(t *testing.T, path, csrf string, form url.Values, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: accountCSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestRecoverResponseDoesNotEnumerateAccountsAndStoresOnlyTokenHash(t *testing.T) {
	s, dbStore := newTestServer(t)
	seedLearner(t, dbStore, "known@example.com", "old-password-123")
	sender := s.emailSender.(*testEmailSender)

	csrfKnown := accountCSRF(t, s, "/recover", s.HandleRecoverGet)
	known := postAccountForm(t, "/recover", csrfKnown, url.Values{"email": {"known@example.com"}}, s.HandleRecoverPost)
	if known.Code != http.StatusAccepted || len(sender.resetLinks) != 1 {
		t.Fatalf("known response status=%d deliveries=%d", known.Code, len(sender.resetLinks))
	}

	csrfUnknown := accountCSRF(t, s, "/recover", s.HandleRecoverGet)
	unknown := postAccountForm(t, "/recover", csrfUnknown, url.Values{"email": {"unknown@example.com"}}, s.HandleRecoverPost)
	if unknown.Code != known.Code || unknown.Body.String() != known.Body.String() {
		t.Fatalf("enumerating response: known=(%d,%q) unknown=(%d,%q)", known.Code, known.Body.String(), unknown.Code, unknown.Body.String())
	}
	if len(sender.resetLinks) != 1 {
		t.Fatalf("unknown account caused delivery; deliveries=%d", len(sender.resetLinks))
	}

	resetURL, err := url.Parse(sender.resetLinks[0])
	if err != nil {
		t.Fatalf("parse reset URL: %v", err)
	}
	raw := resetURL.Query().Get("token")
	var stored string
	if err := dbStore.RawDB().QueryRow(`SELECT token_hash FROM account_tokens WHERE purpose = 'password_reset'`).Scan(&stored); err != nil {
		t.Fatalf("read reset token: %v", err)
	}
	if raw == "" || stored == raw || stored != accountTokenHash(raw) {
		t.Fatalf("reset token storage raw=%q stored=%q", raw, stored)
	}
}

func TestResetPasswordHTTPIsSingleUse(t *testing.T) {
	s, dbStore := newTestServer(t)
	seedLearner(t, dbStore, "reset@example.com", "old-password-123")
	csrf := accountCSRF(t, s, "/recover", s.HandleRecoverGet)
	postAccountForm(t, "/recover", csrf, url.Values{"email": {"reset@example.com"}}, s.HandleRecoverPost)
	resetURL, _ := url.Parse(s.emailSender.(*testEmailSender).resetLinks[0])
	raw := resetURL.Query().Get("token")

	resetCSRF := accountCSRF(t, s, resetURL.RequestURI(), s.HandleResetPasswordGet)
	form := url.Values{
		"token": {raw}, "password": {"new-password-123"}, "password_confirm": {"new-password-123"},
	}
	result := postAccountForm(t, "/reset-password", resetCSRF, form, s.HandleResetPasswordPost)
	if result.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%q", result.Code, result.Body.String())
	}
	learner, err := dbStore.GetLearnerByEmail(context.Background(), "reset@example.com")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(learner.PasswordHash), []byte("new-password-123")) != nil {
		t.Fatalf("new password not active: learner=%+v err=%v", learner, err)
	}

	// A fresh CSRF value does not make the already-consumed capability valid.
	secondCSRF := "fresh-csrf"
	second := postAccountForm(t, "/reset-password", secondCSRF, form, s.HandleResetPasswordPost)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("reset replay status=%d, want 400", second.Code)
	}
}

func TestUnverifiedLoginIsUniformCredentialFailure(t *testing.T) {
	s, dbStore := newTestServer(t)
	seedClient(t, dbStore, "cid", "https://good.example/cb")
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	learner, err := dbStore.CreateUnverifiedLearner(context.Background(), "pending@example.com", string(hash), "", "")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"csrf_token": {"login-csrf"}, "mode": {"login"}, "client_id": {"cid"},
		"redirect_uri": {"https://good.example/cb"}, "response_type": {"code"},
		"resource": {testOAuthResource}, "code_challenge": {"challenge"},
		"code_challenge_method": {"S256"}, "email": {learner.Email},
		"password": {"correct-password"}, "approve_client": {"yes"},
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "login-csrf"})
	rec := httptest.NewRecorder()
	s.HandleAuthorizePost(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid email or password.") {
		t.Fatalf("pending login status=%d body=%q", rec.Code, rec.Body.String())
	}
	if deliveries := len(s.emailSender.(*testEmailSender).verificationLinks); deliveries != 0 {
		t.Fatalf("pending login leaked account state through %d verification deliveries", deliveries)
	}
	var codeCount int
	if err := dbStore.RawDB().QueryRow(`SELECT COUNT(*) FROM oauth_codes WHERE learner_id = ?`, learner.ID).Scan(&codeCount); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if codeCount != 0 {
		t.Fatalf("unverified learner received %d authorization codes", codeCount)
	}
	approved, err := dbStore.IsClientApproved(context.Background(), learner.ID, "cid", "https://good.example/cb")
	if err != nil || approved {
		t.Fatalf("unverified login persisted client consent: approved=%v err=%v", approved, err)
	}
}
