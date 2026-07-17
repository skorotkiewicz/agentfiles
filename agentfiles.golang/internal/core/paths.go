package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Paths struct {
	Home         string
	XDGConfig    string
	Root         string
	StateFile    string
	ConfigFile   string
	LibraryDir   string
	RevisionsDir string
	TrashDir     string
	LockFile     string
}

func PathsFromEnv() (Paths, error) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve home directory: %w", err)
		}
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(xdg) {
		return Paths{}, fmt.Errorf("XDG_CONFIG_HOME must be absolute")
	}
	root := strings.TrimSpace(os.Getenv("AGENTFILES_HOME"))
	if root == "" {
		root = filepath.Join(home, ".agents", "agentfiles")
	} else if !filepath.IsAbs(root) {
		return Paths{}, fmt.Errorf("AGENTFILES_HOME must be absolute")
	}
	root = filepath.Clean(root)
	return Paths{
		Home:         home,
		XDGConfig:    filepath.Clean(xdg),
		Root:         root,
		StateFile:    filepath.Join(root, "state.json"),
		ConfigFile:   filepath.Join(root, "config.json"),
		LibraryDir:   filepath.Join(root, "library"),
		RevisionsDir: filepath.Join(root, "revisions"),
		TrashDir:     filepath.Join(root, "trash"),
		LockFile:     filepath.Join(root, ".lock"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{p.Root, p.LibraryDir, p.RevisionsDir, p.TrashDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func (p Paths) AssetLibraryDir(asset *Asset) string {
	return filepath.Join(p.LibraryDir, asset.Kind.Plural(), asset.Name)
}

func (p Paths) AssetCurrentPath(asset *Asset) string {
	return filepath.Join(p.AssetLibraryDir(asset), "current")
}

func (p Paths) AssetRevisionsDir(asset *Asset) string {
	return filepath.Join(p.RevisionsDir, asset.Kind.Plural(), asset.Name)
}

func (p Paths) RevisionPath(asset *Asset, revision string) string {
	return filepath.Join(p.AssetRevisionsDir(asset), revision)
}

func expandHome(path, home string) string {
	path = strings.ReplaceAll(path, "${HOME}", home)
	path = strings.ReplaceAll(path, "$HOME", home)
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}
