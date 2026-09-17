package auth

import (
	"context"
	"errors"
	"net/http"

	"tutor-mcp/models"
)

type hobbyAccountStore interface {
	GetUserByLoginName(context.Context, string) (*models.User, error)
	HobbyLinkValid(context.Context, string, string) bool
	AcceptHobbyInvite(context.Context, string, string, string) (string, error)
	ResetHobbyPassword(context.Context, string, string) error
}

func (s *OAuthServer) EnableHobbyAccounts() error {
	if _, ok := s.store.(hobbyAccountStore); !ok {
		return errors.New("store does not support hobby accounts")
	}
	s.hobbyAccounts = true
	return nil
}

func (s *OAuthServer) loginUser(ctx context.Context, identifier string) (*models.User, error) {
	if s.hobbyAccounts {
		return s.store.(hobbyAccountStore).GetUserByLoginName(ctx, identifier)
	}
	return s.store.GetLocalUserByEmail(ctx, identifier)
}

func (s *OAuthServer) activeLoginUser(user *models.User) bool {
	if user == nil || user.Status != models.UserStatusActive {
		return false
	}
	if s.hobbyAccounts {
		return user.IdentityMode == "username"
	}
	return user.EmailVerifiedAt != nil && (user.IdentityMode == "email" || user.IdentityMode == "")
}

func (s *OAuthServer) HandleHobbyAccountGet(w http.ResponseWriter, r *http.Request) {
	if !s.hobbyAccounts {
		http.NotFound(w, r)
		return
	}
	purpose := "invite"
	if r.URL.Path == "/account/reset" {
		purpose = "reset"
	}
	raw := r.URL.Query().Get("token")
	if !s.store.(hobbyAccountStore).HobbyLinkValid(r.Context(), raw, purpose) {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Invalid link", Message: "This link is invalid or expired. Ask the server operator for a new link."})
		return
	}
	csrf, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setAccountCSRFCookie(w, csrf, r.URL.Path, 1800)
	renderAccountPage(w, http.StatusOK, accountPageData{Title: "Choose your credentials", Token: raw, CSRFToken: csrf, HobbyPurpose: purpose})
}

func (s *OAuthServer) HandleHobbyAccountPost(w http.ResponseWriter, r *http.Request) {
	if !s.hobbyAccounts {
		http.NotFound(w, r)
		return
	}
	if !parseLimitedForm(w, r, accountFormBodyLimitBytes) {
		return
	}
	if !s.validateAccountCSRF(r) {
		http.Error(w, "forbidden: csrf check failed", http.StatusForbidden)
		return
	}
	purpose := "invite"
	if r.URL.Path == "/account/reset" {
		purpose = "reset"
	}
	raw, password := r.FormValue("token"), r.FormValue("password")
	accounts := s.store.(hobbyAccountStore)
	invalid := func() {
		renderAccountPage(w, http.StatusBadRequest, accountPageData{Title: "Could not save credentials", Message: "Check your identifier, password and link. Reopen the link to try again."})
	}
	if len(password) < passwordMinLen || len(password) > passwordMaxLen || password != r.FormValue("password_confirm") || !accounts.HobbyLinkValid(r.Context(), raw, purpose) {
		invalid()
		return
	}
	name := r.FormValue("login_name")
	if purpose == "invite" {
		var err error
		name, err = models.NormalizeLoginName(name)
		if err != nil {
			invalid()
			return
		}
	}
	hash, err := s.bcrypt.Generate(r.Context(), []byte(password), bcryptCost)
	if errors.Is(err, ErrBcryptBusy) {
		renderBcryptBusyAccountPage(w)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if purpose == "invite" {
		_, err = accounts.AcceptHobbyInvite(r.Context(), raw, name, string(hash))
	} else {
		err = accounts.ResetHobbyPassword(r.Context(), raw, string(hash))
	}
	if err != nil {
		invalid()
		return
	}
	setAccountCSRFCookie(w, "", r.URL.Path, -1)
	renderAccountPage(w, http.StatusOK, accountPageData{Title: "Credentials saved", Message: "Return to your MCP client to sign in and authorize it. Account creation does not grant access to any client."})
}
