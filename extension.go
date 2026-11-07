package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed extension
var extensionFS embed.FS

const extensionZipName = "taskboard-tab-groups.zip"

var extensionVersion = sync.OnceValue(func() string {
	var m struct{ Version string }
	data, err := extensionFS.ReadFile("extension/manifest.json")
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err != nil {
		panic(fmt.Sprintf("embedded extension manifest: %v", err))
	}
	return m.Version
})

func requestOrigin(r *http.Request) *url.URL {
	u := &url.URL{Scheme: "http", Host: r.Host}
	if r.TLS != nil {
		u.Scheme = "https"
	}
	return u
}

// extensionFiles returns the extension with origin added to the content script's matches, so a
// board reached at another address works as soon as the extension is loaded, without the options page.
func extensionFiles(origin *url.URL) (map[string][]byte, error) {
	entries, err := extensionFS.ReadDir("extension")
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if files[e.Name()], err = extensionFS.ReadFile("extension/" + e.Name()); err != nil {
			return nil, err
		}
	}
	// Leave IPv6 literals to the options page rather than risk a pattern that stops the extension loading.
	if strings.Contains(origin.Hostname(), ":") {
		return files, nil
	}
	var m map[string]any
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		return nil, err
	}
	var first map[string]any
	if scripts, _ := m["content_scripts"].([]any); len(scripts) > 0 {
		first, _ = scripts[0].(map[string]any)
	}
	if first == nil {
		return nil, fmt.Errorf("embedded extension manifest has no content script")
	}
	matches, _ := first["matches"].([]any)
	if pattern := origin.Scheme + "://" + origin.Hostname() + "/*"; !slices.Contains(matches, any(pattern)) {
		first["matches"] = append(matches, pattern)
		if files["manifest.json"], err = json.MarshalIndent(m, "", "  "); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (s *server) extensionPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, "extension.html", struct{ ZipName string }{extensionZipName})
}

func (s *server) extensionFilesJSON(w http.ResponseWriter, r *http.Request) {
	files, err := extensionFiles(requestOrigin(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	text := make(map[string]string, len(files))
	for name, data := range files {
		text[name] = string(data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(text)
}

func (s *server) extensionDownload(w http.ResponseWriter, r *http.Request) {
	files, err := extensionFiles(requestOrigin(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	now := time.Now()
	// Files sit at the archive root so Firefox can load the zip itself as a temporary add-on.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		// Embedded files have no modification time, which zip.AddFS would record as 2049.
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err == nil {
			_, err = f.Write(files[name])
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := zw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+extensionZipName+`"`)
	w.Write(buf.Bytes())
}
