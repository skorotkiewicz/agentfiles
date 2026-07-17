package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AGENTFILES_HOME", filepath.Join(home, ".agents", "agentfiles"))
	paths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := LoadAgentProfiles(paths)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Paths: paths, Profiles: profiles, Runner: ExecRunner{}}, home
}

func writeSkill(t *testing.T, parent, name, body string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: Test " + name + " skill\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAddToggleUpdateRollbackSkill(t *testing.T) {
	manager, home := testManager(t)
	sourceParent := t.TempDir()
	source := writeSkill(t, sourceParent, "review-code", "Review the code.")

	operations, err := manager.Add(context.Background(), AddRequest{
		Kind: KindSkill, Source: source, Agents: []string{"codex", "claude-code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 {
		t.Fatalf("expected 3 operations, got %d", len(operations))
	}
	state, err := manager.State()
	if err != nil {
		t.Fatal(err)
	}
	asset := state.Assets["skill/review-code"]
	if asset == nil || len(asset.Revisions) != 1 {
		t.Fatalf("unexpected asset state: %#v", asset)
	}
	for _, link := range []string{
		filepath.Join(home, ".codex", "skills", "review-code"),
		filepath.Join(home, ".claude", "skills", "review-code"),
	} {
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected symlink %s: info=%v err=%v", link, info, err)
		}
	}

	if _, err := manager.Disable(context.Background(), ToggleRequest{Refs: []string{"review-code"}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".codex", "skills", "review-code")); !os.IsNotExist(err) {
		t.Fatalf("codex link should be absent, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "review-code")); err != nil {
		t.Fatalf("claude link should remain: %v", err)
	}

	file := filepath.Join(source, "SKILL.md")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(content, []byte("Updated.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	update, err := manager.Update(context.Background(), UpdateRequest{Refs: []string{"review-code"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(update) != 1 || !update[0].Changed {
		t.Fatalf("expected changed update, got %#v", update)
	}
	state, _ = manager.State()
	asset = state.Assets["skill/review-code"]
	if len(asset.Revisions) != 2 {
		t.Fatalf("expected two revisions, got %#v", asset.Revisions)
	}
	updatedRevision := asset.CurrentRevision
	rollback, err := manager.Rollback(RollbackRequest{Ref: "review-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rollback) != 1 || !rollback[0].Changed || rollback[0].Revision == updatedRevision {
		t.Fatalf("unexpected rollback: %#v", rollback)
	}
}

func TestAddRefusesUnmanagedTargetAndRollsBack(t *testing.T) {
	manager, home := testManager(t)
	source := writeSkill(t, t.TempDir(), "safe-skill", "Do a safe thing.")
	target := filepath.Join(home, ".codex", "skills", "safe-skill")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source, Agents: []string{"codex"}})
	if err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected unmanaged conflict, got %v", err)
	}
	state, stateErr := manager.State()
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if len(state.Assets) != 0 {
		t.Fatalf("failed add must not enter state: %#v", state.Assets)
	}
	if _, err := os.Stat(filepath.Join(manager.Paths.LibraryDir, "skills", "safe-skill")); !os.IsNotExist(err) {
		t.Fatalf("failed add must clean library: %v", err)
	}
}

func TestDiscoverSkillsValidatesFrontmatterAndSelection(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "skills"), "alpha", "Alpha.")
	writeSkill(t, filepath.Join(root, "nested", "skills"), "beta", "Beta.")
	candidates, err := DiscoverSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected two candidates, got %#v", candidates)
	}
	selected, err := SelectSkills(candidates, []string{"beta"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Name != "beta" {
		t.Fatalf("unexpected selection: %#v", selected)
	}
}

func TestAddRejectsDuplicateSelectedSkillNames(t *testing.T) {
	manager, _ := testManager(t)
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "one"), "duplicate", "First.")
	writeSkill(t, filepath.Join(root, "two"), "duplicate", "Second.")
	_, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: root, All: true})
	if err == nil || !strings.Contains(err.Error(), "multiple selected skills") {
		t.Fatalf("expected duplicate-name rejection, got %v", err)
	}
}

type fakeRunner struct {
	calls  []string
	failAt int
}

func (f *fakeRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	f.calls = append(f.calls, command+" "+strings.Join(args, " "))
	if f.failAt > 0 && len(f.calls) == f.failAt {
		return "", errors.New("injected runner failure")
	}
	return "ok", nil
}

func TestNativeExtensionDelegatesLifecycle(t *testing.T) {
	manager, _ := testManager(t)
	runner := &fakeRunner{}
	manager.Runner = runner
	_, err := manager.Add(context.Background(), AddRequest{
		Kind: KindExtension, Source: "formatter@team-market", Name: "formatter", Native: true,
		Agents: []string{"claude-code", "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected two installs, got %#v", runner.calls)
	}
	if _, err := manager.Disable(context.Background(), ToggleRequest{Refs: []string{"extension/formatter"}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	state, _ := manager.State()
	asset := state.Assets["extension/formatter"]
	if Contains(asset.EnabledFor, "codex") || Contains(asset.InstalledFor, "codex") {
		t.Fatalf("codex disable should remove native install: %#v", asset)
	}
	if !Contains(asset.EnabledFor, "claude-code") || !Contains(asset.InstalledFor, "claude-code") {
		t.Fatalf("claude install should remain: %#v", asset)
	}
}

func TestNativeDriversUseTargetedUpdateAndRef(t *testing.T) {
	runner := &fakeRunner{}
	if _, err := nativeUpdate(context.Background(), runner, "codex", "formatter@team-market", "formatter"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"codex plugin marketplace upgrade team-market",
		"codex plugin remove formatter@team-market",
		"codex plugin add formatter@team-market",
	}
	if strings.Join(runner.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected Codex update calls: %#v", runner.calls)
	}
	if _, err := nativeUpdate(context.Background(), runner, "codex", "formatter", "formatter"); err == nil || !strings.Contains(err.Error(), "plugin@marketplace") {
		t.Fatalf("expected marketplace-qualified update error, got %v", err)
	}
	runner.calls = nil
	if _, err := nativeInstall(context.Background(), runner, "gemini", "https://example.test/ext.git", "ext", "v2"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "gemini extensions install https://example.test/ext.git --ref v2" {
		t.Fatalf("unexpected Gemini install call: %#v", runner.calls)
	}
}

func TestContentDigestRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ContentDigest(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestRelativeSourceRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "prompt.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveRelativeSource(root, "escape/prompt.md"); err == nil || !strings.Contains(err.Error(), "traverses a symlink") {
		t.Fatalf("expected intermediate symlink rejection, got %v", err)
	}
}

func TestRemoveMovesManagedContentToTrash(t *testing.T) {
	manager, home := testManager(t)
	source := writeSkill(t, t.TempDir(), "retired-skill", "Retire this.")
	if _, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	operations, err := manager.Remove(context.Background(), RemoveRequest{Refs: []string{"retired-skill"}})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := manager.State()
	if len(state.Assets) != 0 {
		t.Fatalf("removed asset remains in state: %#v", state.Assets)
	}
	if _, err := os.Lstat(filepath.Join(home, ".codex", "skills", "retired-skill")); !os.IsNotExist(err) {
		t.Fatalf("activation link remains: %v", err)
	}
	var trash string
	for _, operation := range operations {
		if operation.Action == "remove" {
			trash = operation.Target
		}
	}
	if trash == "" {
		t.Fatalf("remove did not report trash location: %#v", operations)
	}
	if _, err := os.Stat(filepath.Join(trash, "asset.json")); err != nil {
		t.Fatalf("trash metadata missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(trash, "revisions")); err != nil {
		t.Fatalf("trash revisions missing: %v", err)
	}
}

func TestMigrateSkillsDryRunThenApply(t *testing.T) {
	manager, home := testManager(t)
	from := filepath.Join(home, ".agents", "skills")
	original := writeSkill(t, from, "legacy-skill", "Legacy instructions.")
	legacyLink := filepath.Join(home, ".codex", "skills", "legacy-skill")
	if err := os.MkdirAll(filepath.Dir(legacyLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, legacyLink); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.MigrateSkills(MigrateRequest{From: from, Agents: []string{"codex", "universal"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 {
		t.Fatalf("unexpected migration plan: %#v", plan)
	}
	if info, err := os.Lstat(original); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dry run changed original: info=%v err=%v", info, err)
	}
	if info, err := os.Lstat(legacyLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dry run changed legacy activation: info=%v err=%v", info, err)
	}
	state, _ := manager.State()
	if len(state.Assets) != 0 {
		t.Fatalf("dry run changed state: %#v", state.Assets)
	}
	if _, err := manager.MigrateSkills(MigrateRequest{From: from, Agents: []string{"codex", "universal"}, Apply: true}); err != nil {
		t.Fatal(err)
	}
	state, _ = manager.State()
	asset := state.Assets["skill/legacy-skill"]
	if asset == nil || asset.Source.Type != "snapshot" {
		t.Fatalf("unexpected migrated asset: %#v", asset)
	}
	for _, link := range []string{original, filepath.Join(home, ".codex", "skills", "legacy-skill")} {
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected migrated link %s: info=%v err=%v", link, info, err)
		}
		resolved, err := absoluteLinkTarget(link)
		if err != nil || resolved != filepath.Clean(manager.Paths.AssetCurrentPath(asset)) {
			t.Fatalf("migrated link %s points to %s, want manager current: %v", link, resolved, err)
		}
	}
}

func TestDoctorReportsMissingAndShadowLinks(t *testing.T) {
	manager, home := testManager(t)
	source := writeSkill(t, t.TempDir(), "diagnose-skill", "Diagnose.")
	if _, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	findings, err := manager.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("healthy install has findings: %#v", findings)
	}
	if err := os.Remove(filepath.Join(home, ".codex", "skills", "diagnose-skill")); err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(home, ".agents", "skills", "diagnose-skill")
	if err := os.MkdirAll(shadow, 0o755); err != nil {
		t.Fatal(err)
	}
	findings, err = manager.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["missing-link"] || !codes["shadow-copy"] {
		t.Fatalf("expected missing-link and shadow-copy, got %#v", findings)
	}
}

func TestSharedAgentTargetUsesReferenceCounting(t *testing.T) {
	manager, home := testManager(t)
	sharedDirectory := filepath.Join(home, ".shared-agent", "skills")
	if err := os.MkdirAll(sharedDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasedDirectory := filepath.Join(home, ".aliased-agent", "skills")
	if err := os.MkdirAll(filepath.Dir(aliasedDirectory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sharedDirectory, aliasedDirectory); err != nil {
		t.Fatal(err)
	}
	manager.Profiles["shared-a"] = AgentProfile{Name: "shared-a", DisplayName: "Shared A", Skills: &Target{Directory: sharedDirectory, Shape: ShapeDirectory}}
	manager.Profiles["shared-b"] = AgentProfile{Name: "shared-b", DisplayName: "Shared B", Skills: &Target{Directory: aliasedDirectory, Shape: ShapeDirectory}}
	source := writeSkill(t, t.TempDir(), "shared-skill", "Share this.")
	if _, err := manager.Add(context.Background(), AddRequest{
		Kind: KindSkill, Source: source, Agents: []string{"shared-a", "shared-b"},
	}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sharedDirectory, "shared-skill")
	if _, err := manager.Disable(context.Background(), ToggleRequest{Refs: []string{"shared-skill"}, Agents: []string{"shared-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("shared link was removed while universal still needs it: %v", err)
	}
	state, err := manager.State()
	if err != nil {
		t.Fatal(err)
	}
	asset := state.Assets["skill/shared-skill"]
	if Contains(asset.EnabledFor, "shared-a") || !Contains(asset.EnabledFor, "shared-b") {
		t.Fatalf("unexpected shared activation state: %#v", asset.EnabledFor)
	}
	if _, err := manager.Disable(context.Background(), ToggleRequest{Refs: []string{"shared-skill"}, Agents: []string{"shared-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("last disable should remove shared link: %v", err)
	}
}

func TestToggleRollsBackEarlierLinksOnConflict(t *testing.T) {
	manager, home := testManager(t)
	source := writeSkill(t, t.TempDir(), "toggle-safe", "Toggle safely.")
	if _, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source}); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(home, ".codex", "skills", "toggle-safe")
	if err := os.MkdirAll(filepath.Dir(conflict), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("unmanaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Enable(context.Background(), ToggleRequest{
		Refs: []string{"toggle-safe"}, Agents: []string{"claude-code", "codex"},
	})
	if err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected target conflict, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "toggle-safe")); !os.IsNotExist(err) {
		t.Fatalf("earlier link should have been rolled back: %v", err)
	}
	state, stateErr := manager.State()
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if enabled := state.Assets["skill/toggle-safe"].EnabledFor; len(enabled) != 0 {
		t.Fatalf("failed toggle changed desired state: %#v", enabled)
	}
}

func TestRemoveRestoresNativeActivationAfterLaterFailure(t *testing.T) {
	manager, _ := testManager(t)
	runner := &fakeRunner{failAt: 4}
	manager.Runner = runner
	if _, err := manager.Add(context.Background(), AddRequest{
		Kind: KindExtension, Source: "formatter@team-market", Name: "formatter", Native: true,
		Agents: []string{"claude-code", "codex"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove(context.Background(), RemoveRequest{Refs: []string{"formatter"}}); err == nil {
		t.Fatal("expected injected native remove failure")
	}
	state, err := manager.State()
	if err != nil {
		t.Fatal(err)
	}
	asset := state.Assets["extension/formatter"]
	if asset == nil || !Contains(asset.EnabledFor, "claude-code") || !Contains(asset.EnabledFor, "codex") {
		t.Fatalf("failed remove changed persisted state: %#v", asset)
	}
	if len(runner.calls) != 5 || !strings.Contains(runner.calls[4], "plugin enable") {
		t.Fatalf("expected best-effort native rollback, got %#v", runner.calls)
	}
}

func TestUpdateDryRunDoesNotAcquireOperationLock(t *testing.T) {
	manager, _ := testManager(t)
	source := writeSkill(t, t.TempDir(), "dry-update", "Dry update.")
	if _, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Paths.LockFile, []byte("pid=other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(context.Background(), UpdateRequest{Refs: []string{"dry-update"}, DryRun: true}); err != nil {
		t.Fatalf("dry update should ignore the mutation lock: %v", err)
	}
	if _, err := manager.Update(context.Background(), UpdateRequest{Refs: []string{"dry-update"}}); err == nil || !strings.Contains(err.Error(), "another agentfiles operation") {
		t.Fatalf("real update should honor the mutation lock: %v", err)
	}
}

func TestDoctorDetectsRevisionCorruption(t *testing.T) {
	manager, _ := testManager(t)
	source := writeSkill(t, t.TempDir(), "hash-check", "Hash this.")
	if _, err := manager.Add(context.Background(), AddRequest{Kind: KindSkill, Source: source}); err != nil {
		t.Fatal(err)
	}
	state, err := manager.State()
	if err != nil {
		t.Fatal(err)
	}
	asset := state.Assets["skill/hash-check"]
	file := filepath.Join(manager.Paths.RevisionPath(asset, asset.CurrentRevision), "SKILL.md")
	if err := os.WriteFile(file, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := manager.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range findings {
		found = found || finding.Code == "corrupt-revision"
	}
	if !found {
		t.Fatalf("expected corrupt-revision finding, got %#v", findings)
	}
}
