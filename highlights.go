package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type highlights struct {
	mu   sync.Mutex
	path string
	ids  map[string]bool
}

type highlightsFile struct {
	Highlights []string `json:"highlights"`
}

func highlightsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "taskboard-highlights.json")
}

// loadHighlights treats a missing file as empty; an empty path keeps highlights in memory only.
func loadHighlights(path string) (*highlights, error) {
	h := &highlights{path: path, ids: map[string]bool{}}
	if path == "" {
		return h, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	var f highlightsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, id := range f.Highlights {
		h.ids[id] = true
	}
	return h, nil
}

func (h *highlights) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sortedIDs(h.ids)
}

// set also drops IDs missing from present; a nil present means the board may be incomplete, so nothing is dropped.
func (h *highlights) set(id string, on bool, present map[string]bool) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := pruneHighlights(h.ids, present)
	if on {
		next[id] = true
	} else {
		delete(next, id)
	}
	list := sortedIDs(next)
	if h.path != "" {
		data, err := json.MarshalIndent(highlightsFile{list}, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(h.path, append(data, '\n')); err != nil {
			return nil, err
		}
	}
	h.ids = next
	return list, nil
}

func pruneHighlights(ids, present map[string]bool) map[string]bool {
	next := map[string]bool{}
	for id := range ids {
		if present == nil || present[id] {
			next[id] = true
		}
	}
	return next
}

func sortedIDs(ids map[string]bool) []string {
	list := slices.AppendSeq(make([]string, 0, len(ids)), maps.Keys(ids))
	slices.Sort(list)
	return list
}

func boardIDs(snap Snapshot) map[string]bool {
	ids := map[string]bool{}
	for _, t := range snap.Tasks {
		ids[t.Key] = true
	}
	for _, p := range slices.Concat(snap.Reviews, snap.OtherPRs) {
		ids[p.ID()] = true
	}
	return ids
}
