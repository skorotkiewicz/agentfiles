package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Target struct {
	Directory    string     `json:"directory,omitempty"`
	Shape        AssetShape `json:"shape,omitempty"`
	Suffix       string     `json:"suffix,omitempty"`
	NativeDriver string     `json:"nativeDriver,omitempty"`
}

func (t *Target) IsNative() bool { return t != nil && t.NativeDriver != "" }

type AgentProfile struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Executable  string   `json:"executable,omitempty"`
	DetectDirs  []string `json:"detectDirs,omitempty"`
	Skills      *Target  `json:"skills,omitempty"`
	Prompts     *Target  `json:"prompts,omitempty"`
	Extensions  *Target  `json:"extensions,omitempty"`
}

type customConfig struct {
	Agents map[string]customAgent `json:"agents"`
}

type customAgent struct {
	DisplayName     string `json:"displayName,omitempty"`
	Executable      string `json:"executable,omitempty"`
	SkillsDir       string `json:"skillsDir,omitempty"`
	PromptsDir      string `json:"promptsDir,omitempty"`
	PromptSuffix    string `json:"promptSuffix,omitempty"`
	ExtensionsDir   string `json:"extensionsDir,omitempty"`
	ExtensionShape  string `json:"extensionShape,omitempty"`
	ExtensionSuffix string `json:"extensionSuffix,omitempty"`
}

var agentAliases = map[string]string{
	"claude":  "claude-code",
	"copilot": "github-copilot",
	"gemini":  "gemini-cli",
}

func CanonicalAgentName(name string) string {
	name = strings.TrimSpace(name)
	if canonical, found := agentAliases[name]; found {
		return canonical
	}
	return name
}

func LoadAgentProfiles(paths Paths) (map[string]AgentProfile, error) {
	profiles := builtInAgentProfiles(paths)
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return profiles, nil
		}
		return nil, fmt.Errorf("read %s: %w", paths.ConfigFile, err)
	}
	var config customConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.ConfigFile, err)
	}
	for name, custom := range config.Agents {
		if err := ValidateAssetName(KindExtension, name); err != nil {
			return nil, fmt.Errorf("custom agent %q: %w", name, err)
		}
		profile := profiles[name]
		profile.Name = name
		if custom.DisplayName != "" {
			profile.DisplayName = custom.DisplayName
		} else if profile.DisplayName == "" {
			profile.DisplayName = name
		}
		if custom.Executable != "" {
			profile.Executable = custom.Executable
		}
		if custom.SkillsDir != "" {
			directory := cleanTargetDir(custom.SkillsDir, paths.Home)
			profile.Skills = &Target{Directory: directory, Shape: ShapeDirectory}
			profile.DetectDirs = addUniquePath(profile.DetectDirs, directory)
		}
		if custom.PromptsDir != "" {
			suffix := custom.PromptSuffix
			if suffix == "" {
				suffix = ".md"
			}
			if !safeSuffixPattern.MatchString(suffix) {
				return nil, fmt.Errorf("custom agent %q has unsafe promptSuffix %q", name, suffix)
			}
			directory := cleanTargetDir(custom.PromptsDir, paths.Home)
			profile.Prompts = &Target{Directory: directory, Shape: ShapeFile, Suffix: suffix}
			profile.DetectDirs = addUniquePath(profile.DetectDirs, directory)
		}
		if custom.ExtensionsDir != "" {
			shape := ShapeDirectory
			if custom.ExtensionShape == string(ShapeFile) {
				shape = ShapeFile
			} else if custom.ExtensionShape != "" && custom.ExtensionShape != string(ShapeDirectory) {
				return nil, fmt.Errorf("custom agent %q has invalid extensionShape %q", name, custom.ExtensionShape)
			}
			if shape == ShapeDirectory && custom.ExtensionSuffix != "" {
				return nil, fmt.Errorf("custom agent %q may not set extensionSuffix for directory extensions", name)
			}
			if custom.ExtensionSuffix != "" && !safeSuffixPattern.MatchString(custom.ExtensionSuffix) {
				return nil, fmt.Errorf("custom agent %q has unsafe extensionSuffix %q", name, custom.ExtensionSuffix)
			}
			directory := cleanTargetDir(custom.ExtensionsDir, paths.Home)
			profile.Extensions = &Target{Directory: directory, Shape: shape, Suffix: custom.ExtensionSuffix}
			profile.DetectDirs = addUniquePath(profile.DetectDirs, directory)
		}
		profiles[name] = profile
	}
	return profiles, nil
}

func addUniquePath(paths []string, addition string) []string {
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(addition) {
			return paths
		}
	}
	return append(paths, addition)
}

func cleanTargetDir(path, home string) string {
	path = expandHome(path, home)
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	return filepath.Clean(path)
}

func builtInAgentProfiles(paths Paths) map[string]AgentProfile {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(paths.Home, ".codex")
	} else {
		codexHome = cleanTargetDir(codexHome, paths.Home)
	}
	claudeHome := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if claudeHome == "" {
		claudeHome = filepath.Join(paths.Home, ".claude")
	} else {
		claudeHome = cleanTargetDir(claudeHome, paths.Home)
	}
	geminiHome := filepath.Join(paths.Home, ".gemini")
	opencodeHome := filepath.Join(paths.XDGConfig, "opencode")
	return map[string]AgentProfile{
		"codex": {
			Name: "codex", DisplayName: "Codex", Executable: "codex", DetectDirs: []string{codexHome},
			Skills:     &Target{Directory: filepath.Join(codexHome, "skills"), Shape: ShapeDirectory},
			Prompts:    &Target{Directory: filepath.Join(codexHome, "prompts"), Shape: ShapeFile, Suffix: ".md"},
			Extensions: &Target{Shape: ShapeNative, NativeDriver: "codex"},
		},
		"claude-code": {
			Name: "claude-code", DisplayName: "Claude Code", Executable: "claude", DetectDirs: []string{claudeHome},
			Skills:     &Target{Directory: filepath.Join(claudeHome, "skills"), Shape: ShapeDirectory},
			Prompts:    &Target{Directory: filepath.Join(claudeHome, "commands"), Shape: ShapeFile, Suffix: ".md"},
			Extensions: &Target{Shape: ShapeNative, NativeDriver: "claude"},
		},
		"cursor": {
			Name: "cursor", DisplayName: "Cursor", Executable: "cursor-agent", DetectDirs: []string{filepath.Join(paths.Home, ".cursor")},
			Skills: &Target{Directory: filepath.Join(paths.Home, ".cursor", "skills"), Shape: ShapeDirectory},
		},
		"gemini-cli": {
			Name: "gemini-cli", DisplayName: "Gemini CLI", Executable: "gemini", DetectDirs: []string{geminiHome},
			Skills:     &Target{Directory: filepath.Join(geminiHome, "skills"), Shape: ShapeDirectory},
			Prompts:    &Target{Directory: filepath.Join(geminiHome, "commands"), Shape: ShapeFile, Suffix: ".toml"},
			Extensions: &Target{Shape: ShapeNative, NativeDriver: "gemini"},
		},
		"opencode": {
			Name: "opencode", DisplayName: "OpenCode", Executable: "opencode", DetectDirs: []string{opencodeHome},
			Skills:     &Target{Directory: filepath.Join(opencodeHome, "skills"), Shape: ShapeDirectory},
			Prompts:    &Target{Directory: filepath.Join(opencodeHome, "commands"), Shape: ShapeFile, Suffix: ".md"},
			Extensions: &Target{Directory: filepath.Join(opencodeHome, "plugins"), Shape: ShapeFile},
		},
		"github-copilot": {
			Name: "github-copilot", DisplayName: "GitHub Copilot", Executable: "copilot", DetectDirs: []string{filepath.Join(paths.Home, ".copilot")},
			Skills: &Target{Directory: filepath.Join(paths.Home, ".copilot", "skills"), Shape: ShapeDirectory},
		},
		"universal": {
			Name: "universal", DisplayName: "Universal (.agents)", DetectDirs: []string{filepath.Join(paths.Home, ".agents")},
			Skills: &Target{Directory: filepath.Join(paths.Home, ".agents", "skills"), Shape: ShapeDirectory},
		},
	}
}

func (p AgentProfile) Target(kind Kind) *Target {
	switch kind {
	case KindSkill:
		return p.Skills
	case KindPrompt:
		return p.Prompts
	case KindExtension:
		return p.Extensions
	default:
		return nil
	}
}

func (p AgentProfile) Detected() bool {
	for _, dir := range p.DetectDirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	if p.Executable != "" {
		_, err := exec.LookPath(p.Executable)
		return err == nil
	}
	return false
}

func SortedProfileNames(profiles map[string]AgentProfile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ResolveAgents(profiles map[string]AgentProfile, names []string, kind Kind) ([]AgentProfile, error) {
	seen := map[string]struct{}{}
	result := make([]AgentProfile, 0, len(names))
	for _, raw := range names {
		for _, name := range strings.Split(raw, ",") {
			name = CanonicalAgentName(name)
			if name == "" {
				continue
			}
			if name == "all" || name == "*" {
				for _, candidate := range SortedProfileNames(profiles) {
					profile := profiles[candidate]
					if profile.Target(kind) != nil && profile.Detected() {
						if _, found := seen[candidate]; !found {
							seen[candidate] = struct{}{}
							result = append(result, profile)
						}
					}
				}
				continue
			}
			profile, found := profiles[name]
			if !found {
				return nil, fmt.Errorf("unknown agent %q; run `agentfiles agents` to list profiles", name)
			}
			if profile.Target(kind) == nil {
				return nil, fmt.Errorf("agent %q does not support %ss", name, kind)
			}
			if _, found := seen[name]; !found {
				seen[name] = struct{}{}
				result = append(result, profile)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
