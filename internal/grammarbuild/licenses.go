package grammarbuild

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Preserve license/notice files from the verified upstream archive, including
// nested scanner dependencies. Archive extraction already rejects symlinks.
func exportLicenses(source, destination string) error {
	files, err := licenseFiles(source)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no upstream license files in %s", source)
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(source, rel))
		if err != nil {
			return err
		}
		path := filepath.Join(destination, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func licenseFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		isLicense := false
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			name := strings.ToUpper(part)
			if strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING") || strings.HasPrefix(name, "COPYRIGHT") || strings.HasPrefix(name, "NOTICE") {
				isLicense = true
			}
		}
		if !isLicense {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("license is not a regular file: %s", path)
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}
