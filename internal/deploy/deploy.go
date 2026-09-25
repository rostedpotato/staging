// Package deploy orchestrates Jenkins-backed deployments with booking checks,
// per-slot locking, a status state machine, and console-log capture.
package deploy

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/store"
)

// Provider is the subset of the Jenkins client the service needs (mockable).
type Provider interface {
	Trigger(ctx context.Context, job string, params map[string]string) (queueURL string, err error)
	ResolveBuild(ctx context.Context, queueURL string) (number int64, buildURL string, err error)
	BuildStatus(ctx context.Context, job string, number int64) (building bool, result string, err error)
	ConsoleText(ctx context.Context, job string, number int64) (string, error)
}

type Service struct {
	cfg  *config.Config
	st   *store.Store
	prov Provider
}

func New(cfg *config.Config, st *store.Store, prov Provider) *Service {
	return &Service{cfg: cfg, st: st, prov: prov}
}

type Request struct {
	Slot     string
	Service  string // ws | fs | wbo
	RefType  string // branch | tag
	Ref      string
	UserID   int64
	Username string
	IsAdmin  bool
}

var (
	ErrDisabled    = errors.New("deploy feature is not enabled")
	ErrUnknownSvc  = errors.New("unknown service")
	ErrTagNotAllow = errors.New("this service can only deploy a branch, not a tag")
	ErrBadRef      = errors.New("branch/tag is required")
	ErrNotBooked   = errors.New("you must hold an active booking for this slot to deploy")
	ErrBookedOther = errors.New("slot is currently booked by another user")
)

// Start validates the request, acquires the slot lock, triggers Jenkins, and
// launches a background watcher. Returns the created deployment.
func (s *Service) Start(ctx context.Context, req Request) (*store.Deployment, error) {
	if !s.cfg.Deploy.Enabled {
		return nil, ErrDisabled
	}
	svc := s.cfg.ServiceByKey(req.Service)
	if svc == nil {
		return nil, ErrUnknownSvc
	}
	req.Ref = strings.TrimSpace(req.Ref)
	if req.Ref == "" {
		return nil, ErrBadRef
	}
	if req.RefType == "tag" && !svc.TagParam {
		return nil, ErrTagNotAllow
	}
	if req.RefType != "tag" {
		req.RefType = "branch"
	}

	if err := s.checkBooking(req); err != nil {
		return nil, err
	}

	dep, err := s.st.CreateDeployment(req.Slot, req.Service, req.RefType, req.Ref,
		req.UserID, req.Username, svc.Job)
	if err != nil {
		return nil, err // includes ErrSlotBusy
	}

	params := map[string]string{"ENV": req.Slot}
	if req.RefType == "tag" {
		params["TAG"] = req.Ref
	} else {
		params["BRANCH"] = req.Ref
	}

	queueURL, err := s.prov.Trigger(ctx, svc.Job, params)
	if err != nil {
		_ = s.st.Finish(dep.ID, store.DepError, "trigger failed: "+err.Error())
		return nil, err
	}
	_ = s.st.SetQueue(dep.ID, queueURL)

	go s.watch(dep.ID, svc.Job, queueURL)
	return s.st.DeploymentByID(dep.ID)
}

// ErrNoRollbackTarget is returned when no prior successful deploy exists.
var ErrNoRollbackTarget = errors.New("no previous successful deployment to roll back to")

// Rollback re-deploys the ref of the most recent successful deployment for the
// same slot+service as the given deployment. Admin-only (enforced by caller).
func (s *Service) Rollback(ctx context.Context, fromID int64, userID int64, username string, isAdmin bool) (*store.Deployment, error) {
	from, err := s.st.DeploymentByID(fromID)
	if err != nil || from == nil {
		return nil, errors.New("deployment not found")
	}
	prev, err := s.st.PreviousSuccess(from.Slot, from.Service, fromID)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		return nil, ErrNoRollbackTarget
	}
	return s.Start(ctx, Request{
		Slot:     prev.Slot,
		Service:  prev.Service,
		RefType:  prev.RefType,
		Ref:      prev.Ref,
		UserID:   userID,
		Username: username,
		IsAdmin:  isAdmin,
	})
}

// checkBooking enforces the booking policy (admins bypass, per config).
func (s *Service) checkBooking(req Request) error {
	if !s.cfg.Deploy.RequireBooking || req.IsAdmin {
		// Even when not required, block deploying over someone else's active booking.
		if res, _ := s.st.ActiveForSlot(req.Slot); res != nil && res.UserID != req.UserID && !req.IsAdmin {
			return ErrBookedOther
		}
		return nil
	}
	res, err := s.st.ActiveForSlot(req.Slot)
	if err != nil {
		return err
	}
	if res == nil {
		return ErrNotBooked
	}
	if res.UserID != req.UserID {
		return ErrBookedOther
	}
	return nil
}

// watch runs in the background: resolve the build number, stream console log,
// and finalize status. It is resilient to transient Jenkins errors.
func (s *Service) watch(depID int64, job, queueURL string) {
	poll := time.Duration(s.cfg.Deploy.PollSeconds) * time.Second
	deadline := time.Now().Add(60 * time.Minute)

	// 1) Resolve queue -> build number.
	var number int64
	var buildURL string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		n, url, err := s.prov.ResolveBuild(ctx, queueURL)
		cancel()
		if err != nil {
			s.fail(depID, "resolve build: "+err.Error())
			return
		}
		if n > 0 {
			number, buildURL = n, url
			break
		}
		time.Sleep(poll)
	}
	if number == 0 {
		s.fail(depID, "timed out waiting for build to start")
		return
	}
	_ = s.st.MarkRunning(depID, number, buildURL)

	// 2) Poll status + capture console until the build ends.
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.captureLog(ctx, depID, job, number)
		building, result, err := s.prov.BuildStatus(ctx, job, number)
		cancel()
		if err != nil {
			time.Sleep(poll)
			continue
		}
		if !building && result != "" {
			s.captureLogBg(depID, job, number)
			switch strings.ToUpper(result) {
			case "SUCCESS":
				_ = s.st.Finish(depID, store.DepSuccess, "")
			default:
				_ = s.st.Finish(depID, store.DepFailed, "build result: "+result)
			}
			return
		}
		time.Sleep(poll)
	}
	s.fail(depID, "timed out waiting for build to finish")
}

// captureLog fetches the full console text and appends any new lines.
func (s *Service) captureLog(ctx context.Context, depID int64, job string, number int64) {
	text, err := s.prov.ConsoleText(ctx, job, number)
	if err != nil {
		return
	}
	have, _ := s.st.LogCount(depID)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= have {
		return
	}
	if _, err := s.st.AppendLog(depID, have, lines[have:]); err != nil {
		log.Printf("deploy %d: append log: %v", depID, err)
	}
}

func (s *Service) captureLogBg(depID int64, job string, number int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.captureLog(ctx, depID, job, number)
}

func (s *Service) fail(depID int64, msg string) {
	_, _ = s.st.AppendLog(depID, mustCount(s.st, depID), []string{"[platform] " + msg})
	_ = s.st.Finish(depID, store.DepError, msg)
}

func mustCount(st *store.Store, depID int64) int {
	n, _ := st.LogCount(depID)
	return n
}
