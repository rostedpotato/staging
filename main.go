// Command staging-platform serves the read-only staging dashboard (Phase 1).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"parkee/staging-platform/internal/auth"
	"parkee/staging-platform/internal/config"
	"parkee/staging-platform/internal/deploy"
	"parkee/staging-platform/internal/discovery"
	"parkee/staging-platform/internal/jenkins"
	"parkee/staging-platform/internal/store"
	"parkee/staging-platform/internal/web"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Periodically drop expired sessions.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			st.PurgeExpiredSessions()
		}
	}()

	authn := auth.New(cfg, st)
	bootstrapAdmin(st)

	jc := jenkins.New(cfg.Deploy.JenkinsURL, cfg.Deploy.JenkinsUser, cfg.Deploy.JenkinsToken)
	dep := deploy.New(cfg, st, jc)
	if cfg.Deploy.Enabled && (cfg.Deploy.JenkinsURL == "" || cfg.Deploy.JenkinsToken == "") {
		log.Printf("WARNING: deploy.enabled but Jenkins URL/token missing -> deploys will fail")
	}

	disc := discovery.New(cfg.Discovery)
	srv, err := web.NewServer(cfg, disc, authn, st, dep)
	if err != nil {
		log.Fatalf("web: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("staging-platform listening on %s (config %s)", cfg.Listen, *cfgPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Println("shut down")
}

// bootstrapAdmin creates the very first admin account if the database has no
// users at all yet, so the platform is never locked out. The temp password is
// printed to the log (operator must relay it, then the user resets on first
// login). This never runs again once at least one account exists.
func bootstrapAdmin(st *store.Store) {
	n, err := st.CountUsers()
	if err != nil {
		log.Printf("bootstrap check failed: %v", err)
		return
	}
	if n > 0 {
		return
	}
	temp, err := store.GenerateTempPassword()
	if err != nil {
		log.Fatalf("bootstrap: generate password: %v", err)
	}
	u, err := st.CreateUser("admin", "Administrator", store.RoleAdmin, temp)
	if err != nil {
		log.Fatalf("bootstrap: create admin: %v", err)
	}
	log.Printf("======================================================================")
	log.Printf("No accounts found. Created initial admin account:")
	log.Printf("  username: %s", u.Username)
	log.Printf("  password: %s   (must be changed on first login)", temp)
	log.Printf("======================================================================")
}
