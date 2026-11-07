package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"sync"
)

//go:embed extension
var extensionFS embed.FS

const extensionZipName = "taskboard-tab-groups.zip"

// Files sit at the archive root so Firefox can load the zip itself as a temporary add-on.
var extensionZip = sync.OnceValues(func() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	sub, err := fs.Sub(extensionFS, "extension")
	if err != nil {
		return nil, err
	}
	if err := zw.AddFS(sub); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
})

type extensionData struct {
	Origin   string
	NeedsAdd bool
	ZipName  string
}

func (s *server) extensionPage(w http.ResponseWriter, r *http.Request) {
	u := url.URL{Scheme: "http", Host: r.Host}
	if r.TLS != nil {
		u.Scheme = "https"
	}
	builtin := u.Scheme == "http" && slices.Contains([]string{"127.0.0.1", "localhost"}, u.Hostname())
	render(w, r, "extension.html", extensionData{Origin: u.String(), NeedsAdd: !builtin, ZipName: extensionZipName})
}

func (s *server) extensionDownload(w http.ResponseWriter, r *http.Request) {
	data, err := extensionZip()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+extensionZipName+`"`)
	w.Write(data)
}
