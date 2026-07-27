package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UpdateType distinguishes binary-core updates from plain file updates.
type UpdateType string

const (
	UpdateTypeCore    UpdateType = "core"
	UpdateTypeFile    UpdateType = "file"
	UpdateTypePackage UpdateType = "package"
)

// Task represents one auto-update job.
type Task struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	UpdateType     UpdateType `json:"update_type"`
	RepoURL        string     `json:"repo_url"`   // https://github.com/owner/repo
	CurrentVersion string     `json:"current_version"`
	LatestVersion  string     `json:"latest_version"`
	FileKeyword    string     `json:"file_keyword"`
	BinaryKeyword  string     `json:"binary_keyword"` // optional: pick specific binary from archive (core type only)
	Rename         string     `json:"rename"`      // optional
	TargetPath     string     `json:"target_path"` // absolute path
	PreCmd         string     `json:"pre_cmd"`
	PostCmd        string     `json:"post_cmd"`
	Cron           string     `json:"cron"` // empty = manual only
	LastCheck      time.Time  `json:"last_check"`
	LastUpdate     time.Time  `json:"last_update"`
	Status         string     `json:"status"` // idle / checking / updating / ok / error
	LastError      string     `json:"last_error"`
}

// Settings holds global panel-wide configuration (not tied to a single task).
type Settings struct {
	GithubProxyEnabled bool `json:"github_proxy_enabled"`
}

// Store holds all tasks in memory and persists them as JSON.
type Store struct {
	mu       sync.RWMutex
	dataDir  string
	Tasks    map[string]*Task `json:"tasks"`
	Settings Settings         `json:"settings"`
}

func NewStore(dataDir string) *Store {
	return &Store{
		dataDir: dataDir,
		Tasks:   make(map[string]*Task),
	}
}

func (s *Store) dbPath() string {
	return filepath.Join(s.dataDir, "tasks.json")
}

// storeFile is the on-disk shape: tasks + global settings.
type storeFile struct {
	Tasks    map[string]*Task `json:"tasks"`
	Settings Settings         `json:"settings"`
}

func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.dbPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// New format: {"tasks": {...}, "settings": {...}}.
	var sf storeFile
	if err := json.Unmarshal(data, &sf); err == nil && sf.Tasks != nil {
		s.Tasks = sf.Tasks
		s.Settings = sf.Settings
		return nil
	}

	// Legacy format: the file itself is the tasks map (no "tasks" wrapper key).
	var legacy map[string]*Task
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if legacy == nil {
		legacy = make(map[string]*Task)
	}
	s.Tasks = legacy
	return nil
}

func (s *Store) save() error {
	sf := storeFile{Tasks: s.Tasks, Settings: s.Settings}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dbPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.dbPath())
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// GetSettings returns a copy of the current global settings.
func (s *Store) GetSettings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Settings
}

// UpdateSettings applies fn to the global settings and persists the result.
func (s *Store) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.Settings)
	return s.save()
}

func (s *Store) GetTask(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.Tasks[id]
	return t, ok
}

func (s *Store) ListTasks() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Task, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		list = append(list, t)
	}
	return list
}

func (s *Store) UpsertTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tasks[t.ID] = t
	return s.save()
}

func (s *Store) DeleteTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Tasks, id)
	return s.save()
}

func (s *Store) UpdateTaskField(id string, fn func(*Task)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Tasks[id]
	if !ok {
		return nil
	}
	fn(t)
	return s.save()
}

// LogPath returns the log file path for a task (stored in /tmp for runtime logs).
func (s *Store) LogPath(taskID string) string {
	return filepath.Join("/tmp", "updater-log-"+taskID+".log")
}
