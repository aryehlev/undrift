package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cursor tracks scanning progress for one upstream.
type Cursor struct {
	LastScanned string    `json:"last_scanned"`
	Head        string    `json:"head"`
	ScannedAt   time.Time `json:"scanned_at"`
	Seq         int       `json:"seq"`
}

// Meta is global ledger metadata.
type Meta struct {
	Cursors  map[string]*Cursor `json:"cursors"`
	Revision int64              `json:"revision"`
}

// Store persists records under dir:
//
//	dir/changes/<id>.json
//	dir/tasks/<id>.json
//	dir/meta.json
//
// All read-modify-write sequences must run inside Store.Lock, which provides
// mutual exclusion between goroutines and between processes on one machine.
type Store struct {
	dir     string
	lockDir string
	mu      sync.Mutex
}

// Open returns a store rooted at dir. Lock files are kept in lockDir.
func Open(dir, lockDir string) (*Store, error) {
	for _, d := range []string{filepath.Join(dir, "changes"), filepath.Join(dir, "tasks"), lockDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, lockDir: lockDir}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// ErrNotFound is returned for missing records.
var ErrNotFound = errors.New("not found")

// staleLock is how old a lock file may become before it is broken.
const staleLock = 2 * time.Minute

// Lock runs fn while holding the store lock.
func (s *Store) Lock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.lockDir, "store.lock")
	deadline := time.Now().Add(60 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleLock {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for state lock %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(path)
	return fn()
}

func (s *Store) changePath(id string) string { return filepath.Join(s.dir, "changes", id+".json") }
func (s *Store) taskPath(id string) string   { return filepath.Join(s.dir, "tasks", id+".json") }

// GetChange loads a change by exact ID.
func (s *Store) GetChange(id string) (*Change, error) {
	var c Change
	if err := readJSON(s.changePath(id), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// PutChange saves a change, bumping its revision.
func (s *Store) PutChange(c *Change) error {
	now := Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	c.Revision++
	return writeJSON(s.changePath(c.ID), c)
}

// HasChange reports whether a change record exists.
func (s *Store) HasChange(id string) bool {
	_, err := os.Stat(s.changePath(id))
	return err == nil
}

// Changes lists all changes ordered by upstream and sequence.
func (s *Store) Changes() ([]*Change, error) {
	var out []*Change
	err := eachJSON(filepath.Join(s.dir, "changes"), func(data []byte) error {
		var c Change
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		out = append(out, &c)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Seq < out[j].Seq
	})
	return out, err
}

// FindChange resolves an ID, SHA prefix, or "#PR" reference.
func (s *Store) FindChange(ref string) (*Change, error) {
	if c, err := s.GetChange(ref); err == nil {
		return c, nil
	}
	all, err := s.Changes()
	if err != nil {
		return nil, err
	}
	var matches []*Change
	pr := 0
	if strings.HasPrefix(ref, "#") {
		pr, _ = strconv.Atoi(ref[1:])
	}
	for _, c := range all {
		switch {
		case pr > 0 && c.PRNumber == pr:
			matches = append(matches, c)
		case len(ref) >= 4 && (strings.HasPrefix(c.SHA, ref) || strings.HasPrefix(c.ID, ref)):
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("change %q: %w", ref, ErrNotFound)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("change %q is ambiguous (%d matches)", ref, len(matches))
	}
}

// GetTask loads a task by ID.
func (s *Store) GetTask(id string) (*Task, error) {
	var t Task
	if err := readJSON(s.taskPath(id), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// FindTask resolves a task ID, ID prefix, or a change reference.
func (s *Store) FindTask(ref string) (*Task, error) {
	if t, err := s.GetTask(ref); err == nil {
		return t, nil
	}
	tasks, err := s.Tasks()
	if err != nil {
		return nil, err
	}
	var matches []*Task
	for _, t := range tasks {
		if len(ref) >= 4 && strings.HasPrefix(t.ID, ref) {
			matches = append(matches, t)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("task %q is ambiguous", ref)
	}
	if c, err := s.FindChange(ref); err == nil && c.TaskID != "" {
		return s.GetTask(c.TaskID)
	}
	return nil, fmt.Errorf("task %q: %w", ref, ErrNotFound)
}

// PutTask saves a task, bumping its revision.
func (s *Store) PutTask(t *Task) error {
	now := Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	t.Revision++
	return writeJSON(s.taskPath(t.ID), t)
}

// Tasks lists all tasks ordered by priority then sequence.
func (s *Store) Tasks() ([]*Task, error) {
	var out []*Task
	err := eachJSON(filepath.Join(s.dir, "tasks"), func(data []byte) error {
		var t Task
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		out = append(out, &t)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out, err
}

// Meta loads ledger metadata.
func (s *Store) Meta() (*Meta, error) {
	m := &Meta{Cursors: map[string]*Cursor{}}
	err := readJSON(filepath.Join(s.dir, "meta.json"), m)
	if errors.Is(err, ErrNotFound) {
		return m, nil
	}
	if m.Cursors == nil {
		m.Cursors = map[string]*Cursor{}
	}
	return m, err
}

// PutMeta saves ledger metadata.
func (s *Store) PutMeta(m *Meta) error {
	m.Revision++
	return writeJSON(filepath.Join(s.dir, "meta.json"), m)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data)
}

// WriteFileAtomic writes data to path via a temporary file and rename, so
// readers never observe partially written records.
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func eachJSON(dir string, fn func([]byte) error) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if err := fn(data); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}
