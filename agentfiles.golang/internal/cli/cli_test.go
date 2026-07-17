package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setTestEnvironment(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AGENTFILES_HOME", filepath.Join(home, ".agents", "agentfiles"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	return home
}

func testSkill(t *testing.T, name string) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "---\nname: " + name + "\ndescription: Exercise the CLI\n---\n\nDo the work.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "agentfiles <command>") || stderr.Len() != 0 {
		t.Fatalf("unexpected help output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestAddListDisableEndToEnd(t *testing.T) {
	home := setTestEnvironment(t)
	source := testSkill(t, "cli-skill")
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"add", source, "--agent", "claude-code", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var operations []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &operations); err != nil {
		t.Fatalf("add did not emit JSON: %v\n%s", err, stdout.String())
	}
	if len(operations) != 2 {
		t.Fatalf("unexpected add operations: %#v", operations)
	}
	link := filepath.Join(home, ".claude", "skills", "cli-skill")
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected activation link: info=%v err=%v", info, err)
	}

	stdout.Reset()
	if err := Run(context.Background(), []string{"list", "--agent=claude-code", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Assets []struct {
			ID string `json:"id"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Assets) != 1 || listing.Assets[0].ID != "skill/cli-skill" {
		t.Fatalf("unexpected listing: %#v", listing)
	}

	stdout.Reset()
	if err := Run(context.Background(), []string{"disable", "cli-skill", "--agent", "claude-code"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("disable left activation link behind: %v", err)
	}
	if !strings.Contains(stdout.String(), "disable skill/cli-skill") {
		t.Fatalf("unexpected disable output: %q", stdout.String())
	}
}

func TestPromptWarnsForDeprecatedCodexPrompt(t *testing.T) {
	setTestEnvironment(t)
	prompt := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(prompt, []byte("Review $ARGUMENTS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"add", "prompt", prompt, "--agent", "codex"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "deprecated") {
		t.Fatalf("expected Codex prompt warning, got %q", stderr.String())
	}
}

func TestDryRunLeavesManagerHomeAbsent(t *testing.T) {
	home := setTestEnvironment(t)
	source := testSkill(t, "planned-skill")
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"add", source, "--agent", "claude-code", "--dry-run"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	managerHome := filepath.Join(home, ".agents", "agentfiles")
	if _, err := os.Stat(managerHome); !os.IsNotExist(err) {
		t.Fatalf("dry-run created manager home: %v", err)
	}
	if !strings.Contains(stdout.String(), "would snapshot source") {
		t.Fatalf("unexpected dry-run output: %q", stdout.String())
	}
}

func TestDoctorJSONUsesEmptyArrayWhenHealthy(t *testing.T) {
	setTestEnvironment(t)
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("expected stable empty JSON array, got %q", stdout.String())
	}
}
