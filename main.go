package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"compose-updater/internal"
)

type app struct {
	tmpl        *template.Template
	auth        *internal.Auth
	state       *internal.StateStore
	mgr         *internal.Manager
	composeRoot string
}

type row struct {
	Name          string
	Enabled       bool
	State         internal.RunState
	LastRun       string
	LastError     string
	PostScript    string
	PostContainer string
}

type finalStepView struct {
	Enabled bool
	Script  string
}

func (a *app) finalStepView() finalStepView {
	enabled, script := a.state.GetFinalStep()
	return finalStepView{Enabled: enabled, Script: script}
}

func main() {
	composeRoot := getenv("COMPOSE_ROOT", "/compose")
	dataDir := getenv("DATA_DIR", "/data")
	addr := getenv("LISTEN_ADDR", ":8080")
	maxParallel, err := strconv.Atoi(getenv("MAX_PARALLEL", "4"))
	if err != nil || maxParallel < 1 {
		maxParallel = 4
	}

	tmpl, err := template.ParseGlob(filepath.Join(templateDir(), "*.html"))
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	state, err := internal.NewStateStore(filepath.Join(dataDir, "state.json"))
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	auth, err := internal.NewAuth(filepath.Join(dataDir, "auth.json"))
	if err != nil {
		log.Fatalf("load auth: %v", err)
	}
	mgr := internal.NewManager(composeRoot, maxParallel, state)

	a := &app{tmpl: tmpl, auth: auth, state: state, mgr: mgr, composeRoot: composeRoot}

	if err := a.rescan(); err != nil {
		log.Printf("initial scan of %s failed: %v", composeRoot, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /setup", a.handleSetupGet)
	mux.HandleFunc("POST /setup", a.handleSetupPost)
	mux.HandleFunc("GET /login", a.handleLoginGet)
	mux.HandleFunc("POST /login", a.handleLoginPost)
	mux.HandleFunc("POST /logout", a.handleLogout)

	mux.HandleFunc("GET /{$}", a.auth.RequireAuth(a.handleIndex))
	mux.HandleFunc("POST /api/stacks/{name}/toggle", a.auth.RequireAuth(a.handleToggle))
	mux.HandleFunc("POST /api/stacks/{name}/post-script", a.auth.RequireAuth(a.handlePostScript))
	mux.HandleFunc("POST /api/final-step", a.auth.RequireAuth(a.handleFinalStep))
	mux.HandleFunc("POST /api/final-step/run", a.auth.RequireAuth(a.handleFinalStepRun))
	mux.HandleFunc("POST /api/rescan", a.auth.RequireAuth(a.handleRescan))
	mux.HandleFunc("POST /api/update", a.auth.RequireAuth(a.handleUpdate))
	mux.HandleFunc("GET /api/ws", a.auth.RequireAuth(a.handleWS))

	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(http.FileServer(http.Dir(staticDir())))))

	log.Printf("compose-updater listening on %s (compose root: %s, max parallel: %d)", addr, composeRoot, maxParallel)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// templateDir/staticDir allow running `go run .` from the repo root in
// dev while the Docker image keeps templates/static next to the binary.
func templateDir() string {
	if v := os.Getenv("TEMPLATE_DIR"); v != "" {
		return v
	}
	return "templates"
}

func staticDir() string {
	if v := os.Getenv("STATIC_DIR"); v != "" {
		return v
	}
	return "static"
}

// noCache tells browsers and any caching layer in front of us (e.g. a
// Cloudflare tunnel, which otherwise applies its own default edge TTL to
// static file extensions when the origin is silent) to always revalidate
// with the origin instead of serving a stale copy after a deploy.
// http.FileServer already sets Last-Modified/ETag, so revalidation is a
// cheap 304, not a full refetch.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func (a *app) rescan() error {
	names, err := internal.ScanStacks(a.composeRoot)
	if err != nil {
		return err
	}
	if _, err := a.state.Sync(names); err != nil {
		return err
	}
	a.mgr.Reconcile(names)
	return nil
}

func (a *app) buildRows() []row {
	cfgs := a.state.Snapshot()
	names := make([]string, 0, len(cfgs))
	for n := range cfgs {
		names = append(names, n)
	}
	sort.Strings(names)

	rows := make([]row, 0, len(names))
	for _, n := range names {
		rows = append(rows, a.buildRow(n, cfgs[n]))
	}
	return rows
}

func (a *app) buildRow(name string, cfg internal.StackConfig) row {
	r := row{Name: name, Enabled: cfg.Enabled, State: internal.StateIdle, PostScript: cfg.PostUpdateScript, PostContainer: cfg.PostUpdateContainer}
	if st, ok := a.mgr.Status(name); ok {
		r.State = st.State
		r.LastError = st.LastError
		if !st.LastRun.IsZero() {
			r.LastRun = st.LastRun.UTC().Format(time.RFC3339)
		}
	}
	return r
}

// --- auth handlers ---

func (a *app) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	if a.auth.HasPassword() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, "setup.html", map[string]any{})
}

func (a *app) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	if a.auth.HasPassword() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.render(w, "setup.html", map[string]any{"Error": "bad form"})
		return
	}
	pw := r.FormValue("password")
	confirm := r.FormValue("confirm")

	if len(pw) < 8 {
		a.render(w, "setup.html", map[string]any{"Error": "Password must be at least 8 characters."})
		return
	}
	if pw != confirm {
		a.render(w, "setup.html", map[string]any{"Error": "Passwords do not match."})
		return
	}
	if err := a.auth.SetPassword(pw); err != nil {
		log.Printf("set password: %v", err)
		a.render(w, "setup.html", map[string]any{"Error": "Could not save password, check server logs."})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if !a.auth.HasPassword() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	a.render(w, "login.html", map[string]any{})
}

func (a *app) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if !a.auth.HasPassword() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.render(w, "login.html", map[string]any{"Error": "bad form"})
		return
	}
	if !a.auth.CheckPassword(r.FormValue("password")) {
		a.render(w, "login.html", map[string]any{"Error": "Incorrect password."})
		return
	}
	token := a.auth.NewSession()
	internal.SetSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		a.auth.RevokeSession(cookie.Value)
	}
	internal.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- app handlers ---

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	snap := a.mgr.Snapshot()
	a.render(w, "index.html", map[string]any{
		"Rows":      a.buildRows(),
		"Active":    snap.Active,
		"FinalStep": a.finalStepView(),
	})
}

func (a *app) handleToggle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, ok := a.state.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg, err := a.state.SetEnabled(name, !cfg.Enabled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.renderFragment(w, "row", a.buildRow(name, cfg))
}

func (a *app) handlePostScript(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := a.state.Get(name); !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg, err := a.state.SetPostScript(name, strings.TrimSpace(r.FormValue("container")), r.FormValue("script"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.renderFragment(w, "row", a.buildRow(name, cfg))
}

func (a *app) handleFinalStep(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	enabled := r.FormValue("enabled") != ""
	if err := a.state.SetFinalStep(enabled, r.FormValue("script")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.renderFragment(w, "final-step-btn", a.finalStepView())
}

func (a *app) handleFinalStepRun(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if err := a.mgr.RunFinalStepNow(r.FormValue("script")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *app) handleRescan(w http.ResponseWriter, r *http.Request) {
	if err := a.rescan(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.renderFragment(w, "tbody", map[string]any{"Rows": a.buildRows()})
}

func (a *app) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	isGroupRun := target == "all"

	var names []string
	if isGroupRun {
		for _, row := range a.buildRows() {
			if row.Enabled {
				names = append(names, row.Name)
			}
		}
	} else if target != "" {
		names = []string{target}
	}

	if err := a.mgr.StartRun(names, isGroupRun); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// wsUpgrader accepts every Origin: RequireAuth already gates this
// endpoint via the session cookie, and Traefik/Cloudflare's header
// forwarding has produced subtle Host-vs-Origin mismatches in this
// deployment before, so there's nothing gained by also relying on
// gorilla's default same-origin check here.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsMessage is one push to a connected browser - see internal.Event for
// the server-side source this is built from.
type wsMessage struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// wsPingInterval keeps the connection active through any idle timeout an
// intermediary (Traefik, the Cloudflare Tunnel) might apply - cheap
// insurance, not something this deployment has needed yet for WebSocket
// specifically, but SSE's failure mode here was exactly this class of
// problem for a different transport.
const wsPingInterval = 30 * time.Second

func (a *app) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	ch := a.mgr.Subscribe()
	defer a.mgr.Unsubscribe(ch)

	if err := conn.WriteJSON(wsMessage{Type: "snapshot", Data: a.mgr.Snapshot()}); err != nil {
		return
	}

	// The browser never sends anything meaningful - this goroutine exists
	// only to drive gorilla's control-frame handling and notice when the
	// connection goes away.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-closed:
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(wsMessage{Type: ev.Name, Data: ev.Data}); err != nil {
				return
			}
		}
	}
}

// render writes a full page (extends base layout via {{define "content"}}).
func (a *app) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *app) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render fragment %s: %v", name, err)
	}
}
