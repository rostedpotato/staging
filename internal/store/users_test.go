package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/u.db")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateUserAndVerifyPassword(t *testing.T) {
	st := newTestStore(t)

	u, err := st.CreateUser("alice", "Alice", RoleEngineering, "TempPass123!")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleEngineering {
		t.Fatalf("want engineering, got %s", u.Role)
	}
	if !u.MustResetPassword {
		t.Fatal("new account must require password reset")
	}

	// Correct password verifies.
	got, err := st.VerifyPassword("alice", "TempPass123!")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Username != "alice" {
		t.Fatalf("unexpected user: %+v", got)
	}

	// Wrong password is rejected without revealing details.
	if _, err := st.VerifyPassword("alice", "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

func TestVerifyPasswordUnknownUser(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.VerifyPassword("nobody", "whatever"); err != ErrInvalidCredentials {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

func TestSetPasswordClearsMustReset(t *testing.T) {
	st := newTestStore(t)
	u, err := st.CreateUser("bob", "Bob", RoleEngineering, "Temp1234!")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword(u.ID, "MyNewPassw0rd!"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserByUsername("bob")
	if got.MustResetPassword {
		t.Fatal("must_reset_password should be cleared after SetPassword")
	}
	// Old temp password no longer works; new one does.
	if _, err := st.VerifyPassword("bob", "Temp1234!"); err != ErrInvalidCredentials {
		t.Fatal("old password should no longer verify")
	}
	if _, err := st.VerifyPassword("bob", "MyNewPassw0rd!"); err != nil {
		t.Fatalf("new password should verify: %v", err)
	}
}

func TestResetPasswordByAdmin(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("carol", "Carol", RoleEngineering, "Temp1234!")
	_ = st.SetPassword(u.ID, "ChosenByCarol1!")

	temp, err := st.ResetPassword(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserByUsername("carol")
	if !got.MustResetPassword {
		t.Fatal("admin reset must force must_reset_password again")
	}
	if _, err := st.VerifyPassword("carol", temp); err != nil {
		t.Fatalf("new temp password should verify: %v", err)
	}
}

func TestCreateUserDuplicateUsername(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.CreateUser("dup", "A", RoleEngineering, "pass1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("dup", "A2", RoleAdmin, "pass5678"); err != ErrUserExists {
		t.Fatalf("want ErrUserExists, got %v", err)
	}
}

func TestCountAdminsAndStatus(t *testing.T) {
	st := newTestStore(t)
	a, _ := st.CreateUser("admin1", "Admin", RoleAdmin, "pass1234")
	_, _ = st.CreateUser("dev1", "Dev", RoleEngineering, "pass1234")

	if n, _ := st.CountAdmins(); n != 1 {
		t.Fatalf("want 1 admin, got %d", n)
	}
	// Disabling the admin drops the active-admin count.
	if err := st.SetStatus(a.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAdmins(); n != 0 {
		t.Fatalf("want 0 active admins after disable, got %d", n)
	}
}

func TestCountUsersBootstrap(t *testing.T) {
	st := newTestStore(t)
	if n, _ := st.CountUsers(); n != 0 {
		t.Fatalf("want 0 users initially, got %d", n)
	}
	_, _ = st.CreateUser("first", "First", RoleAdmin, "pass1234")
	if n, _ := st.CountUsers(); n != 1 {
		t.Fatalf("want 1 user after bootstrap, got %d", n)
	}
}

func TestDisabledUserNoSession(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("dev2", "Dev", RoleEngineering, "pass1234")
	sess, err := st.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Active user resolves.
	if got, _ := st.UserForSession(sess.ID); got == nil {
		t.Fatal("active user should resolve from session")
	}
	// Disabled user must not resolve, even with a valid session token.
	if err := st.SetStatus(u.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.UserForSession(sess.ID); got != nil {
		t.Fatal("disabled user must not resolve from session")
	}
}

func TestDisabledUserCannotVerifyPassword(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("dev3", "Dev", RoleEngineering, "pass1234")
	_ = st.SetStatus(u.ID, "disabled")
	if _, err := st.VerifyPassword("dev3", "pass1234"); err != ErrInvalidCredentials {
		t.Fatalf("disabled user should not verify, got %v", err)
	}
}

func TestHiddenSlots(t *testing.T) {
	st := newTestStore(t)
	if err := st.SetSlotHidden("04", true); err != nil {
		t.Fatal(err)
	}
	h, _ := st.HiddenSlots()
	if !h["04"] {
		t.Fatal("expected slot 04 hidden")
	}
	if err := st.SetSlotHidden("04", false); err != nil {
		t.Fatal(err)
	}
	h, _ = st.HiddenSlots()
	if h["04"] {
		t.Fatal("expected slot 04 visible after unhide")
	}
}
