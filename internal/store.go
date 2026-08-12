// Package internal implements compose-updater's server: scanning compose
// folders, persisting toggle state, running updates, and basic auth.
package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// loadJSON reads path into v. If the file doesn't exist, v is left
// untouched and no error is returned (caller sees zero value).
func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// saveJSON writes v to path atomically (write to temp file, then rename)
// so a crash mid-write never corrupts the existing file.
func saveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
