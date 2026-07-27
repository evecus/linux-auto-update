package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type Server struct {
	mux       *http.ServeMux
	store     *Store
	scheduler *Scheduler
}

func NewServer(store *Store, scheduler *Scheduler) *Server {
	s := &Server{
		mux:       http.NewServeMux(),
		store:     store,
		scheduler: scheduler,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/tasks/", s.handleTask)
	s.mux.HandleFunc("/api/check/", s.handleCheck)
	s.mux.HandleFunc("/api/update/", s.handleUpdate)
	s.mux.HandleFunc("/api/logs/", s.handleLogs)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GET  /api/tasks         -> list all tasks
// POST /api/tasks         -> create task
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks := s.store.ListTasks()
		if tasks == nil {
			tasks = []*Task{}
		}
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].Name < tasks[j].Name
		})
		writeJSON(w, 200, tasks)

	case http.MethodPost:
		var t Task
		if err := readJSON(r, &t); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if t.RepoURL == "" || t.FileKeyword == "" || (t.UpdateType != UpdateTypePackage && t.TargetPath == "") {
			writeJSON(w, 400, map[string]string{"error": "repo_url and file_keyword are required; target_path is required for core/file types"})
			return
		}
		if t.ID == "" {
			t.ID = newID()
		}
		t.Status = "idle"
		if err := s.store.UpsertTask(&t); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		s.scheduler.Register(&t)
		writeJSON(w, 201, t)

	default:
		w.WriteHeader(405)
	}
}

// GET    /api/tasks/{id}   -> get task
// PUT    /api/tasks/{id}   -> update task
// DELETE /api/tasks/{id}   -> delete task
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if id == "" {
		w.WriteHeader(400)
		return
	}

	switch r.Method {
	case http.MethodGet:
		t, ok := s.store.GetTask(id)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, t)

	case http.MethodPut:
		var t Task
		if err := readJSON(r, &t); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		t.ID = id
		if err := s.store.UpsertTask(&t); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		s.scheduler.Register(&t)
		writeJSON(w, 200, t)

	case http.MethodDelete:
		s.scheduler.Unregister(id)
		// remove log file
		os.Remove(s.store.LogPath(id))
		if err := s.store.DeleteTask(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "deleted"})

	default:
		w.WriteHeader(405)
	}
}

// POST /api/check/{id}  -> check for update (non-blocking, result via status)
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/check/")
	t, ok := s.store.GetTask(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	go func() {
		s.store.UpdateTaskField(t.ID, func(tk *Task) { tk.Status = "checking" })
		logPath := s.store.LogPath(t.ID)
		lf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if lf != nil {
			lf.WriteString("\n========== check " + time.Now().Format("2006-01-02 15:04:05") + " ==========\n")
		}
		logger := func(msg string) {
			if lf != nil {
				lf.WriteString("[" + time.Now().Format("15:04:05") + "] " + msg + "\n")
			}
		}
		result, err := CheckUpdate(t, logger)
		if lf != nil {
			lf.Close()
		}
		if err != nil {
			s.store.UpdateTaskField(t.ID, func(tk *Task) {
				tk.Status = "error"
				tk.LastError = err.Error()
				tk.LastCheck = time.Now()
			})
			return
		}
		s.store.UpdateTaskField(t.ID, func(tk *Task) {
			tk.LastCheck = time.Now()
			tk.LatestVersion = result.LatestVersion
			if result.HasUpdate {
				tk.Status = "update_available"
				tk.LastError = ""
			} else {
				tk.Status = "ok"
				tk.LastError = ""
			}
		})
	}()
	writeJSON(w, 202, map[string]string{"status": "checking"})
}

// POST /api/update/{id}  -> trigger update now (non-blocking)
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/update/")
	t, ok := s.store.GetTask(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	go RunUpdate(t, s.store)
	writeJSON(w, 202, map[string]string{"status": "updating"})
}

// GET /api/logs/{id}  -> return last N lines of log
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	logPath := s.store.LogPath(id)
	data, err := os.ReadFile(logPath)
	if err != nil {
		writeJSON(w, 200, map[string]string{"log": ""})
		return
	}
	writeJSON(w, 200, map[string]string{"log": string(data)})
}

// GET /api/settings  -> return global settings
// PUT /api/settings  -> update global settings (partial)
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.store.GetSettings())

	case http.MethodPut:
		var req struct {
			GithubProxyEnabled *bool `json:"github_proxy_enabled"`
		}
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := s.store.UpdateSettings(func(st *Settings) {
			if req.GithubProxyEnabled != nil {
				st.GithubProxyEnabled = *req.GithubProxyEnabled
			}
		}); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, s.store.GetSettings())

	default:
		w.WriteHeader(405)
	}
}

// GET / -> serve embedded HTML panel
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(indexHTML))
}
