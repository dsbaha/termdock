// Command termdock is the TermDock server: multi-user, multi-tenant
// browser terminals backed by Docker containers.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"termdock/internal/auth"
	"termdock/internal/config"
	"termdock/internal/db"
	"termdock/internal/dockerx"
	"termdock/internal/hub"
	"termdock/internal/orgs"
	"termdock/internal/sessions"
	"termdock/web"
)

var errNoDocker = errors.New("docker daemon is not reachable")

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	dockerClient, err := dockerx.New()
	if err != nil {
		log.Printf("docker client init failed (%v); container operations will be unavailable", err)
		dockerClient = nil
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := dockerClient.Ping(ctx); err != nil {
			// Prefer a precise, actionable diagnosis for unix sockets
			// (missing socket vs permission denied) over the raw ping error.
			if uerr := dockerx.CheckUnixSocket(dockerx.ResolveHost()); uerr != nil {
				log.Printf("container runtime not reachable: %v; container operations will be unavailable", uerr)
			} else {
				log.Printf("docker daemon not reachable (%v); container operations will be unavailable", err)
			}
			dockerClient = nil
		}
		cancel()
	}

	// The terminal hub: one docker exec PTY per running session, multiplexed
	// to browser websockets. Input marks the session active (idle reaper).
	h := hub.New(
		func(ctx context.Context, containerID, shell string, cols, rows uint) (hub.PTYConn, error) {
			if dockerClient == nil {
				return nil, errNoDocker
			}
			return dockerClient.ExecOpen(ctx, containerID, shell, cols, rows)
		},
		func(sessionID string) {
			_ = database.TouchSession(sessionID, db.Now())
		},
	)

	authH := &auth.Handler{DB: database, Config: cfg}
	orgsH := &orgs.Handler{DB: database}
	sessH := &sessions.Handler{
		DB:             database,
		Docker:         dockerClient,
		Hub:            h,
		DefaultImage:   cfg.DefaultImage,
		DefaultIdleMin: cfg.DefaultIdleMin,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Boot: reconcile sessions that were alive when we last ran, then start
	// the idle reaper.
	go func() {
		sessH.Reconcile(ctx)
		sessH.ReaperLoop(ctx)
	}()

	mux := http.NewServeMux()

	// Public auth endpoints.
	mux.HandleFunc("POST /api/auth/register", authH.Register)
	mux.HandleFunc("POST /api/auth/login", authH.Login)

	// Everything below requires a session cookie.
	authed := authH.RequireAuth

	mux.Handle("POST /api/auth/logout", authed(http.HandlerFunc(authH.Logout)))
	mux.Handle("GET /api/auth/me", authed(http.HandlerFunc(authH.Me)))

	mux.Handle("POST /api/orgs", authed(http.HandlerFunc(orgsH.Create)))
	mux.Handle("GET /api/orgs", authed(http.HandlerFunc(orgsH.List)))
	mux.Handle("GET /api/orgs/{id}/members", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgsH.ListMembers(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /api/orgs/{id}/members", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgsH.AddMember(w, r, r.PathValue("id"))
	})))
	mux.Handle("DELETE /api/orgs/{id}/members/{userID}", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgsH.RemoveMember(w, r, r.PathValue("id"), r.PathValue("userID"))
	})))

	mux.Handle("POST /api/sessions", authed(http.HandlerFunc(sessH.Create)))
	mux.Handle("GET /api/sessions", authed(http.HandlerFunc(sessH.List)))
	mux.Handle("GET /api/sessions/{id}", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.Detail(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /api/sessions/{id}/start", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.Start(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /api/sessions/{id}/stop", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.Stop(w, r, r.PathValue("id"))
	})))
	mux.Handle("DELETE /api/sessions/{id}", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.Delete(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /api/sessions/{id}/attach", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.Attach(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /api/sessions/{id}/shares", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.CreateShare(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /api/sessions/{id}/shares", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.ListShares(w, r, r.PathValue("id"))
	})))
	mux.Handle("DELETE /api/sessions/{id}/shares/{shareID}", authed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessH.RevokeShare(w, r, r.PathValue("id"), r.PathValue("shareID"))
	})))

	// Standalone popup terminal page. Auth is checked here so an expired
	// session gets a friendly message; authorization for the session itself
	// is enforced by /api/sessions/{id} and the attach websocket.
	mux.HandleFunc("GET /t/{id}", func(w http.ResponseWriter, r *http.Request) {
		if authH.Authenticate(r) == nil {
			web.NotAuthenticatedPage().ServeHTTP(w, r)
			return
		}
		web.TerminalPage().ServeHTTP(w, r)
	})

	// Frontend.
	mux.Handle("/", web.Handler())

	// Basic request logging.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mux.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("termdock listening on %s (data: %s, registration open: %v)",
			cfg.Addr, cfg.DataDir, cfg.RegOpen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
