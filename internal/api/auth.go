package api

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/store"
)

// authConfigResponse tells the login page what it may offer.
type authConfigResponse struct {
	AllowLocal bool                `json:"allowLocal"`
	Providers  []auth.ProviderInfo `json:"providers"`
	// NeedsBootstrap is true when no account exists yet, so the UI can point
	// the operator at the generated admin credentials in the server log.
	NeedsBootstrap bool `json:"needsBootstrap"`
}

func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	count, err := s.db.CountUsers()
	if err != nil {
		s.log.Error("counting users failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not read the user database")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, authConfigResponse{
		AllowLocal:     s.cfg.Auth.AllowLocal,
		Providers:      s.auth.ProviderInfos(),
		NeedsBootstrap: count == 0,
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Code is the second factor. Empty on the first call: the client learns it
	// is needed from the 401 that comes back, then asks again with it.
	Code string `json:"code"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	u, err := s.auth.CompleteLogin(r.Context(), req.Email, req.Password, req.Code, s.auth.ClientIP(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLocalLoginDisabled):
			httpx.WriteError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, auth.ErrTwoFactorRequired):
			// A distinct body, so the login form knows to ask for a code
			// rather than telling the user their password was wrong.
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{
				"error":             err.Error(),
				"twoFactorRequired": true,
			})
		case errors.Is(err, auth.ErrInvalidTOTP), errors.Is(err, auth.ErrRecoveryCodeInvalid):
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{
				"error":             err.Error(),
				"twoFactorRequired": true,
			})
		case errors.Is(err, auth.ErrInvalidCredentials):
			httpx.WriteError(w, http.StatusUnauthorized, err.Error())
		default:
			httpx.WriteError(w, http.StatusTooManyRequests, err.Error())
		}
		return
	}
	if err := s.auth.IssueSession(w, r, u); err != nil {
		s.log.Error("issuing session failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(w, r); err != nil {
		s.log.Warn("logout failed", "error", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())

	var req changePasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if u.PasswordHash == "" {
		httpx.WriteError(w, http.StatusConflict, "this account signs in with SSO and has no password")
		return
	}

	ok, err := auth.VerifyPassword(u.PasswordHash, req.CurrentPassword)
	if err != nil || !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.db.SetPasswordHash(u.ID, hash); err != nil {
		s.log.Error("changing password failed", "user", u.ID, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	// Every other session was authorised with the old password.
	if err := s.db.DeleteUserSessions(u.ID); err != nil {
		s.log.Warn("clearing sessions after password change failed", "user", u.ID, "error", err)
	}
	if err := s.auth.IssueSession(w, r, u); err != nil {
		s.log.Error("re-issuing session failed", "user", u.ID, "error", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	redirectURL, err := s.auth.StartSSO(w, r, provider, r.URL.Query().Get("next"))
	if err != nil {
		s.log.Warn("starting SSO failed", "provider", provider, "error", err)
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	user, next, err := s.auth.CompleteSSO(w, r, provider)
	if err != nil {
		s.log.Warn("SSO callback failed", "provider", provider, "error", err)
		// Send the browser back to the login page with a readable reason
		// rather than dumping JSON into the address bar.
		http.Redirect(w, r, "/login?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	s.log.Info("SSO login", "provider", provider, "user", user.Email)
	http.Redirect(w, r, next, http.StatusFound)
}

// requireRole is a small helper for handlers that guard themselves rather than
// having a dedicated middleware chain.
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, min store.Role) (store.User, bool) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not authenticated")
		return store.User{}, false
	}
	if !u.Role.AtLeast(min) {
		httpx.WriteErrorf(w, http.StatusForbidden, "this action requires the %s role", min)
		return store.User{}, false
	}
	return u, true
}

// --- two-factor -----------------------------------------------------------

func (s *Server) handleTwoFactorStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	status, err := s.auth.TwoFactorStatusFor(u)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

// handleTwoFactorEnrol issues a secret. It is returned exactly once, so the
// response is the only chance to scan or write it down.
func (s *Server) handleTwoFactorEnrol(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	enrolment, err := s.auth.BeginTwoFactorEnrolment(u)
	if err != nil {
		if errors.Is(err, auth.ErrTwoFactorAlreadyOn) {
			httpx.WriteError(w, http.StatusConflict, err.Error())
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, enrolment)
}

type twoFactorCodeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleTwoFactorConfirm(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var req twoFactorCodeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	codes, err := s.auth.ConfirmTwoFactorEnrolment(u, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidTOTP):
			httpx.WriteError(w, http.StatusUnauthorized, err.Error())
		case errors.Is(err, auth.ErrTwoFactorNotPending), errors.Is(err, auth.ErrTwoFactorAlreadyOn):
			httpx.WriteError(w, http.StatusConflict, err.Error())
		default:
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// Shown once. There is no endpoint that returns them again, only one that
	// replaces them, because only hashes are kept.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

type disableTwoFactorRequest struct {
	// Password re-proves who is asking. Turning off a second factor is exactly
	// what somebody who found an unlocked screen would want to do.
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	if s.auth.TwoFactorRequired(u) {
		httpx.WriteError(w, http.StatusForbidden,
			"this server requires two-factor authentication; it cannot be turned off")
		return
	}

	var req disableTwoFactorRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.auth.CompleteLogin(r.Context(), u.Email, req.Password, req.Code, s.auth.ClientIP(r)); err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "password or code is not correct")
		return
	}
	if err := s.auth.DisableTwoFactor(u); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var req twoFactorCodeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A fresh set invalidates the old ones, so it needs the same proof as
	// signing in does.
	if err := s.auth.VerifySecondFactor(r.Context(), u, req.Code); err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, err.Error())
		return
	}

	codes, err := s.auth.RegenerateRecoveryCodes(u)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}
