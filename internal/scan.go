package internal

import (
	"os"
	"path/filepath"
	"sort"
)

// composeFileNames mirrors the discovery order `docker compose` itself
// uses, so we only treat a folder as a stack if compose would actually
// find a file in it.
var composeFileNames = []string{
	"compose.yaml",
	"compose.yml",
	"docker-compose.yaml",
	"docker-compose.yml",
}

// ScanStacks lists immediate subdirectories of root that contain a
// compose file, sorted by name.
func ScanStacks(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if hasComposeFile(filepath.Join(root, e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func hasComposeFile(dir string) bool {
	for _, f := range composeFileNames {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}
