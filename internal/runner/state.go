package runner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type State struct {
	mu     sync.RWMutex
	path   string
	status map[string]string // file -> status (queued|processing|done|failed)
}

func NewState(workDir string) *State {
	_ = os.MkdirAll(workDir, 0o755)
	p := filepath.Join(workDir, ".hevc_state.tsv")
	return &State{
		path:   p,
		status: make(map[string]string),
	}
}

func (s *State) Load() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		s.status[parts[0]] = parts[1]
	}
	return sc.Err()
}

func (s *State) appendLine(file, st string) error {
	fd, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer fd.Close()
	_, err = fmt.Fprintf(fd, "%s\t%s\n", file, st)
	return err
}

func (s *State) Get(file string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status[file]
}

func (s *State) Set(file, st string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[file] = st
	return s.appendLine(file, st)
}
