package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Manager struct {
	Paths    Paths
	Profiles map[string]AgentProfile
	Runner   CommandRunner
}

func OpenManager() (*Manager, error) {
	paths, err := PathsFromEnv()
	if err != nil {
		return nil, err
	}
	profiles, err := LoadAgentProfiles(paths)
	if err != nil {
		return nil, err
	}
	return &Manager{Paths: paths, Profiles: profiles, Runner: ExecRunner{}}, nil
}

type AddRequest struct {
	Kind       Kind
	Source     string
	Ref        string
	Path       string
	Name       string
	Selectors  []string
	Agents     []string
	All        bool
	Native     bool
	Filesystem bool
	DryRun     bool
}

type Operation struct {
	Action   string `json:"action"`
	Asset    string `json:"asset"`
	Agent    string `json:"agent,omitempty"`
	Target   string `json:"target,omitempty"`
	Revision string `json:"revision,omitempty"`
	Changed  bool   `json:"changed"`
	Message  string `json:"message,omitempty"`
}

func (m *Manager) Add(ctx context.Context, request AddRequest) ([]Operation, error) {
	if request.Kind == KindExtension && request.Native && request.Filesystem {
		return nil, fmt.Errorf("--native and --filesystem are mutually exclusive")
	}
	if request.Kind == KindExtension && request.Native {
		return m.addNative(ctx, request)
	}
	return m.addFilesystem(ctx, request)
}

func (m *Manager) addFilesystem(ctx context.Context, request AddRequest) ([]Operation, error) {
	profiles, err := ResolveAgents(m.Profiles, request.Agents, request.Kind)
	if err != nil {
		return nil, err
	}
	for _, profile := range profiles {
		if profile.Target(request.Kind).IsNative() {
			return nil, fmt.Errorf("agent %s uses a native %s manager; pass --native", profile.Name, request.Kind)
		}
	}
	prepared, err := PrepareSource(ctx, m.Paths, request.Source, request.Ref)
	if err != nil {
		return nil, err
	}
	defer prepared.Close()
	candidates, err := m.resolveCandidates(prepared, request)
	if err != nil {
		return nil, err
	}
	if err := ValidateUniqueCandidateNames(candidates); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	assets := make([]*Asset, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ValidateAssetName(request.Kind, candidate.Name); err != nil {
			return nil, err
		}
		asset := &Asset{
			ID: AssetID(request.Kind, candidate.Name), Kind: request.Kind, Name: candidate.Name,
			Shape: candidate.Shape, Suffix: candidate.Suffix,
			Source:    Source{Type: prepared.Source.Type, Location: prepared.Source.Location, Ref: prepared.Source.Ref, Subpath: candidate.Subpath},
			CreatedAt: now, UpdatedAt: now,
		}
		for _, profile := range profiles {
			if _, err := TargetPath(profile, asset); err != nil {
				return nil, err
			}
		}
		assets = append(assets, asset)
	}
	if request.DryRun {
		state, err := LoadState(m.Paths)
		if err != nil {
			return nil, err
		}
		operations := make([]Operation, 0, len(assets)*(len(profiles)+1))
		for _, asset := range assets {
			if _, found := state.Assets[asset.ID]; found {
				return nil, fmt.Errorf("asset %s is already installed; use `agentfiles update %s`", asset.ID, asset.ID)
			}
			operations = append(operations, Operation{Action: "add", Asset: asset.ID, Changed: true, Message: "would snapshot source"})
			for _, profile := range profiles {
				target, err := CheckEnableTarget(m.Paths, profile, asset)
				if err != nil {
					return nil, fmt.Errorf("enable %s for %s: %w", asset.ID, profile.Name, err)
				}
				operations = append(operations, Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Target: target, Changed: true, Message: "would create symlink"})
			}
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
	for _, asset := range assets {
		if _, found := state.Assets[asset.ID]; found {
			return nil, fmt.Errorf("asset %s is already installed; use `agentfiles update %s`", asset.ID, asset.ID)
		}
	}
	var operations []Operation
	var linked []struct {
		profile AgentProfile
		asset   *Asset
	}
	rollback := func() {
		for i := len(linked) - 1; i >= 0; i-- {
			_, _, _ = DisableLink(m.Paths, linked[i].profile, linked[i].asset)
		}
		for _, asset := range assets {
			_ = os.RemoveAll(m.Paths.AssetLibraryDir(asset))
			_ = os.RemoveAll(m.Paths.AssetRevisionsDir(asset))
		}
	}
	for index, asset := range assets {
		revision, _, err := InstallRevision(m.Paths, asset, candidates[index].Path, prepared.ResolvedRef)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("install %s: %w", asset.ID, err)
		}
		operations = append(operations, Operation{Action: "add", Asset: asset.ID, Revision: revision.ID, Changed: true})
		for _, profile := range profiles {
			target, created, err := EnableLink(m.Paths, profile, asset)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("enable %s for %s: %w", asset.ID, profile.Name, err)
			}
			if created {
				linked = append(linked, struct {
					profile AgentProfile
					asset   *Asset
				}{profile: profile, asset: asset})
			}
			asset.EnabledFor = AddSorted(asset.EnabledFor, profile.Name)
			message := ""
			if !created {
				message = "shared managed view already exists"
			}
			operations = append(operations, Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Target: target, Changed: true, Message: message})
		}
		state.Assets[asset.ID] = asset
	}
	if err := SaveState(m.Paths, state); err != nil {
		rollback()
		return nil, err
	}
	return operations, nil
}

func (m *Manager) resolveCandidates(prepared *PreparedSource, request AddRequest) ([]Candidate, error) {
	root := prepared.Root
	prefix := ""
	if request.Path != "" {
		resolved, relative, err := ResolveRelativeSource(prepared.Root, request.Path)
		if err != nil {
			return nil, err
		}
		root, prefix = resolved, relative
	}
	if request.Kind == KindSkill {
		if info, err := os.Stat(root); err == nil && !info.IsDir() {
			if filepath.Base(root) != "SKILL.md" {
				return nil, fmt.Errorf("skill --path must name a skill directory or SKILL.md")
			}
			root = filepath.Dir(root)
			prefix = filepath.ToSlash(filepath.Dir(filepath.FromSlash(prefix)))
			if prefix == "." {
				prefix = ""
			}
		}
		candidates, err := DiscoverSkills(root)
		if err != nil {
			return nil, err
		}
		for index := range candidates {
			candidates[index].Subpath = joinSlash(prefix, candidates[index].Subpath)
		}
		return SelectSkills(candidates, request.Selectors, request.All)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	shape := ShapeDirectory
	suffix := ""
	if info.Mode().IsRegular() {
		shape = ShapeFile
		suffix, err = safeSuffix(root)
		if err != nil {
			return nil, err
		}
	} else if !info.IsDir() {
		return nil, fmt.Errorf("source path %s is not a regular file or directory", root)
	}
	if request.Kind == KindPrompt && shape != ShapeFile {
		return nil, fmt.Errorf("prompt source must resolve to one file; use --path")
	}
	name := request.Name
	if name == "" {
		name = filepath.Base(root)
		if shape == ShapeFile {
			name = strings.TrimSuffix(name, filepath.Ext(name))
		}
	}
	if err := ValidateAssetName(request.Kind, name); err != nil {
		return nil, fmt.Errorf("%w; use --name to provide a portable name", err)
	}
	if request.Kind == KindExtension && !request.Filesystem {
		return nil, fmt.Errorf("filesystem extensions require --filesystem")
	}
	return []Candidate{{Name: name, Path: root, Subpath: prefix, Shape: shape, Suffix: suffix}}, nil
}

func joinSlash(left, right string) string {
	left = strings.Trim(left, "/")
	right = strings.Trim(right, "/")
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + "/" + right
}

func nativeName(source, explicit string) (string, error) {
	if explicit != "" {
		if err := ValidateAssetName(KindExtension, explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	name := source
	if slash := strings.LastIndex(strings.TrimRight(name, "/"), "/"); slash >= 0 {
		name = strings.TrimRight(name, "/")[slash+1:]
	}
	name = strings.TrimSuffix(name, ".git")
	if at := strings.Index(name, "@"); at > 0 {
		name = name[:at]
	}
	name = strings.ToLower(name)
	if err := ValidateAssetName(KindExtension, name); err != nil {
		return "", fmt.Errorf("cannot derive extension name from %q: %w; pass --name", source, err)
	}
	return name, nil
}

func (m *Manager) addNative(ctx context.Context, request AddRequest) ([]Operation, error) {
	profiles, err := ResolveAgents(m.Profiles, request.Agents, KindExtension)
	if err != nil {
		return nil, err
	}
	for _, profile := range profiles {
		if !profile.Extensions.IsNative() {
			return nil, fmt.Errorf("agent %s uses filesystem extensions; pass --filesystem", profile.Name)
		}
	}
	name, err := nativeName(request.Source, request.Name)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	asset := &Asset{
		ID: AssetID(KindExtension, name), Kind: KindExtension, Name: name, Shape: ShapeNative,
		Source: Source{Type: "native", Location: request.Source, Ref: request.Ref}, CreatedAt: now, UpdatedAt: now,
	}
	if request.DryRun {
		state, err := LoadState(m.Paths)
		if err != nil {
			return nil, err
		}
		if _, found := state.Assets[asset.ID]; found {
			return nil, fmt.Errorf("asset %s is already installed", asset.ID)
		}
		operations := []Operation{{Action: "add", Asset: asset.ID, Changed: true, Message: "would record native extension"}}
		for _, profile := range profiles {
			operations = append(operations, Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Changed: true, Message: "would invoke " + profile.Extensions.NativeDriver})
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
	if _, found := state.Assets[asset.ID]; found {
		return nil, fmt.Errorf("asset %s is already installed", asset.ID)
	}
	operations := []Operation{{Action: "add", Asset: asset.ID, Changed: true}}
	installed := make([]AgentProfile, 0, len(profiles))
	for _, profile := range profiles {
		output, err := nativeInstall(ctx, m.Runner, profile.Extensions.NativeDriver, request.Source, name, request.Ref)
		if err != nil {
			for index := len(installed) - 1; index >= 0; index-- {
				_, _ = nativeUninstall(ctx, m.Runner, installed[index].Extensions.NativeDriver, request.Source, name)
			}
			return nil, fmt.Errorf("install %s for %s: %w", asset.ID, profile.Name, err)
		}
		installed = append(installed, profile)
		asset.InstalledFor = AddSorted(asset.InstalledFor, profile.Name)
		asset.EnabledFor = AddSorted(asset.EnabledFor, profile.Name)
		operations = append(operations, Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Changed: true, Message: output})
	}
	state.Assets[asset.ID] = asset
	if err := SaveState(m.Paths, state); err != nil {
		for index := len(installed) - 1; index >= 0; index-- {
			_, _ = nativeUninstall(ctx, m.Runner, installed[index].Extensions.NativeDriver, request.Source, name)
		}
		return nil, err
	}
	return operations, nil
}

type ToggleRequest struct {
	Refs   []string
	Agents []string
	DryRun bool
}

func (m *Manager) Enable(ctx context.Context, request ToggleRequest) ([]Operation, error) {
	return m.toggle(ctx, request, true)
}

func (m *Manager) Disable(ctx context.Context, request ToggleRequest) ([]Operation, error) {
	return m.toggle(ctx, request, false)
}

func (m *Manager) toggle(ctx context.Context, request ToggleRequest, enable bool) ([]Operation, error) {
	if len(request.Refs) == 0 {
		return nil, fmt.Errorf("at least one asset is required")
	}
	if enable && len(request.Agents) == 0 {
		return nil, fmt.Errorf("enable requires at least one --agent")
	}
	if request.DryRun {
		state, err := LoadState(m.Paths)
		if err != nil {
			return nil, err
		}
		return m.planToggle(state, request, enable)
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
	original, err := cloneState(state)
	if err != nil {
		return nil, err
	}
	type toggleAction struct {
		asset   *Asset
		profile AgentProfile
	}
	var actions []toggleAction
	for _, asset := range assets {
		agentNames := request.Agents
		if !enable && len(agentNames) == 0 {
			agentNames = append([]string{}, asset.EnabledFor...)
		}
		profiles, err := ResolveAgents(m.Profiles, agentNames, asset.Kind)
		if err != nil {
			return nil, err
		}
		for _, profile := range profiles {
			actions = append(actions, toggleAction{asset: asset, profile: profile})
		}
	}
	var operations []Operation
	ids := assetIDs(assets)
	for _, action := range actions {
		var operation Operation
		if enable {
			operation, err = m.enableOne(ctx, action.profile, action.asset)
		} else {
			operation, err = m.disableOne(ctx, action.profile, action.asset)
		}
		if err != nil {
			return nil, withRollbackError(err, m.restoreActivations(ctx, state, original, ids))
		}
		operations = append(operations, operation)
	}
	for _, asset := range assets {
		asset.UpdatedAt = time.Now().UTC()
	}
	if err := SaveState(m.Paths, state); err != nil {
		return nil, withRollbackError(err, m.restoreActivations(ctx, state, original, ids))
	}
	return operations, nil
}

func (m *Manager) planToggle(state *State, request ToggleRequest, enable bool) ([]Operation, error) {
	assets, err := resolveAssetRefs(state, request.Refs)
	if err != nil {
		return nil, err
	}
	action := "disable"
	if enable {
		action = "enable"
	}
	var operations []Operation
	for _, asset := range assets {
		agents := request.Agents
		if !enable && len(agents) == 0 {
			agents = asset.EnabledFor
		}
		profiles, err := ResolveAgents(m.Profiles, agents, asset.Kind)
		if err != nil {
			return nil, err
		}
		for _, profile := range profiles {
			target := ""
			if !profile.Target(asset.Kind).IsNative() {
				target, _ = TargetPath(profile, asset)
			}
			operations = append(operations, Operation{Action: action, Asset: asset.ID, Agent: profile.Name, Target: target, Changed: enable != Contains(asset.EnabledFor, profile.Name), Message: "dry run"})
		}
	}
	return operations, nil
}

func (m *Manager) enableOne(ctx context.Context, profile AgentProfile, asset *Asset) (Operation, error) {
	target := profile.Target(asset.Kind)
	wasEnabled := Contains(asset.EnabledFor, profile.Name)
	if target.IsNative() {
		installed, output, err := nativeEnable(ctx, m.Runner, target.NativeDriver, asset.Source.Location, asset.Name, asset.Source.Ref, Contains(asset.InstalledFor, profile.Name))
		if err != nil {
			return Operation{}, fmt.Errorf("enable %s for %s: %w", asset.ID, profile.Name, err)
		}
		if installed {
			asset.InstalledFor = AddSorted(asset.InstalledFor, profile.Name)
		}
		asset.EnabledFor = AddSorted(asset.EnabledFor, profile.Name)
		return Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Changed: !wasEnabled, Message: output}, nil
	}
	path, created, err := EnableLink(m.Paths, profile, asset)
	if err != nil {
		return Operation{}, fmt.Errorf("enable %s for %s: %w", asset.ID, profile.Name, err)
	}
	asset.EnabledFor = AddSorted(asset.EnabledFor, profile.Name)
	message := ""
	if !created && !wasEnabled {
		message = "shared managed view already exists"
	}
	return Operation{Action: "enable", Asset: asset.ID, Agent: profile.Name, Target: path, Changed: !wasEnabled, Message: message}, nil
}

func (m *Manager) disableOne(ctx context.Context, profile AgentProfile, asset *Asset) (Operation, error) {
	target := profile.Target(asset.Kind)
	if !Contains(asset.EnabledFor, profile.Name) {
		return Operation{Action: "disable", Asset: asset.ID, Agent: profile.Name, Changed: false}, nil
	}
	if target.IsNative() {
		installed, output, err := nativeDisable(ctx, m.Runner, target.NativeDriver, asset.Source.Location, asset.Name)
		if err != nil {
			return Operation{}, fmt.Errorf("disable %s for %s: %w", asset.ID, profile.Name, err)
		}
		if !installed {
			asset.InstalledFor = RemoveValues(asset.InstalledFor, profile.Name)
		}
		asset.EnabledFor = RemoveValues(asset.EnabledFor, profile.Name)
		return Operation{Action: "disable", Asset: asset.ID, Agent: profile.Name, Changed: true, Message: output}, nil
	}
	targetPath, err := TargetPath(profile, asset)
	if err != nil {
		return Operation{}, fmt.Errorf("disable %s for %s: %w", asset.ID, profile.Name, err)
	}
	for _, otherAgent := range asset.EnabledFor {
		if otherAgent == profile.Name {
			continue
		}
		otherProfile, found := m.Profiles[otherAgent]
		if !found || otherProfile.Target(asset.Kind) == nil || otherProfile.Target(asset.Kind).IsNative() {
			continue
		}
		otherPath, pathErr := TargetPath(otherProfile, asset)
		if pathErr == nil && sameTargetPath(otherPath, targetPath) {
			asset.EnabledFor = RemoveValues(asset.EnabledFor, profile.Name)
			return Operation{Action: "disable", Asset: asset.ID, Agent: profile.Name, Target: targetPath, Changed: true, Message: "shared managed view retained for " + otherAgent}, nil
		}
	}
	path, removed, err := DisableLink(m.Paths, profile, asset)
	if err != nil {
		return Operation{}, fmt.Errorf("disable %s for %s: %w", asset.ID, profile.Name, err)
	}
	asset.EnabledFor = RemoveValues(asset.EnabledFor, profile.Name)
	message := ""
	if !removed {
		message = "managed view was already absent"
	}
	return Operation{Action: "disable", Asset: asset.ID, Agent: profile.Name, Target: path, Changed: true, Message: message}, nil
}

func resolveAssetRefs(state *State, refs []string) ([]*Asset, error) {
	seen := map[string]struct{}{}
	result := make([]*Asset, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if asset, found := state.Assets[ref]; found {
			if _, duplicate := seen[asset.ID]; !duplicate {
				seen[asset.ID] = struct{}{}
				result = append(result, asset)
			}
			continue
		}
		var matches []*Asset
		for _, asset := range state.Assets {
			if asset.Name == ref {
				matches = append(matches, asset)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("asset %q is not installed", ref)
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("asset name %q is ambiguous; use kind/name", ref)
		}
		if _, duplicate := seen[matches[0].ID]; !duplicate {
			seen[matches[0].ID] = struct{}{}
			result = append(result, matches[0])
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

type UpdateRequest struct {
	Refs   []string
	DryRun bool
}

func (m *Manager) Update(ctx context.Context, request UpdateRequest) ([]Operation, error) {
	var lock *Lock
	var err error
	if !request.DryRun {
		lock, err = AcquireLock(m.Paths)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
	}
	state, err := LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	var assets []*Asset
	if len(request.Refs) == 0 {
		for _, asset := range state.Assets {
			assets = append(assets, asset)
		}
		sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })
	} else {
		assets, err = resolveAssetRefs(state, request.Refs)
		if err != nil {
			return nil, err
		}
	}
	type filesystemUpdate struct {
		asset      *Asset
		prepared   *PreparedSource
		sourcePath string
		revisionID string
	}
	type nativeUpdatePlan struct {
		asset   *Asset
		profile AgentProfile
		agent   string
	}
	var filesystemPlans []filesystemUpdate
	var nativePlans []nativeUpdatePlan
	defer func() {
		for _, plan := range filesystemPlans {
			plan.prepared.Close()
		}
	}()
	for _, asset := range assets {
		if asset.Shape == ShapeNative {
			for _, agentName := range asset.InstalledFor {
				profile, found := m.Profiles[agentName]
				if !found || profile.Target(asset.Kind) == nil || !profile.Target(asset.Kind).IsNative() {
					return nil, fmt.Errorf("update %s: agent %s no longer has a native extension profile", asset.ID, agentName)
				}
				nativePlans = append(nativePlans, nativeUpdatePlan{asset: asset, profile: profile, agent: agentName})
			}
			continue
		}
		if asset.Source.Type == "snapshot" {
			return nil, fmt.Errorf("update %s: migrated snapshot has no refreshable source; add it again from a local path or Git source", asset.ID)
		}
		prepared, err := PrepareSource(ctx, m.Paths, asset.Source.Location, asset.Source.Ref)
		if err != nil {
			return nil, fmt.Errorf("update %s: %w", asset.ID, err)
		}
		sourcePath := prepared.Root
		if asset.Source.Subpath != "" {
			sourcePath, _, err = ResolveRelativeSource(prepared.Root, asset.Source.Subpath)
		}
		if err == nil && asset.Kind == KindSkill {
			_, err = ValidateSkillFile(filepath.Join(sourcePath, "SKILL.md"), asset.Name)
		}
		if err != nil {
			prepared.Close()
			return nil, fmt.Errorf("update %s: %w", asset.ID, err)
		}
		digest, shape, err := ContentDigest(sourcePath)
		if err != nil {
			prepared.Close()
			return nil, fmt.Errorf("update %s: %w", asset.ID, err)
		}
		if shape != asset.Shape {
			prepared.Close()
			return nil, fmt.Errorf("update %s changed shape from %s to %s", asset.ID, asset.Shape, shape)
		}
		filesystemPlans = append(filesystemPlans, filesystemUpdate{
			asset: asset, prepared: prepared, sourcePath: sourcePath, revisionID: digest[:16],
		})
	}
	if request.DryRun {
		operations := make([]Operation, 0, len(filesystemPlans)+len(nativePlans))
		for _, plan := range filesystemPlans {
			operations = append(operations, Operation{Action: "update", Asset: plan.asset.ID, Revision: plan.revisionID, Changed: plan.revisionID != plan.asset.CurrentRevision, Message: "dry run"})
		}
		for _, plan := range nativePlans {
			operations = append(operations, Operation{Action: "update", Asset: plan.asset.ID, Agent: plan.agent, Changed: true, Message: "would invoke native updater"})
		}
		return operations, nil
	}
	original, err := cloneState(state)
	if err != nil {
		return nil, err
	}
	created := make(map[string][]string, len(filesystemPlans))
	operations := make([]Operation, 0, len(filesystemPlans)+len(nativePlans))
	for _, plan := range filesystemPlans {
		created[plan.asset.ID] = nil
		previous := plan.asset.CurrentRevision
		revision, published, err := InstallRevision(m.Paths, plan.asset, plan.sourcePath, plan.prepared.ResolvedRef)
		if err != nil {
			return nil, withRollbackError(fmt.Errorf("update %s: %w", plan.asset.ID, err), m.restoreFilesystemUpdates(state, original, created))
		}
		if published {
			created[plan.asset.ID] = append(created[plan.asset.ID], revision.ID)
		}
		operations = append(operations, Operation{Action: "update", Asset: plan.asset.ID, Revision: revision.ID, Changed: revision.ID != previous})
	}
	nativeCompleted := 0
	for _, plan := range nativePlans {
		driver := plan.profile.Target(plan.asset.Kind).NativeDriver
		output, err := nativeUpdate(ctx, m.Runner, driver, plan.asset.Source.Location, plan.asset.Name)
		if err != nil {
			cause := fmt.Errorf("update %s for %s: %w", plan.asset.ID, plan.agent, err)
			if nativeCompleted > 0 {
				cause = fmt.Errorf("%w (some vendor-managed updates already completed)", cause)
			}
			return nil, withRollbackError(cause, m.restoreFilesystemUpdates(state, original, created))
		}
		nativeCompleted++
		plan.asset.UpdatedAt = time.Now().UTC()
		operations = append(operations, Operation{Action: "update", Asset: plan.asset.ID, Agent: plan.agent, Changed: true, Message: output})
	}
	if err := SaveState(m.Paths, state); err != nil {
		cause := err
		if nativeCompleted > 0 {
			cause = fmt.Errorf("%w (vendor-managed updates completed)", cause)
		}
		return nil, withRollbackError(cause, m.restoreFilesystemUpdates(state, original, created))
	}
	return operations, nil
}

type RollbackRequest struct {
	Ref      string
	Revision string
	DryRun   bool
}

func (m *Manager) Rollback(request RollbackRequest) ([]Operation, error) {
	var lock *Lock
	var err error
	if !request.DryRun {
		lock, err = AcquireLock(m.Paths)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
	}
	state, err := LoadState(m.Paths)
	if err != nil {
		return nil, err
	}
	assets, err := resolveAssetRefs(state, []string{request.Ref})
	if err != nil {
		return nil, err
	}
	asset := assets[0]
	if asset.Shape == ShapeNative {
		return nil, fmt.Errorf("native extension %s has no agentfiles revisions", asset.ID)
	}
	targetRevision := request.Revision
	if targetRevision == "" {
		for index := len(asset.Revisions) - 1; index >= 0; index-- {
			if asset.Revisions[index].ID != asset.CurrentRevision {
				targetRevision = asset.Revisions[index].ID
				break
			}
		}
	}
	if targetRevision == "" {
		return nil, fmt.Errorf("asset %s has no previous revision", asset.ID)
	}
	found := false
	for _, revision := range asset.Revisions {
		if revision.ID == targetRevision {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("asset %s has no revision %s", asset.ID, targetRevision)
	}
	content := revisionContentPath(m.Paths, asset, targetRevision)
	digest, shape, err := ContentDigest(content)
	if err != nil {
		return nil, fmt.Errorf("verify rollback revision %s: %w", targetRevision, err)
	}
	if shape != asset.Shape || digest[:16] != targetRevision {
		return nil, fmt.Errorf("rollback revision %s is corrupt; run `agentfiles doctor`", targetRevision)
	}
	operation := Operation{Action: "rollback", Asset: asset.ID, Revision: targetRevision, Changed: targetRevision != asset.CurrentRevision}
	if request.DryRun || !operation.Changed {
		return []Operation{operation}, nil
	}
	previousRevision := asset.CurrentRevision
	previousUpdated := asset.UpdatedAt
	if err := SwitchCurrent(m.Paths, asset, targetRevision); err != nil {
		return nil, err
	}
	asset.CurrentRevision = targetRevision
	asset.UpdatedAt = time.Now().UTC()
	if err := SaveState(m.Paths, state); err != nil {
		asset.CurrentRevision = previousRevision
		asset.UpdatedAt = previousUpdated
		return nil, withRollbackError(err, SwitchCurrent(m.Paths, asset, previousRevision))
	}
	return []Operation{operation}, nil
}

func (m *Manager) State() (*State, error) {
	return LoadState(m.Paths)
}

func MarshalIndented(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
