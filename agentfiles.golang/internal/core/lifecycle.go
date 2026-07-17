package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type RemoveRequest struct {
	Refs   []string
	DryRun bool
}

func (m *Manager) Remove(ctx context.Context, request RemoveRequest) ([]Operation, error) {
	if len(request.Refs) == 0 {
		return nil, fmt.Errorf("remove requires at least one asset")
	}
	if request.DryRun {
		state, err := LoadState(m.Paths)
		if err != nil {
			return nil, err
		}
		assets, err := resolveAssetRefs(state, request.Refs)
		if err != nil {
			return nil, err
		}
		var operations []Operation
		for _, asset := range assets {
			for _, agent := range asset.EnabledFor {
				operations = append(operations, Operation{Action: "disable", Asset: asset.ID, Agent: agent, Changed: true, Message: "dry run"})
			}
			for _, agent := range asset.InstalledFor {
				profile, found := m.Profiles[agent]
				if !found || profile.Target(asset.Kind) == nil || !profile.Target(asset.Kind).IsNative() {
					return nil, fmt.Errorf("cannot uninstall %s for unknown native agent %s", asset.ID, agent)
				}
				if Contains(asset.EnabledFor, agent) && profile.Target(asset.Kind).NativeDriver == "codex" {
					continue
				}
				operations = append(operations, Operation{Action: "uninstall", Asset: asset.ID, Agent: agent, Changed: true, Message: "dry run"})
			}
			operations = append(operations, Operation{Action: "remove", Asset: asset.ID, Changed: true, Message: "would move managed content to trash"})
		}
		return operations, nil
	}
	lock, err := AcquireLock(m.Paths)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	state, err := LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	assets, err := resolveAssetRefs(state, request.Refs)
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		for _, agentName := range AddSorted(asset.EnabledFor, asset.InstalledFor...) {
			profile, found := m.Profiles[agentName]
			if !found || profile.Target(asset.Kind) == nil {
				return nil, fmt.Errorf("cannot remove %s for unknown agent %s", asset.ID, agentName)
			}
			if Contains(asset.InstalledFor, agentName) && !profile.Target(asset.Kind).IsNative() {
				return nil, fmt.Errorf("cannot uninstall %s for non-native agent %s", asset.ID, agentName)
			}
		}
	}
	original, err := cloneState(state)
	if err != nil {
		return nil, err
	}
	trashRoot := filepath.Join(m.Paths.TrashDir, time.Now().UTC().Format("20060102T150405.000000000Z"))
	var operations []Operation
	type movedPath struct{ from, to string }
	var moved []movedPath
	rollback := func() error {
		var failures []error
		for index := len(moved) - 1; index >= 0; index-- {
			if err := os.MkdirAll(filepath.Dir(moved[index].from), 0o755); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := os.Rename(moved[index].to, moved[index].from); err != nil {
				failures = append(failures, err)
			}
		}
		if err := m.restoreActivations(ctx, state, original, assetIDs(assets)); err != nil {
			failures = append(failures, err)
		}
		if err := os.RemoveAll(trashRoot); err != nil {
			failures = append(failures, err)
		}
		return errors.Join(failures...)
	}
	for _, asset := range assets {
		for _, agentName := range append([]string{}, asset.EnabledFor...) {
			profile := m.Profiles[agentName]
			operation, err := m.disableOne(ctx, profile, asset)
			if err != nil {
				return nil, withRollbackError(err, rollback())
			}
			operations = append(operations, operation)
		}
		for _, agentName := range append([]string{}, asset.InstalledFor...) {
			profile := m.Profiles[agentName]
			output, err := nativeUninstall(ctx, m.Runner, profile.Target(asset.Kind).NativeDriver, asset.Source.Location, asset.Name)
			if err != nil {
				return nil, withRollbackError(fmt.Errorf("uninstall %s for %s: %w", asset.ID, agentName, err), rollback())
			}
			asset.InstalledFor = RemoveValues(asset.InstalledFor, agentName)
			operations = append(operations, Operation{Action: "uninstall", Asset: asset.ID, Agent: agentName, Changed: true, Message: output})
		}
		assetTrash := filepath.Join(trashRoot, asset.Kind.Plural(), asset.Name)
		if err := os.MkdirAll(assetTrash, 0o755); err != nil {
			return nil, withRollbackError(err, rollback())
		}
		if asset.Shape != ShapeNative {
			for _, pair := range []movedPath{
				{from: m.Paths.AssetLibraryDir(asset), to: filepath.Join(assetTrash, "library")},
				{from: m.Paths.AssetRevisionsDir(asset), to: filepath.Join(assetTrash, "revisions")},
			} {
				if _, err := os.Lstat(pair.from); os.IsNotExist(err) {
					continue
				} else if err != nil {
					return nil, withRollbackError(err, rollback())
				}
				if err := os.Rename(pair.from, pair.to); err != nil {
					return nil, withRollbackError(fmt.Errorf("move %s to trash: %w", pair.from, err), rollback())
				}
				moved = append(moved, pair)
			}
		}
		metadata, _ := json.MarshalIndent(asset, "", "  ")
		if err := os.WriteFile(filepath.Join(assetTrash, "asset.json"), append(metadata, '\n'), 0o600); err != nil {
			return nil, withRollbackError(err, rollback())
		}
		delete(state.Assets, asset.ID)
		operations = append(operations, Operation{Action: "remove", Asset: asset.ID, Target: assetTrash, Changed: true, Message: "recoverable trash"})
	}
	if err := SaveState(m.Paths, state); err != nil {
		return nil, withRollbackError(err, rollback())
	}
	return operations, nil
}

type MigrateRequest struct {
	From   string
	Agents []string
	Apply  bool
}

type vercelSkillLock struct {
	Skills map[string]struct {
		SourceURL string `json:"sourceUrl"`
		Ref       string `json:"ref"`
		SkillPath string `json:"skillPath"`
	} `json:"skills"`
}

type legacyActivation struct {
	path   string
	target string
}

func (m *Manager) MigrateSkills(request MigrateRequest) ([]Operation, error) {
	from := expandHome(request.From, m.Paths.Home)
	if from == "" {
		from = filepath.Join(m.Paths.Home, ".agents", "skills")
	}
	from, err := filepath.Abs(from)
	if err != nil {
		return nil, err
	}
	candidates, err := DiscoverSkills(from)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no skills found in %s", from)
	}
	if err := ValidateUniqueCandidateNames(candidates); err != nil {
		return nil, err
	}
	for left := range candidates {
		for right := range candidates {
			if left != right && pathWithin(candidates[left].Path, candidates[right].Path) {
				return nil, fmt.Errorf("migration source contains nested skills %s and %s; migrate them from separate roots", candidates[left].Name, candidates[right].Name)
			}
		}
	}
	profiles, err := ResolveAgents(m.Profiles, request.Agents, KindSkill)
	if err != nil {
		return nil, err
	}
	if request.Apply && len(profiles) == 0 {
		return nil, fmt.Errorf("migration apply requires at least one --agent")
	}
	legacyLinks, err := m.inspectMigrationTargets(candidates, profiles)
	if err != nil {
		return nil, err
	}
	lockMetadata := loadVercelSkillLock(filepath.Join(filepath.Dir(from), ".skill-lock.json"))
	state, err := LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		id := AssetID(KindSkill, candidate.Name)
		if _, found := state.Assets[id]; found {
			return nil, fmt.Errorf("asset %s is already managed", id)
		}
	}
	var operations []Operation
	for _, candidate := range candidates {
		operations = append(operations, Operation{Action: "migrate", Asset: AssetID(KindSkill, candidate.Name), Target: candidate.Path, Changed: true, Message: ternary(request.Apply, "apply", "dry run")})
		for _, profile := range profiles {
			operations = append(operations, Operation{Action: "enable", Asset: AssetID(KindSkill, candidate.Name), Agent: profile.Name, Changed: true, Message: ternary(request.Apply, "apply", "dry run")})
		}
	}
	if !request.Apply {
		return operations, nil
	}
	operationLock, err := AcquireLock(m.Paths)
	if err != nil {
		return nil, err
	}
	defer operationLock.Release()
	state, err = LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	legacyLinks, err = m.inspectMigrationTargets(candidates, profiles)
	if err != nil {
		return nil, err
	}
	trashRoot := filepath.Join(m.Paths.TrashDir, time.Now().UTC().Format("20060102T150405.000000000Z"), "migration-originals")
	type movedOriginal struct{ original, trash string }
	var moved []movedOriginal
	var removedLegacy []legacyActivation
	var linked []struct {
		profile AgentProfile
		asset   *Asset
	}
	var assets []*Asset
	rollback := func() {
		for index := len(linked) - 1; index >= 0; index-- {
			_, _, _ = DisableLink(m.Paths, linked[index].profile, linked[index].asset)
		}
		for index := len(moved) - 1; index >= 0; index-- {
			_ = os.MkdirAll(filepath.Dir(moved[index].original), 0o755)
			_ = os.Rename(moved[index].trash, moved[index].original)
		}
		for index := len(removedLegacy) - 1; index >= 0; index-- {
			_ = os.MkdirAll(filepath.Dir(removedLegacy[index].path), 0o755)
			_ = os.Symlink(removedLegacy[index].target, removedLegacy[index].path)
		}
		for _, asset := range assets {
			_ = os.RemoveAll(m.Paths.AssetLibraryDir(asset))
			_ = os.RemoveAll(m.Paths.AssetRevisionsDir(asset))
		}
	}
	for _, candidate := range candidates {
		now := time.Now().UTC()
		source := Source{Type: "snapshot", Location: candidate.Path}
		if metadata, found := lockMetadata[candidate.Name]; found && metadata.SourceURL != "" {
			subpath := filepath.ToSlash(filepath.Dir(filepath.FromSlash(metadata.SkillPath)))
			if subpath == "." {
				subpath = ""
			}
			source = Source{Type: "git", Location: metadata.SourceURL, Ref: metadata.Ref, Subpath: subpath}
		}
		asset := &Asset{
			ID: AssetID(KindSkill, candidate.Name), Kind: KindSkill, Name: candidate.Name,
			Shape: ShapeDirectory, Source: source, CreatedAt: now, UpdatedAt: now,
		}
		if _, found := state.Assets[asset.ID]; found {
			rollback()
			return nil, fmt.Errorf("asset %s became managed during migration", asset.ID)
		}
		if _, _, err := InstallRevision(m.Paths, asset, candidate.Path, ""); err != nil {
			rollback()
			return nil, fmt.Errorf("migrate %s: %w", asset.ID, err)
		}
		assets = append(assets, asset)
		originalTrash := filepath.Join(trashRoot, asset.Name)
		if err := os.MkdirAll(filepath.Dir(originalTrash), 0o755); err != nil {
			rollback()
			return nil, err
		}
		if err := os.Rename(candidate.Path, originalTrash); err != nil {
			rollback()
			return nil, fmt.Errorf("move original %s to migration trash: %w", candidate.Path, err)
		}
		moved = append(moved, movedOriginal{original: candidate.Path, trash: originalTrash})
		for _, legacy := range legacyLinks[asset.ID] {
			linked, readErr := os.Readlink(legacy.path)
			if readErr != nil || linked != legacy.target {
				rollback()
				if readErr != nil {
					return nil, fmt.Errorf("recheck legacy activation %s: %w", legacy.path, readErr)
				}
				return nil, fmt.Errorf("legacy activation %s changed during migration", legacy.path)
			}
			if err := os.Remove(legacy.path); err != nil {
				rollback()
				return nil, fmt.Errorf("remove legacy activation %s: %w", legacy.path, err)
			}
			removedLegacy = append(removedLegacy, legacy)
		}
		for _, profile := range profiles {
			target, created, err := EnableLink(m.Paths, profile, asset)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("enable migrated %s for %s: %w", asset.ID, profile.Name, err)
			}
			if created {
				linked = append(linked, struct {
					profile AgentProfile
					asset   *Asset
				}{profile: profile, asset: asset})
			}
			asset.EnabledFor = AddSorted(asset.EnabledFor, profile.Name)
			_ = target
		}
		state.Assets[asset.ID] = asset
	}
	if err := SaveState(m.Paths, state); err != nil {
		rollback()
		return nil, err
	}
	return operations, nil
}

func (m *Manager) inspectMigrationTargets(candidates []Candidate, profiles []AgentProfile) (map[string][]legacyActivation, error) {
	result := make(map[string][]legacyActivation)
	seenPaths := map[string]struct{}{}
	for _, candidate := range candidates {
		asset := &Asset{ID: AssetID(KindSkill, candidate.Name), Kind: KindSkill, Name: candidate.Name, Shape: ShapeDirectory}
		for _, profile := range profiles {
			target, err := TargetPath(profile, asset)
			if err != nil {
				return nil, err
			}
			info, err := os.Lstat(target)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect migration target %s: %w", target, err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				if targetIdentity(target) == filepath.Clean(candidate.Path) {
					continue
				}
				return nil, fmt.Errorf("refuse to replace unmanaged migration target %s", target)
			}
			linked, err := absoluteLinkTarget(target)
			if err != nil {
				return nil, err
			}
			if linked == filepath.Clean(m.Paths.AssetCurrentPath(asset)) {
				continue
			}
			if linked != filepath.Clean(candidate.Path) {
				return nil, fmt.Errorf("refuse to replace foreign migration symlink %s -> %s", target, linked)
			}
			identity := targetIdentity(target)
			if _, duplicate := seenPaths[identity]; duplicate {
				continue
			}
			raw, err := os.Readlink(target)
			if err != nil {
				return nil, err
			}
			seenPaths[identity] = struct{}{}
			result[asset.ID] = append(result[asset.ID], legacyActivation{path: target, target: raw})
		}
	}
	return result, nil
}

func loadVercelSkillLock(path string) map[string]struct {
	SourceURL string `json:"sourceUrl"`
	Ref       string `json:"ref"`
	SkillPath string `json:"skillPath"`
} {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lock vercelSkillLock
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	return lock.Skills
}

func ternary(condition bool, yes, no string) string {
	if condition {
		return yes
	}
	return no
}

func sortedAssets(state *State) []*Asset {
	assets := make([]*Asset, 0, len(state.Assets))
	for _, asset := range state.Assets {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })
	return assets
}
