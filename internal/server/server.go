// Package server hosts the control API (used by stdio shims) and the web UI on one local port.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/proxy"
	"github.com/mcpfault/mcpfault/internal/report"
	"github.com/mcpfault/mcpfault/internal/runner"
)

//go:embed ui
var uiFiles embed.FS

// DefaultAddr is where the control server listens unless told otherwise.
const DefaultAddr = "127.0.0.1:7361"

// Server serves hooks, the UI API and the UI.
type Server struct {
	Engine  *engine.Engine
	Addr    string
	Dir     string // sessions directory
	Cwd     string
	Version string
	Mode    string           // run | ui | proxy
	Live    *config.Scenario // proxy mode: the scenario whose servers and faults are active

	mu     sync.Mutex
	active *activeRun
	srv    *http.Server
}

type activeRun struct {
	ID       string `json:"id,omitempty"`
	Scenario string `json:"scenario"`
	File     string `json:"file"`
	cancel   context.CancelFunc
}

// Start listens and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("cannot listen on %s (is another mcpfault running? use --control to pick another address): %w", s.Addr, err)
		}
		return err
	}
	s.Addr = ln.Addr().String()
	s.srv = &http.Server{Handler: s.routes(), ReadHeaderTimeout: 30 * time.Second}
	go s.srv.Serve(ln)
	return nil
}

// Close stops the server.
func (s *Server) Close() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.srv.Shutdown(ctx)
	}
}

// URL is the UI address.
func (s *Server) URL() string { return "http://" + s.Addr }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	hooks := proxy.LocalHooks{Engine: s.Engine}

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": s.Version, "mode": s.Mode})
	})
	mux.HandleFunc("POST /v1/hooks/hello", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Server string }
		if decode(w, r, &req) {
			hooks.Hello(req.Server)
			writeJSON(w, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("POST /v1/hooks/call", func(w http.ResponseWriter, r *http.Request) {
		var req proxy.CallRequest
		if decode(w, r, &req) {
			d, _ := hooks.Call(req.Server, req.ID, req.Tool, req.Args)
			writeJSON(w, d)
		}
	})
	mux.HandleFunc("POST /v1/hooks/result", func(w http.ResponseWriter, r *http.Request) {
		var req proxy.ResultRequest
		if decode(w, r, &req) {
			d, _ := hooks.Result(req.CallID, req.Response)
			writeJSON(w, d)
		}
	})
	mux.HandleFunc("POST /v1/hooks/tools", func(w http.ResponseWriter, r *http.Request) {
		var req proxy.ToolsRequest
		if decode(w, r, &req) {
			hooks.Tools(req.Server, req.Result)
			writeJSON(w, map[string]bool{"ok": true})
		}
	})

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /api/calls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Engine.Calls(-1))
	})
	mux.HandleFunc("POST /api/runs", s.handleStartRun)
	mux.HandleFunc("POST /api/runs/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.active != nil && s.active.cancel != nil {
			s.active.cancel()
		}
		s.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/events", s.handleEvents)

	static, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return mux
}

type scenarioInfo struct {
	File        string   `json:"file"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Runs        int      `json:"runs"`
	Agent       string   `json:"agent,omitempty"`
	Faults      []string `json:"faults,omitempty"`
	Invariants  int      `json:"invariants"`
	Error       string   `json:"error,omitempty"`
}

func (s *Server) scenarios() []scenarioInfo {
	files, _ := config.Discover(s.Cwd)
	var out []scenarioInfo
	for _, f := range files {
		rel, _ := filepath.Rel(s.Cwd, f)
		sc, err := config.Load(f)
		if err != nil {
			out = append(out, scenarioInfo{File: filepath.ToSlash(rel), Name: filepath.Base(f), Error: err.Error()})
			continue
		}
		info := scenarioInfo{File: filepath.ToSlash(rel), Name: sc.Name, Description: sc.Description, Runs: sc.Runs, Invariants: len(sc.Invariants)}
		if sc.Agent != nil {
			if sc.Agent.HTTP != nil {
				info.Agent = sc.Agent.HTTP.Method + " " + sc.Agent.HTTP.URL
			} else {
				info.Agent = sc.Agent.Command.String()
			}
		}
		for _, f := range sc.Faults {
			info.Faults = append(info.Faults, f.Label)
		}
		out = append(out, info)
	}
	return out
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var active *activeRun
	if s.active != nil {
		a := *s.active
		active = &a
	}
	s.mu.Unlock()
	state := map[string]any{
		"version":   s.Version,
		"mode":      s.Mode,
		"cwd":       s.Cwd,
		"control":   s.Addr,
		"active":    active,
		"sessions":  report.List(s.Dir),
		"scenarios": s.scenarios(),
	}
	if s.Live != nil {
		var faults []string
		for _, f := range s.Live.Faults {
			faults = append(faults, f.Label)
		}
		var servers []map[string]string
		for _, srv := range s.Live.Servers {
			endpoint := srv.ProxyURL()
			if srv.Transport == "stdio" {
				endpoint = "mcpfault stdio --server " + srv.Name + " -- " + srv.Command.String()
			}
			servers = append(servers, map[string]string{"name": srv.Name, "transport": srv.Transport, "endpoint": endpoint, "upstream": srv.URL})
		}
		state["live"] = map[string]any{"scenario": s.Live.Name, "faults": faults, "servers": servers}
	}
	writeJSON(w, state)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, id, "report.json"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
		Runs int    `json:"runs"`
	}
	if !decode(w, r, &req) {
		return
	}
	path := filepath.Join(s.Cwd, filepath.FromSlash(req.File))
	if rel, err := filepath.Rel(s.Cwd, path); err != nil || strings.HasPrefix(rel, "..") {
		http.Error(w, "scenario must be inside the project", http.StatusBadRequest)
		return
	}
	sc, err := config.Load(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		http.Error(w, "a session is already running", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.active = &activeRun{Scenario: sc.Name, File: req.File, cancel: cancel}
	s.mu.Unlock()

	unsub := s.Engine.Subscribe(func(ev engine.Event) {
		if ev.Type == "session_started" {
			if m, ok := ev.Data.(map[string]any); ok {
				s.mu.Lock()
				if s.active != nil {
					s.active.ID, _ = m["id"].(string)
				}
				s.mu.Unlock()
			}
		}
	})
	go func() {
		defer cancel()
		defer unsub()
		_, err := runner.Run(ctx, s.Engine, sc, runner.Options{Runs: req.Runs, OutDir: s.Dir, Control: s.Addr, Cwd: s.Cwd})
		if err != nil {
			s.Engine.Emit(engine.Event{Type: "error", Data: map[string]any{"message": err.Error()}})
		}
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		s.Engine.Emit(engine.Event{Type: "idle"})
	}()
	writeJSON(w, map[string]any{"ok": true, "scenario": sc.Name})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan []byte, 256)
	unsub := s.Engine.Subscribe(func(ev engine.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		select {
		case ch <- b:
		default: // slow client: drop; the UI refetches on run boundaries
		}
	})
	defer unsub()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
