package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Asset    string `json:"asset,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

func (m *Manager) Doctor() ([]Finding, error) {
	state, err := LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	findings := make([]Finding, 0)
	if data, lockErr := os.ReadFile(m.Paths.LockFile); lockErr == nil {
		findings = append(findings, Finding{Severity: "warning", Code: "operation-lock", Path: m.Paths.LockFile, Message: "an operation lock exists: " + strings.TrimSpace(string(data))})
	} else if !os.IsNotExist(lockErr) {
		findings = append(findings, Finding{Severity: "warning", Code: "unreadable-lock", Path: m.Paths.LockFile, Message: lockErr.Error()})
	}
	expectedLinks := map[string]string{}
	for _, asset := range sortedAssets(state) {
		if asset.Shape != ShapeNative {
			current := m.Paths.AssetCurrentPath(asset)
			if info, err := os.Lstat(current); err != nil {
				findings = append(findings, Finding{Severity: "error", Code: "missing-current", Asset: asset.ID, Path: current, Message: err.Error()})
			} else if info.Mode()&os.ModeSymlink == 0 {
				findings = append(findings, Finding{Severity: "error", Code: "current-not-symlink", Asset: asset.ID, Path: current, Message: "current revision pointer is not a symlink"})
			} else if _, err := os.Stat(current); err != nil {
				findings = append(findings, Finding{Severity: "error", Code: "broken-current", Asset: asset.ID, Path: current, Message: err.Error()})
			} else if linked, linkErr := absoluteLinkTarget(current); linkErr != nil {
				findings = append(findings, Finding{Severity: "error", Code: "unreadable-current", Asset: asset.ID, Path: current, Message: linkErr.Error()})
			} else if expected := filepath.Clean(revisionContentPath(m.Paths, asset, asset.CurrentRevision)); linked != expected {
				findings = append(findings, Finding{Severity: "error", Code: "wrong-current", Asset: asset.ID, Path: current, Message: fmt.Sprintf("points to %s; expected %s", linked, expected)})
			}
			for _, revision := range asset.Revisions {
				content := revisionContentPath(m.Paths, asset, revision.ID)
				if _, err := os.Stat(content); err != nil {
					findings = append(findings, Finding{Severity: "error", Code: "missing-revision", Asset: asset.ID, Path: content, Message: err.Error()})
					continue
				}
				digest, shape, err := ContentDigest(content)
				if err != nil {
					findings = append(findings, Finding{Severity: "error", Code: "unreadable-revision", Asset: asset.ID, Path: content, Message: err.Error()})
				} else if digestID := digest[:16]; shape != asset.Shape || digestID != revision.ID {
					findings = append(findings, Finding{Severity: "error", Code: "corrupt-revision", Asset: asset.ID, Path: content, Message: fmt.Sprintf("content digest is %s (%s), expected %s (%s)", digestID, shape, revision.ID, asset.Shape)})
				}
			}
		}
		for _, agentName := range asset.EnabledFor {
			profile, found := m.Profiles[agentName]
			if !found {
				findings = append(findings, Finding{Severity: "error", Code: "unknown-agent", Asset: asset.ID, Agent: agentName, Message: "enabled agent profile no longer exists"})
				continue
			}
			target := profile.Target(asset.Kind)
			if target == nil {
				findings = append(findings, Finding{Severity: "error", Code: "unsupported-agent-kind", Asset: asset.ID, Agent: agentName, Message: "agent profile no longer supports this asset kind"})
				continue
			}
			if target.IsNative() {
				if !Contains(asset.InstalledFor, agentName) {
					findings = append(findings, Finding{Severity: "warning", Code: "native-state-drift", Asset: asset.ID, Agent: agentName, Message: "extension is marked enabled but not installed"})
				}
				continue
			}
			path, err := TargetPath(profile, asset)
			if err != nil {
				findings = append(findings, Finding{Severity: "error", Code: "invalid-target", Asset: asset.ID, Agent: agentName, Message: err.Error()})
				continue
			}
			expectedLinks[targetIdentity(path)] = asset.ID
			if finding := checkManagedLink(m.Paths, path, asset); finding != nil {
				finding.Agent = agentName
				findings = append(findings, *finding)
			}
		}
		if asset.Kind == KindSkill && !Contains(asset.EnabledFor, "universal") {
			if universal, found := m.Profiles["universal"]; found && universal.Skills != nil {
				path, targetErr := TargetPath(universal, asset)
				if targetErr == nil {
					if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink == 0 {
						findings = append(findings, Finding{Severity: "warning", Code: "shadow-copy", Asset: asset.ID, Agent: "universal", Path: path, Message: "an unmanaged copy remains in the universal discovery directory"})
					}
				}
			}
		}
	}
	for _, profile := range m.Profiles {
		for _, kind := range []Kind{KindSkill, KindPrompt, KindExtension} {
			target := profile.Target(kind)
			if target == nil || target.IsNative() || target.Directory == "" {
				continue
			}
			entries, err := os.ReadDir(target.Directory)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				path := filepath.Join(target.Directory, entry.Name())
				if _, expected := expectedLinks[targetIdentity(path)]; expected {
					continue
				}
				info, err := os.Lstat(path)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					continue
				}
				linked, err := absoluteLinkTarget(path)
				if err == nil && pathWithin(m.Paths.Root, linked) {
					findings = append(findings, Finding{Severity: "warning", Code: "orphan-link", Agent: profile.Name, Path: path, Message: "agentfiles-owned-looking link is not present in desired state"})
				}
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		left := findings[i].Severity + findings[i].Code + findings[i].Asset + findings[i].Agent + findings[i].Path
		right := findings[j].Severity + findings[j].Code + findings[j].Asset + findings[j].Agent + findings[j].Path
		return left < right
	})
	return findings, nil
}

func checkManagedLink(paths Paths, path string, asset *Asset) *Finding {
	info, err := os.Lstat(path)
	if err != nil {
		return &Finding{Severity: "error", Code: "missing-link", Asset: asset.ID, Path: path, Message: err.Error()}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return &Finding{Severity: "error", Code: "target-conflict", Asset: asset.ID, Path: path, Message: "enabled target is not a symlink"}
	}
	linked, err := absoluteLinkTarget(path)
	if err != nil {
		return &Finding{Severity: "error", Code: "unreadable-link", Asset: asset.ID, Path: path, Message: err.Error()}
	}
	expected := filepath.Clean(paths.AssetCurrentPath(asset))
	if filepath.Clean(linked) != expected {
		return &Finding{Severity: "error", Code: "wrong-link", Asset: asset.ID, Path: path, Message: fmt.Sprintf("points to %s; expected %s", linked, expected)}
	}
	if _, err := os.Stat(path); err != nil {
		return &Finding{Severity: "error", Code: "broken-link", Asset: asset.ID, Path: path, Message: err.Error()}
	}
	return nil
}

func absoluteLinkTarget(path string) (string, error) {
	linked, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(linked) {
		linked = filepath.Join(filepath.Dir(path), linked)
	}
	return filepath.Clean(linked), nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
