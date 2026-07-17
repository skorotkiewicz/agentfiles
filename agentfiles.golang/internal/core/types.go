package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const StateSchemaVersion = 1

type Kind string

const (
	KindSkill     Kind = "skill"
	KindPrompt    Kind = "prompt"
	KindExtension Kind = "extension"
)

func ParseKind(value string) (Kind, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "skill", "skills":
		return KindSkill, nil
	case "prompt", "prompts", "command", "commands":
		return KindPrompt, nil
	case "extension", "extensions", "plugin", "plugins":
		return KindExtension, nil
	default:
		return "", fmt.Errorf("unknown asset kind %q (want skill, prompt, or extension)", value)
	}
}

func (k Kind) Plural() string {
	switch k {
	case KindSkill:
		return "skills"
	case KindPrompt:
		return "prompts"
	case KindExtension:
		return "extensions"
	default:
		return string(k) + "s"
	}
}

type AssetShape string

const (
	ShapeDirectory AssetShape = "directory"
	ShapeFile      AssetShape = "file"
	ShapeNative    AssetShape = "native"
)

type Source struct {
	Type     string `json:"type"`
	Location string `json:"location"`
	Ref      string `json:"ref,omitempty"`
	Subpath  string `json:"subpath,omitempty"`
}

type Revision struct {
	ID          string    `json:"id"`
	ResolvedRef string    `json:"resolvedRef,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
}

type Asset struct {
	ID              string     `json:"id"`
	Kind            Kind       `json:"kind"`
	Name            string     `json:"name"`
	Shape           AssetShape `json:"shape"`
	Suffix          string     `json:"suffix,omitempty"`
	Source          Source     `json:"source"`
	CurrentRevision string     `json:"currentRevision,omitempty"`
	Revisions       []Revision `json:"revisions,omitempty"`
	EnabledFor      []string   `json:"enabledFor,omitempty"`
	InstalledFor    []string   `json:"installedFor,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type State struct {
	Schema int               `json:"schema"`
	Assets map[string]*Asset `json:"assets"`
}

func NewState() *State {
	return &State{Schema: StateSchemaVersion, Assets: map[string]*Asset{}}
}

func AssetID(kind Kind, name string) string {
	return string(kind) + "/" + name
}

var safeNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var revisionIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func ValidateAssetName(kind Kind, name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("%s name must contain 1-64 characters", kind)
	}
	if kind == KindSkill {
		if !skillNamePattern.MatchString(name) {
			return fmt.Errorf("skill name %q must use lowercase letters, digits, and single hyphens", name)
		}
		return nil
	}
	if !safeNamePattern.MatchString(name) {
		return fmt.Errorf("%s name %q must use lowercase letters, digits, dots, underscores, or single separators", kind, name)
	}
	return nil
}

func (s *State) Validate() error {
	if s.Schema != StateSchemaVersion {
		return fmt.Errorf("unsupported state schema %d (this binary supports %d)", s.Schema, StateSchemaVersion)
	}
	if s.Assets == nil {
		return fmt.Errorf("state assets map is missing")
	}
	for id, asset := range s.Assets {
		if asset == nil {
			return fmt.Errorf("asset %q is null", id)
		}
		if asset.Kind != KindSkill && asset.Kind != KindPrompt && asset.Kind != KindExtension {
			return fmt.Errorf("asset %q has unknown kind %q", id, asset.Kind)
		}
		if err := ValidateAssetName(asset.Kind, asset.Name); err != nil {
			return fmt.Errorf("asset %q: %w", id, err)
		}
		if expected := AssetID(asset.Kind, asset.Name); id != expected || asset.ID != expected {
			return fmt.Errorf("asset key %q does not match id %q", id, expected)
		}
		if asset.Source.Location == "" {
			return fmt.Errorf("asset %q has no source location", id)
		}
		switch asset.Shape {
		case ShapeNative:
			if asset.Kind != KindExtension {
				return fmt.Errorf("asset %q: only extensions may use native shape", id)
			}
			if asset.CurrentRevision != "" || len(asset.Revisions) != 0 {
				return fmt.Errorf("native asset %q may not contain filesystem revisions", id)
			}
		case ShapeDirectory, ShapeFile:
			if asset.CurrentRevision == "" {
				return fmt.Errorf("asset %q has no current revision", id)
			}
			if asset.Shape == ShapeDirectory && asset.Suffix != "" {
				return fmt.Errorf("directory asset %q may not have a suffix", id)
			}
			if asset.Suffix != "" && !safeSuffixPattern.MatchString(asset.Suffix) {
				return fmt.Errorf("asset %q has unsafe suffix %q", id, asset.Suffix)
			}
			seenRevisions := map[string]struct{}{}
			currentFound := false
			for _, revision := range asset.Revisions {
				if !revisionIDPattern.MatchString(revision.ID) {
					return fmt.Errorf("asset %q has invalid revision id %q", id, revision.ID)
				}
				if _, duplicate := seenRevisions[revision.ID]; duplicate {
					return fmt.Errorf("asset %q repeats revision %q", id, revision.ID)
				}
				seenRevisions[revision.ID] = struct{}{}
				currentFound = currentFound || revision.ID == asset.CurrentRevision
			}
			if !currentFound {
				return fmt.Errorf("asset %q current revision %q is not in its revision history", id, asset.CurrentRevision)
			}
		default:
			return fmt.Errorf("asset %q has unknown shape %q", id, asset.Shape)
		}
	}
	return nil
}

func Contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func AddSorted(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range append(append([]string{}, values...), additions...) {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func RemoveValues(values []string, removals ...string) []string {
	remove := make(map[string]struct{}, len(removals))
	for _, value := range removals {
		remove[value] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, found := remove[value]; !found {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
