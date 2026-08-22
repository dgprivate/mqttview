package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mqttview/mqttview/internal/auth"
	"github.com/mqttview/mqttview/internal/httpx"
	"github.com/mqttview/mqttview/internal/store"
)

func (s *Server) mountUsers(r chi.Router) {
	r.Route("/users", func(r chi.Router) {
		r.Use(s.auth.RequireRole(store.RoleAdmin))
		r.Get("/", s.handleListUsers)
		r.Post("/", s.handleCreateUser)
		r.Put("/{id}", s.handleUpdateUser)
		r.Put("/{id}/password", s.handleResetPassword)
		r.Delete("/{id}/2fa", s.handleClearTwoFactor)
		r.Delete("/{id}", s.handleDeleteUser)
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, _ *http.Request) {
	users, err := s.db.ListUsers()
	if err != nil {
		s.log.Error("listing users failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	if users == nil {
		users = []store.User{}
	}
	httpx.WriteJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Email    string     `json:"email"`
	Name     string     `json:"name"`
	Role     store.Role `json:"role"`
	Password string     `json:"password"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if store.NormalizeEmail(req.Email) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "email is required")
		return
	}
	if req.Role == "" {
		req.Role = store.RoleViewer
	}
	if !store.ValidRole(req.Role) {
		httpx.WriteErrorf(w, http.StatusBadRequest, "unknown role %q", req.Role)
		return
	}

	// An account with no password can still be created: it is the normal way
	// to pre-authorise someone who will sign in through SSO.
	var hash string
	if req.Password != "" {
		var err error
		if hash, err = auth.HashPassword(req.Password); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	u, err := s.db.CreateUser(store.User{
		ID:           uuid.NewString(),
		Email:        req.Email,
		Name:         req.Name,
		Role:         req.Role,
		PasswordHash: hash,
		Provider:     "local",
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, http.StatusConflict, "a user with that email already exists")
			return
		}
		s.log.Error("creating user failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not create the user")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, u)
}

type updateUserRequest struct {
	Email    string     `json:"email"`
	Name     string     `json:"name"`
	Role     store.Role `json:"role"`
	Disabled bool       `json:"disabled"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor, _ := auth.UserFrom(r.Context())

	var req updateUserRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !store.ValidRole(req.Role) {
		httpx.WriteErrorf(w, http.StatusBadRequest, "unknown role %q", req.Role)
		return
	}

	existing, err := s.db.GetUser(id)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "no such user")
		return
	}

	// Losing the last admin would lock everyone out of user management, so
	// the demotion or disable is refused rather than silently accepted.
	losingAdmin := existing.Role == store.RoleAdmin && !existing.Disabled &&
		(req.Role != store.RoleAdmin || req.Disabled)
	if losingAdmin {
		admins, err := s.db.CountAdmins()
		if err == nil && admins <= 1 {
			httpx.WriteError(w, http.StatusConflict, "this is the last administrator; promote someone else first")
			return
		}
	}
	if actor.ID == id && req.Disabled {
		httpx.WriteError(w, http.StatusConflict, "you cannot disable your own account")
		return
	}

	existing.Email = req.Email
	existing.Name = req.Name
	existing.Role = req.Role
	existing.Disabled = req.Disabled

	if err := s.db.UpdateUser(existing); err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, http.StatusConflict, "a user with that email already exists")
			return
		}
		s.log.Error("updating user failed", "id", id, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not update the user")
		return
	}
	if req.Disabled {
		if err := s.db.DeleteUserSessions(id); err != nil {
			s.log.Warn("clearing sessions of disabled user failed", "id", id, "error", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, existing)
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req resetPasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.db.SetPasswordHash(id, hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, "no such user")
			return
		}
		s.log.Error("resetting password failed", "id", id, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not reset the password")
		return
	}
	if err := s.db.DeleteUserSessions(id); err != nil {
		s.log.Warn("clearing sessions after password reset failed", "id", id, "error", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor, _ := auth.UserFrom(r.Context())

	if actor.ID == id {
		httpx.WriteError(w, http.StatusConflict, "you cannot delete your own account")
		return
	}
	existing, err := s.db.GetUser(id)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "no such user")
		return
	}
	if existing.Role == store.RoleAdmin && !existing.Disabled {
		admins, err := s.db.CountAdmins()
		if err == nil && admins <= 1 {
			httpx.WriteError(w, http.StatusConflict, "this is the last administrator")
			return
		}
	}
	if err := s.db.DeleteUser(id); err != nil {
		s.log.Error("deleting user failed", "id", id, "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete the user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClearTwoFactor turns off a user's second factor.
//
// This is the lost-phone path: an administrator does it for somebody else, and
// unlike the self-service route it needs no code, because the whole reason for
// asking is that the person cannot produce one. It is logged with both names.
func (s *Server) handleClearTwoFactor(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}

	target, err := s.db.GetUser(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "no such user")
		return
	}
	if err := s.auth.DisableTwoFactor(target); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.log.Warn("administrator cleared two-factor for another account",
		"admin", actor.Email, "user", target.Email)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
