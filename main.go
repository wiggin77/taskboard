package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		run := map[string]func([]string) error{"certs": runCerts, "init": runInit}[os.Args[1]]
		if run != nil {
			if err := run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}
	configPath := flag.String("config", "taskboard.json", "path to the config file")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `usage:
  taskboard [-config FILE]                           start the board
  taskboard init [TEMPLATE] [-config FILE] [-force]  write a starter config (TEMPLATE: %s)
  taskboard certs init|server|client ...             manage certificates for remote access
`, strings.Join(templateNames(), ", "))
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("%s not found. Create it with `taskboard init`, or `taskboard init mattermost` at Mattermost.", *configPath)
	}
	if err != nil {
		log.Fatal(err)
	}
	for _, tool := range []string{"acli", "gh", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			log.Fatalf("%s not found in PATH", tool)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	board := NewBoard(cfg)
	if board.highlights, err = loadHighlights(highlightsPath(*configPath)); err != nil {
		log.Fatal(err)
	}
	handler := newServer(board, *configPath)
	servers := []*http.Server{{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}}
	if cfg.TLSListen != "" {
		tlsConfig, err := serverTLS(cfg.tlsDir(*configPath), func() []string { return board.Config().RevokedCerts })
		if err != nil {
			log.Fatal(err)
		}
		servers = append(servers, &http.Server{Addr: cfg.TLSListen, Handler: handler, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second})
	}
	go board.Run(ctx)

	errs := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			var err error
			if srv.TLSConfig != nil {
				log.Printf("taskboard listening on https://%s (client certificate required)", srv.Addr)
				err = srv.ListenAndServeTLS("", "")
			} else {
				log.Printf("taskboard listening on http://%s", srv.Addr)
				err = srv.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	select {
	case err := <-errs:
		log.Fatal(err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
}
