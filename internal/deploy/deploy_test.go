package deploy

import (
	"context"
	"sync"
	"testing"
	"time"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// mockProvider simulates Jenkins: build starts after 1 poll, then succeeds.
type mockProvider struct {
	mu       sync.Mutex
	triggers int
	polls    int
	result   string // SUCCESS/FAILURE
}

func (m *mockProvider) Trigger(_ context.Context, job string, _ map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggers++
	return "https://jenkins/queue/item/1", nil
}
func (m *mockProvider) ResolveBuild(_ context.Context, _ string) (int64, string, error) {
	return 42, "https://jenkins/job/x/42/", nil
}
func (m *mockProvider) BuildStatus(_ context.Context, _ string, _ int64) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.polls++
	if m.polls < 2 {
		return true, "", nil // still building
	}
	return false, m.result, nil
}
func (m *mockProvider) ConsoleText(_ context.Context, _ string, _ int64) (string, error) {
	return "line1\nline2\nline3", nil
}

func newTestSvc(t *testing.T, prov Provider, requireBooking bool) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Deploy.Enabled = true
	cfg.Deploy.PollSeconds = 1
	cfg.Deploy.RequireBooking = requireBooking
	cfg.Deploy.Services = []config.DeployService{
		{Key: "ws", Label: "WS", Job: "deploy-ws", TagParam: true},
		{Key: "wbo", Label: "WBO", Job: "deploy-wbo", TagParam: false},
	}
	return New(cfg, st, prov), st
}

func mkUser(t *testing.T, st *store.Store, username string) *store.User {
	u, err := st.CreateUser(username, username, store.RoleEngineering, "pass1234")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestDeploySuccess(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")

	dep, err := svc.Start(context.Background(), Request{
		Slot: "dev-09", Service: "ws", RefType: "branch", Ref: "master",
		UserID: u.ID, Username: u.Username,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, st, dep.ID, store.DepSuccess)

	logs, _ := st.LogsSince(dep.ID, 0)
	if len(logs) < 3 {
		t.Fatalf("expected >=3 log lines, got %d", len(logs))
	}
}

func TestSlotLockBlocksSecond(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")

	if _, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "ws", RefType: "branch", Ref: "m", UserID: u.ID, Username: u.Username,
	}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Second deploy on same slot before the first finishes must be blocked.
	_, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "ws", RefType: "branch", Ref: "m2", UserID: u.ID, Username: u.Username,
	})
	if err != store.ErrSlotBusy {
		t.Fatalf("expected ErrSlotBusy, got %v", err)
	}
}

func TestRequireBooking(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, true)
	u := mkUser(t, st, "alice")

	// No booking -> blocked.
	_, err := svc.Start(context.Background(), Request{
		Slot: "dev-08", Service: "ws", RefType: "branch", Ref: "m", UserID: u.ID, Username: u.Username,
	})
	if err != ErrNotBooked {
		t.Fatalf("expected ErrNotBooked, got %v", err)
	}

	// With active booking -> allowed.
	_, err = st.CreateReservation("dev-08", u.ID, u.Username, "qa",
		time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), Request{
		Slot: "dev-08", Service: "ws", RefType: "branch", Ref: "m", UserID: u.ID, Username: u.Username,
	}); err != nil {
		t.Fatalf("expected allowed with booking, got %v", err)
	}
}

func TestBookedByOtherBlocks(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false) // booking not required
	owner := mkUser(t, st, "owner")
	other := mkUser(t, st, "other")

	_, err := st.CreateReservation("dev-10", owner.ID, owner.Username, "qa",
		time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Another user deploying over an active booking is blocked even if not required.
	_, err = svc.Start(context.Background(), Request{
		Slot: "dev-10", Service: "ws", RefType: "branch", Ref: "m",
		UserID: other.ID, Username: other.Username,
	})
	if err != ErrBookedOther {
		t.Fatalf("expected ErrBookedOther, got %v", err)
	}
}

func TestTagNotAllowedForWBO(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")
	_, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "wbo", RefType: "tag", Ref: "v1", UserID: u.ID, Username: u.Username,
	})
	if err != ErrTagNotAllow {
		t.Fatalf("expected ErrTagNotAllow, got %v", err)
	}
}

func TestDeployFailure(t *testing.T) {
	prov := &mockProvider{result: "FAILURE"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")
	dep, err := svc.Start(context.Background(), Request{
		Slot: "dev-09", Service: "ws", RefType: "branch", Ref: "m", UserID: u.ID, Username: u.Username,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, dep.ID, store.DepFailed)
}

func TestRollback(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")

	// First successful deploy of "v1".
	d1, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "ws", RefType: "branch", Ref: "v1", UserID: u.ID, Username: u.Username,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, d1.ID, store.DepSuccess)

	// Second deploy of "v2" (also succeeds).
	d2, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "ws", RefType: "branch", Ref: "v2", UserID: u.ID, Username: u.Username,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, d2.ID, store.DepSuccess)

	// Rollback from d2 -> should re-deploy v1 (previous success).
	rb, err := svc.Rollback(context.Background(), d2.ID, u.ID, u.Username, true)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rb.Ref != "v1" {
		t.Fatalf("rollback should target v1, got %q", rb.Ref)
	}
	waitStatus(t, st, rb.ID, store.DepSuccess)
}

func TestRollbackNoTarget(t *testing.T) {
	prov := &mockProvider{result: "SUCCESS"}
	svc, st := newTestSvc(t, prov, false)
	u := mkUser(t, st, "alice")
	d1, err := svc.Start(context.Background(), Request{
		Slot: "ho", Service: "ws", RefType: "branch", Ref: "only", UserID: u.ID, Username: u.Username,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, d1.ID, store.DepSuccess)
	// No earlier success exists -> ErrNoRollbackTarget.
	if _, err := svc.Rollback(context.Background(), d1.ID, u.ID, u.Username, true); err != ErrNoRollbackTarget {
		t.Fatalf("expected ErrNoRollbackTarget, got %v", err)
	}
}

func waitStatus(t *testing.T, st *store.Store, id int64, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		d, _ := st.DeploymentByID(id)
		if d != nil && d.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	d, _ := st.DeploymentByID(id)
	got := "nil"
	if d != nil {
		got = d.Status
	}
	t.Fatalf("deployment %d: want status %q, got %q", id, want, got)
}
