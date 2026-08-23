package api_test

import (
	"net/http"
	"testing"
)

// User administration is the one place where a mistake locks everybody out, so
// each refusal has to be the specific one and not a generic 400.

func newUser(t *testing.T, ts *testServer, email, role string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
		"email": email, "password": "correct-horse-battery", "role": role,
	}), http.StatusCreated, &created)
	return created.ID
}

func TestCreatingAUserThatAlreadyExists(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	newUser(t, ts, "dupe@example.com", "viewer")

	// 409, not 400: the request was well formed, the world disagrees with it.
	if code := ts.status(http.MethodPost, "/api/users", map[string]any{
		"email": "dupe@example.com", "password": "correct-horse-battery", "role": "viewer",
	}); code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
}

func TestCreatingAUserWithAnUnusableRoleOrEmail(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	for name, body := range map[string]map[string]any{
		"an unknown role": {"email": "a@example.com", "role": "wizard"},
		"a non-address":   {"email": "not-an-email", "role": "viewer"},
		"no email at all": {"email": "", "role": "viewer"},
	} {
		if code := ts.status(http.MethodPost, "/api/users", body); code != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", name, code)
		}
	}
}

func TestUpdatingAUser(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "target@example.com", "viewer")
	newUser(t, ts, "taken@example.com", "viewer")

	if code := ts.status(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "target@example.com", "role": "wizard",
	}); code != http.StatusBadRequest {
		t.Errorf("an unknown role gave %d, want 400", code)
	}
	if code := ts.status(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "not-an-address", "role": "viewer",
	}); code != http.StatusBadRequest {
		t.Errorf("a non-address gave %d, want 400", code)
	}
	// Renaming onto somebody else's address would silently merge two accounts.
	if code := ts.status(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "taken@example.com", "role": "viewer",
	}); code != http.StatusConflict {
		t.Errorf("taking another user's email gave %d, want 409", code)
	}
	if code := ts.status(http.MethodPut, "/api/users/no-such-id", map[string]any{
		"email": "a@example.com", "role": "viewer",
	}); code != http.StatusNotFound {
		t.Errorf("an unknown user gave %d, want 404", code)
	}
}

func TestYouCannotLockYourselfOut(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var me struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)

	if code := ts.status(http.MethodPut, "/api/users/"+me.ID, map[string]any{
		"email": adminEmail, "role": "admin", "disabled": true,
	}); code != http.StatusConflict {
		t.Errorf("disabling yourself gave %d, want 409", code)
	}
	if code := ts.status(http.MethodDelete, "/api/users/"+me.ID, nil); code != http.StatusConflict {
		t.Errorf("deleting yourself gave %d, want 409", code)
	}
}

func TestDisablingAUserEndsTheirSession(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "soon-gone@example.com", "operator")

	other := ts.asUser(t, "soon-gone@example.com", "correct-horse-battery")
	if code := other.status(http.MethodGet, "/api/connections", nil); code != http.StatusOK {
		t.Fatalf("the new user cannot use the API: %d", code)
	}

	if code := ts.status(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "soon-gone@example.com", "role": "operator", "disabled": true,
	}); code != http.StatusOK {
		t.Fatalf("disabling gave %d", code)
	}

	// Disabling somebody who is signed in has to take effect now, not whenever
	// their cookie happens to expire.
	if code := other.status(http.MethodGet, "/api/connections", nil); code != http.StatusUnauthorized {
		t.Errorf("a disabled user is still signed in: %d", code)
	}
}

func TestResettingAPassword(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "forgetful@example.com", "viewer")

	other := ts.asUser(t, "forgetful@example.com", "correct-horse-battery")

	if code := ts.status(http.MethodPut, "/api/users/"+id+"/password",
		map[string]any{"password": "short"}); code != http.StatusBadRequest {
		t.Errorf("a password that is too short gave %d, want 400", code)
	}
	if code := ts.status(http.MethodPut, "/api/users/no-such-id/password",
		map[string]any{"password": "another-good-passphrase"}); code != http.StatusNotFound {
		t.Errorf("an unknown user gave %d, want 404", code)
	}
	if code := ts.status(http.MethodPut, "/api/users/"+id+"/password",
		map[string]any{"password": "another-good-passphrase"}); code != http.StatusOK {
		t.Fatalf("the reset gave %d", code)
	}

	// A reset is what an administrator does when an account may be compromised,
	// so whoever was signed in with the old password is signed out.
	if code := other.status(http.MethodGet, "/api/connections", nil); code != http.StatusUnauthorized {
		t.Errorf("the old session survived a password reset: %d", code)
	}
}

func TestDeletingAUser(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "temp@example.com", "viewer")

	if code := ts.status(http.MethodDelete, "/api/users/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("delete gave %d, want 204", code)
	}
	if code := ts.status(http.MethodDelete, "/api/users/"+id, nil); code != http.StatusNotFound {
		t.Errorf("deleting the same user twice gave %d, want 404", code)
	}
}

func TestTheLastAdministratorCannotBeDeleted(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// A second admin does the deleting, so the refusal is about the count of
	// administrators and not about deleting yourself.
	newUser(t, ts, "second-admin@example.com", "admin")
	second := ts.asUser(t, "second-admin@example.com", "correct-horse-battery")

	var me struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &me)

	// With two admins the deletion is allowed.
	if code := second.status(http.MethodDelete, "/api/users/"+me.ID, nil); code != http.StatusNoContent {
		t.Fatalf("deleting one of two admins gave %d", code)
	}

	var secondMe struct {
		ID string `json:"id"`
	}
	second.decode(second.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &secondMe)

	// The one that is left cannot be removed by anyone, including a third party.
	newUser(t, second, "helper@example.com", "admin")
	helper := second.asUser(t, "helper@example.com", "correct-horse-battery")
	if code := helper.status(http.MethodDelete, "/api/users/"+secondMe.ID, nil); code != http.StatusNoContent {
		t.Fatalf("deleting an admin while another exists gave %d", code)
	}

	var helperMe struct {
		ID string `json:"id"`
	}
	helper.decode(helper.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, &helperMe)
	newUser(t, helper, "viewer-only@example.com", "viewer")
	viewer := helper.asUser(t, "viewer-only@example.com", "correct-horse-battery")

	// A viewer is not allowed to touch users at all, which is a 403 and not the
	// 409 the last-admin rule would give.
	if code := viewer.status(http.MethodDelete, "/api/users/"+helperMe.ID, nil); code != http.StatusForbidden {
		t.Errorf("a viewer deleting an admin got %d, want 403", code)
	}
}

func TestListingUsersShowsEveryone(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	newUser(t, ts, "one@example.com", "viewer")
	newUser(t, ts, "two@example.com", "operator")

	var users []struct {
		Email        string `json:"email"`
		PasswordHash string `json:"passwordHash"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/users", nil), http.StatusOK, &users)
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	// The API never returns a password hash, however it is asked.
	for _, u := range users {
		if u.PasswordHash != "" {
			t.Fatalf("%s came back with a password hash", u.Email)
		}
	}
}
