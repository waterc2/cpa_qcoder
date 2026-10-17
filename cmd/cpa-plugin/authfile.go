package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// deleteAuthFile removes a Qoder auth file after validating the path is a
// qoder auth file and does not escape via traversal.
func deleteAuthFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	slash := filepath.ToSlash(path)
	if strings.Contains(slash, "../") || strings.Contains(slash, "/..") {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	base := filepath.Base(path)
	if !isQoderAuthName(base) {
		return fmt.Errorf("refusing to delete non-qoder auth file: %s", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
