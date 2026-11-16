package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

//go:embed configs/*.json
var configTemplates embed.FS

const initUsage = `usage:
  taskboard init [TEMPLATE] [-config FILE] [-force]

TEMPLATE is one of: %s (default basic).`

func templateNames() []string {
	entries, _ := fs.ReadDir(configTemplates, "configs")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names
}

func configTemplate(name string) ([]byte, error) {
	if !slices.Contains(templateNames(), name) {
		return nil, fmt.Errorf("unknown template %q\n"+initUsage, name, strings.Join(templateNames(), ", "))
	}
	return configTemplates.ReadFile("configs/" + name + ".json")
}

func runInit(args []string) error {
	name := "basic"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fset.String("config", "taskboard.json", "config file to write")
	force := fset.Bool("force", false, "overwrite an existing config file")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 0 {
		return fmt.Errorf(initUsage, strings.Join(templateNames(), ", "))
	}
	data, err := configTemplate(name)
	if err != nil {
		return err
	}
	if err := writeNewConfig(*configPath, data, *force); err != nil {
		return err
	}
	run := os.Args[0]
	if *configPath != "taskboard.json" {
		run += " -config " + *configPath
	}
	fmt.Printf("Wrote %s from the %s template.\n", *configPath, name)
	if name == "basic" {
		fmt.Println("Set jira.base_url, project_keys, repos and github.organizations to your own values.")
	} else {
		fmt.Println("Check that the repos' paths match your checkouts.")
	}
	fmt.Printf("Edit the file, or start taskboard with `%s` and use Settings at http://127.0.0.1:8484/settings.\n", run)
	return nil
}

func writeNewConfig(path string, data []byte, force bool) error {
	if force {
		return writeFileAtomic(path, data)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists (pass -force to overwrite it)", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
