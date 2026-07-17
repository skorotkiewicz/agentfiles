package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func ContentDigest(source string) (string, AssetShape, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", "", err
	}
	hasher := sha256.New()
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("source %s is a symlink; symlinked package content is rejected by default", source)
	}
	if info.Mode().IsRegular() {
		// A single-file asset's source filename is adapter-specific metadata, not
		// package content. Hash it under a canonical name so stored snapshots can
		// be verified and renaming a source file does not create a new revision.
		if err := hashFile(hasher, source, "asset", info); err != nil {
			return "", "", err
		}
		return hex.EncodeToString(hasher.Sum(nil)), ShapeFile, nil
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("source %s is not a regular file or directory", source)
	}
	type entryInfo struct {
		path string
		rel  string
		info os.FileInfo
	}
	var entries []entryInfo
	err = filepath.Walk(source, func(path string, child os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if child.IsDir() && isVCSDirectory(child.Name()) {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if child.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("package contains symlink %s; symlinked package content is rejected by default", relative)
		}
		if !child.IsDir() && !child.Mode().IsRegular() {
			return fmt.Errorf("package contains unsupported file type %s", relative)
		}
		entries = append(entries, entryInfo{path: path, rel: filepath.ToSlash(relative), info: child})
		return nil
	})
	if err != nil {
		return "", "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	for _, entry := range entries {
		if entry.info.IsDir() {
			_, _ = io.WriteString(hasher, "dir\x00"+entry.rel+"\x00")
			continue
		}
		if err := hashFile(hasher, entry.path, entry.rel, entry.info); err != nil {
			return "", "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), ShapeDirectory, nil
}

func hashFile(hasher hash.Hash, path, relative string, info os.FileInfo) error {
	_, _ = io.WriteString(hasher, "file\x00"+relative+"\x00"+fmt.Sprintf("%04o", info.Mode().Perm())+"\x00")
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedInfo) || !os.SameFile(currentInfo, openedInfo) {
		return fmt.Errorf("source file %s changed while it was being read", path)
	}
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	_, _ = io.WriteString(hasher, "\x00")
	return nil
}

func CopyPackage(source, destination string, shape AssetShape, suffix string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	if shape == ShapeFile {
		return copyRegularFile(source, filepath.Join(destination, "asset"+suffix))
	}
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if info.IsDir() && isVCSDirectory(info.Name()) {
			return filepath.SkipDir
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to copy symlink %s", relative)
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to copy unsupported file %s", relative)
		}
		return copyRegularFile(path, target)
	})
}

func isVCSDirectory(name string) bool {
	switch name {
	case ".git", ".hg", ".svn":
		return true
	default:
		return false
	}
}

func copyRegularFile(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil {
		return err
	}
	currentInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedInfo) || !os.SameFile(currentInfo, openedInfo) {
		return fmt.Errorf("source file %s changed while it was being copied", source)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		_ = output.Close()
		if failed {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	failed = false
	return nil
}

func revisionContentPath(paths Paths, asset *Asset, revision string) string {
	container := paths.RevisionPath(asset, revision)
	if asset.Shape == ShapeFile {
		return filepath.Join(container, "asset"+asset.Suffix)
	}
	return container
}

func InstallRevision(paths Paths, asset *Asset, source, resolvedRef string) (Revision, bool, error) {
	digest, shape, err := ContentDigest(source)
	if err != nil {
		return Revision{}, false, fmt.Errorf("hash package: %w", err)
	}
	if asset.Shape != shape {
		return Revision{}, false, fmt.Errorf("asset %s expects %s content but source is %s", asset.ID, asset.Shape, shape)
	}
	revisionID := digest[:16]
	for _, revision := range asset.Revisions {
		if revision.ID == revisionID {
			storedDigest, storedShape, verifyErr := ContentDigest(revisionContentPath(paths, asset, revisionID))
			if verifyErr != nil {
				return Revision{}, false, fmt.Errorf("verify retained revision %s: %w", revisionID, verifyErr)
			}
			if storedShape != shape || storedDigest != digest {
				return Revision{}, false, fmt.Errorf("retained revision %s is corrupt; run `agentfiles doctor`", revisionID)
			}
			if asset.CurrentRevision != revisionID {
				if err := SwitchCurrent(paths, asset, revisionID); err != nil {
					return Revision{}, false, err
				}
				asset.CurrentRevision = revisionID
				asset.UpdatedAt = nowUTC()
			}
			return revision, false, nil
		}
	}
	parent := paths.AssetRevisionsDir(asset)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Revision{}, false, err
	}
	staging, err := os.MkdirTemp(parent, ".revision-*")
	if err != nil {
		return Revision{}, false, err
	}
	defer os.RemoveAll(staging)
	if err := CopyPackage(source, staging, shape, asset.Suffix); err != nil {
		return Revision{}, false, fmt.Errorf("copy package: %w", err)
	}
	stagedContent := staging
	if shape == ShapeFile {
		stagedContent = filepath.Join(staging, "asset"+asset.Suffix)
	}
	stagedDigest, stagedShape, err := ContentDigest(stagedContent)
	if err != nil {
		return Revision{}, false, fmt.Errorf("verify staged package: %w", err)
	}
	if stagedShape != shape || stagedDigest != digest {
		return Revision{}, false, fmt.Errorf("source changed while creating revision %s", revisionID)
	}
	destination := paths.RevisionPath(asset, revisionID)
	published := true
	if err := os.Rename(staging, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr != nil {
			return Revision{}, false, fmt.Errorf("publish revision: %w", err)
		}
		existingDigest, existingShape, digestErr := ContentDigest(revisionContentPath(paths, asset, revisionID))
		if digestErr != nil || existingShape != shape || existingDigest != digest {
			if digestErr != nil {
				return Revision{}, false, fmt.Errorf("validate existing revision %s: %w", revisionID, digestErr)
			}
			return Revision{}, false, fmt.Errorf("existing revision directory %s does not match digest %s", destination, revisionID)
		}
		published = false
	}
	revision := Revision{ID: revisionID, ResolvedRef: resolvedRef, InstalledAt: nowUTC()}
	previousRevisionCount := len(asset.Revisions)
	previousCurrent := asset.CurrentRevision
	previousUpdated := asset.UpdatedAt
	asset.Revisions = append(asset.Revisions, revision)
	asset.CurrentRevision = revisionID
	asset.UpdatedAt = revision.InstalledAt
	if err := SwitchCurrent(paths, asset, revisionID); err != nil {
		asset.Revisions = asset.Revisions[:previousRevisionCount]
		asset.CurrentRevision = previousCurrent
		asset.UpdatedAt = previousUpdated
		if published {
			_ = os.RemoveAll(destination)
		}
		return Revision{}, false, err
	}
	return revision, published, nil
}

func SwitchCurrent(paths Paths, asset *Asset, revision string) error {
	target := revisionContentPath(paths, asset, revision)
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("revision %s is unavailable: %w", revision, err)
	}
	library := paths.AssetLibraryDir(asset)
	if err := os.MkdirAll(library, 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(library, ".current-*")
	if err != nil {
		return err
	}
	temp := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Remove(temp); err != nil {
		return err
	}
	relative, err := filepath.Rel(library, target)
	if err != nil {
		return err
	}
	if err := os.Symlink(relative, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, paths.AssetCurrentPath(asset)); err != nil {
		return fmt.Errorf("switch current revision: %w", err)
	}
	return nil
}

func nowUTC() (resultTime time.Time) {
	return time.Now().UTC()
}

var safeSuffixPattern = regexp.MustCompile(`^\.[a-z0-9]+$`)

func safeSuffix(path string) (string, error) {
	suffix := strings.ToLower(filepath.Ext(path))
	if suffix == "" {
		return "", nil
	}
	if len(suffix) > 12 || !safeSuffixPattern.MatchString(suffix) {
		return "", fmt.Errorf("unsupported file suffix %q", suffix)
	}
	return suffix, nil
}
