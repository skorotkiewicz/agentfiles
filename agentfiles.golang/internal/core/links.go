package core

import (
	"fmt"
	"os"
	"path/filepath"
)

func TargetPath(profile AgentProfile, asset *Asset) (string, error) {
	target := profile.Target(asset.Kind)
	if target == nil {
		return "", fmt.Errorf("agent %s does not support %ss", profile.Name, asset.Kind)
	}
	if target.IsNative() {
		return "", fmt.Errorf("agent %s manages %ss natively", profile.Name, asset.Kind)
	}
	if target.Directory == "" {
		return "", fmt.Errorf("agent %s has no %s target directory", profile.Name, asset.Kind)
	}
	if target.Shape != asset.Shape {
		return "", fmt.Errorf("agent %s expects %s %ss but %s is %s", profile.Name, target.Shape, asset.Kind, asset.ID, asset.Shape)
	}
	name := asset.Name
	if asset.Shape == ShapeFile {
		suffix := target.Suffix
		if suffix == "" {
			suffix = asset.Suffix
		}
		if target.Suffix != "" && asset.Suffix != target.Suffix {
			return "", fmt.Errorf("agent %s requires %s %ss, but %s uses %s", profile.Name, target.Suffix, asset.Kind, asset.ID, asset.Suffix)
		}
		name += suffix
	}
	return filepath.Join(target.Directory, name), nil
}

func EnableLink(paths Paths, profile AgentProfile, asset *Asset) (string, bool, error) {
	targetPath, err := TargetPath(profile, asset)
	if err != nil {
		return "", false, err
	}
	current := paths.AssetCurrentPath(asset)
	if _, err := os.Stat(current); err != nil {
		return "", false, fmt.Errorf("asset %s has no current content: %w", asset.ID, err)
	}
	if info, err := os.Lstat(targetPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return targetPath, false, fmt.Errorf("refuse to replace unmanaged path %s", targetPath)
		}
		linked, readErr := os.Readlink(targetPath)
		if readErr != nil {
			return targetPath, false, readErr
		}
		if !filepath.IsAbs(linked) {
			linked = filepath.Join(filepath.Dir(targetPath), linked)
		}
		if filepath.Clean(linked) == filepath.Clean(current) {
			return targetPath, false, nil
		}
		return targetPath, false, fmt.Errorf("refuse to replace foreign symlink %s -> %s", targetPath, linked)
	} else if !os.IsNotExist(err) {
		return targetPath, false, err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return targetPath, false, err
	}
	// Creating a symlink at its final name is already atomic and, unlike
	// rename, will not overwrite a path created by another process between the
	// ownership check and publication.
	if err := os.Symlink(current, targetPath); err != nil {
		if _, statErr := os.Lstat(targetPath); statErr == nil {
			if _, checkErr := CheckEnableTarget(paths, profile, asset); checkErr == nil {
				return targetPath, false, nil
			} else {
				return targetPath, false, checkErr
			}
		}
		return targetPath, false, err
	}
	return targetPath, true, nil
}

// CheckEnableTarget performs the ownership checks used by EnableLink without
// creating directories or links. It is used to keep dry-runs side-effect free
// while still reporting conflicts accurately.
func CheckEnableTarget(paths Paths, profile AgentProfile, asset *Asset) (string, error) {
	targetPath, err := TargetPath(profile, asset)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(targetPath)
	if os.IsNotExist(err) {
		return targetPath, nil
	}
	if err != nil {
		return targetPath, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return targetPath, fmt.Errorf("refuse to replace unmanaged path %s", targetPath)
	}
	linked, err := absoluteLinkTarget(targetPath)
	if err != nil {
		return targetPath, err
	}
	expected := filepath.Clean(paths.AssetCurrentPath(asset))
	if filepath.Clean(linked) != expected {
		return targetPath, fmt.Errorf("refuse to replace foreign symlink %s -> %s", targetPath, linked)
	}
	return targetPath, nil
}

func DisableLink(paths Paths, profile AgentProfile, asset *Asset) (string, bool, error) {
	targetPath, err := TargetPath(profile, asset)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(targetPath)
	if os.IsNotExist(err) {
		return targetPath, false, nil
	}
	if err != nil {
		return targetPath, false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return targetPath, false, fmt.Errorf("refuse to remove unmanaged path %s", targetPath)
	}
	linked, err := os.Readlink(targetPath)
	if err != nil {
		return targetPath, false, err
	}
	if !filepath.IsAbs(linked) {
		linked = filepath.Join(filepath.Dir(targetPath), linked)
	}
	expected := paths.AssetCurrentPath(asset)
	if filepath.Clean(linked) != filepath.Clean(expected) {
		return targetPath, false, fmt.Errorf("refuse to remove foreign symlink %s -> %s", targetPath, linked)
	}
	if err := os.Remove(targetPath); err != nil {
		return targetPath, false, err
	}
	return targetPath, true, nil
}

func targetIdentity(path string) string {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(path)))
}

func sameTargetPath(left, right string) bool {
	return targetIdentity(left) == targetIdentity(right)
}
