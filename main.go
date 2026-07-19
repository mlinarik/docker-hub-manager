package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type config struct {
	Addr       string
	SecretName string
	Namespace  string
	HubURL     string
	User       string
	Password   string
}

type application struct {
	cfg    config
	log    *slog.Logger
	http   *http.Client
	secret secretStore
}

func main() {
	cfg := config{
		Addr: env("LISTEN_ADDR", ":8080"), SecretName: env("SECRET_NAME", "docker-tracker-credentials"),
		Namespace: detectNamespace(), HubURL: strings.TrimRight(env("DOCKER_HUB_URL", "https://hub.docker.com"), "/"),
		User: os.Getenv("APP_USERNAME"), Password: os.Getenv("APP_PASSWORD"),
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	app := &application{cfg: cfg, log: log, http: &http.Client{Timeout: 30 * time.Second}}
	app.secret = newKubernetesSecretStore(cfg.Namespace, cfg.SecretName, app.http)

	srv := &http.Server{Addr: cfg.Addr, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	go func() {
		log.Info("docker tracker listening", "addr", cfg.Addr, "namespace", cfg.Namespace, "secret", cfg.SecretName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	<-stop.Done()
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = srv.Shutdown(ctx)
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("PUT /api/credentials", a.saveCredentials)
	mux.HandleFunc("DELETE /api/credentials", a.deleteCredentials)
	mux.HandleFunc("GET /api/repositories", a.repositories)
	mux.HandleFunc("GET /api/repositories/{repository}/tags", a.tags)
	mux.HandleFunc("POST /api/delete", a.deleteTags)
	assets, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return a.middleware(mux)
}

func (a *application) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		if a.cfg.User != "" || a.cfg.Password != "" {
			u, p, ok := r.BasicAuth()
			userOK := subtle.ConstantTimeCompare([]byte(u), []byte(a.cfg.User)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(p), []byte(a.cfg.Password)) == 1
			if !ok || !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="Docker Tracker"`)
				problem(w, http.StatusUnauthorized, "Authentication required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func detectNamespace() string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	return env("POD_NAMESPACE", "default")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decode(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

func (a *application) status(w http.ResponseWriter, r *http.Request) {
	c, err := a.secret.Get(r.Context())
	if errors.Is(err, errNotConfigured) {
		writeJSON(w, 200, map[string]any{"configured": false, "secretName": a.cfg.SecretName, "namespace": a.cfg.Namespace})
		return
	}
	if err != nil {
		a.log.Error("read secret", "error", err)
		problem(w, 500, "Could not read the Kubernetes Secret")
		return
	}
	writeJSON(w, 200, map[string]any{"configured": true, "username": c.Username, "dockerNamespace": c.DockerNamespace, "secretName": a.cfg.SecretName, "namespace": a.cfg.Namespace})
}

func (a *application) saveCredentials(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if err := decode(r, &in); err != nil {
		problem(w, 400, "Invalid request")
		return
	}
	in.Username, in.Token, in.DockerNamespace = strings.TrimSpace(in.Username), strings.TrimSpace(in.Token), strings.TrimSpace(in.DockerNamespace)
	if in.Username == "" || in.Token == "" {
		problem(w, 400, "Docker ID and access token are required")
		return
	}
	if in.DockerNamespace == "" {
		in.DockerNamespace = in.Username
	}
	if strings.ContainsAny(in.DockerNamespace, "/?#") {
		problem(w, 400, "Invalid Docker namespace")
		return
	}
	hub := hubClient{base: a.cfg.HubURL, client: a.http, credentials: in}
	if _, err := hub.authenticate(r.Context()); err != nil {
		problem(w, 401, "Docker Hub rejected those credentials: "+err.Error())
		return
	}
	if err := a.secret.Put(r.Context(), in); err != nil {
		a.log.Error("save secret", "error", err)
		problem(w, 500, "Could not save the Kubernetes Secret")
		return
	}
	writeJSON(w, 200, map[string]bool{"saved": true})
}

func (a *application) deleteCredentials(w http.ResponseWriter, r *http.Request) {
	if err := a.secret.Delete(r.Context()); err != nil && !errors.Is(err, errNotConfigured) {
		problem(w, 500, "Could not delete the Kubernetes Secret")
		return
	}
	w.WriteHeader(204)
}

func parsePage(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 50
	}
	return page, size
}
func (a *application) hub(r *http.Request) (*hubClient, error) {
	c, err := a.secret.Get(r.Context())
	if err != nil {
		return nil, err
	}
	h := &hubClient{base: a.cfg.HubURL, client: a.http, credentials: c}
	h.bearer, err = h.authenticate(r.Context())
	return h, err
}

func (a *application) repositories(w http.ResponseWriter, r *http.Request) {
	h, err := a.hub(r)
	if err != nil {
		problem(w, 401, "Connect Docker Hub credentials first")
		return
	}
	page, size := parsePage(r)
	result, err := h.repositories(r.Context(), page, size, r.URL.Query().Get("q"))
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
func (a *application) tags(w http.ResponseWriter, r *http.Request) {
	h, err := a.hub(r)
	if err != nil {
		problem(w, 401, "Connect Docker Hub credentials first")
		return
	}
	page, size := parsePage(r)
	result, err := h.tags(r.Context(), r.PathValue("repository"), page, size)
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

type deleteRequest struct {
	Items   []deleteItem `json:"items"`
	Confirm string       `json:"confirm"`
}
type deleteItem struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}
type deleteResult struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Deleted    bool   `json:"deleted"`
	Error      string `json:"error,omitempty"`
}

func (a *application) deleteTags(w http.ResponseWriter, r *http.Request) {
	var in deleteRequest
	if err := decode(r, &in); err != nil {
		problem(w, 400, "Invalid request")
		return
	}
	if in.Confirm != "DELETE" {
		problem(w, 400, "Type DELETE to confirm")
		return
	}
	if len(in.Items) == 0 || len(in.Items) > 500 {
		problem(w, 400, "Select between 1 and 500 tags")
		return
	}
	h, err := a.hub(r)
	if err != nil {
		problem(w, 401, "Connect Docker Hub credentials first")
		return
	}
	results := make([]deleteResult, len(in.Items))
	jobs := make(chan int)
	done := make(chan struct{})
	workers := 5
	if len(in.Items) < workers {
		workers = len(in.Items)
	}
	for range workers {
		go func() {
			for i := range jobs {
				item := in.Items[i]
				res := deleteResult{Repository: item.Repository, Tag: item.Tag}
				if item.Repository == "" || item.Tag == "" || strings.ContainsAny(item.Repository+item.Tag, "/?#") {
					res.Error = "Invalid repository or tag"
				} else if err := h.deleteTag(r.Context(), item.Repository, item.Tag); err != nil {
					res.Error = err.Error()
				} else {
					res.Deleted = true
				}
				results[i] = res
			}
			done <- struct{}{}
		}()
	}
	for i := range in.Items {
		jobs <- i
	}
	close(jobs)
	for range workers {
		<-done
	}
	writeJSON(w, 200, map[string]any{"results": results})
}
