package core

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type PreparedSource struct {
	Root        string
	Source      Source
	ResolvedRef string
	cleanup     func()
}

func (p *PreparedSource) Close() {
	if p != nil && p.cleanup != nil {
		p.cleanup()
	}
}

var githubShorthandPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:\.git)?$`)

func PrepareSource(ctx context.Context, paths Paths, location, ref string) (*PreparedSource, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, fmt.Errorf("source is required")
	}
	local := expandHome(location, paths.Home)
	info, localErr := os.Lstat(local)
	if localErr == nil {
		absolute, err := filepath.Abs(local)
		if err != nil {
			return nil, fmt.Errorf("resolve local source: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("local source %s is a symlink; package symlinks are rejected", absolute)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("local source %s is not a regular file or directory", absolute)
		}
		return &PreparedSource{
			Root:   absolute,
			Source: Source{Type: "local", Location: absolute, Ref: ref},
		}, nil
	}
	if !os.IsNotExist(localErr) {
		return nil, fmt.Errorf("inspect local source %s: %w", local, localErr)
	}

	gitURL := location
	if githubShorthandPattern.MatchString(location) {
		gitURL = "https://github.com/" + strings.TrimSuffix(location, ".git") + ".git"
	} else if !looksLikeGitURL(location) {
		return nil, fmt.Errorf("source %q is neither an existing local path nor a supported Git URL", location)
	}
	if strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("Git ref may not begin with a hyphen")
	}
	checkout, err := os.MkdirTemp("", "agentfiles-source-*")
	if err != nil {
		return nil, fmt.Errorf("create source staging directory: %w", err)
	}
	fail := func(err error) (*PreparedSource, error) {
		_ = os.RemoveAll(checkout)
		return nil, err
	}
	if ref == "" {
		if output, err := runGit(ctx, "clone", "--depth", "1", "--no-tags", "--", gitURL, checkout); err != nil {
			return fail(fmt.Errorf("clone %s: %w%s", gitURL, err, commandOutput(output)))
		}
	} else {
		if output, err := runGit(ctx, "clone", "--filter=blob:none", "--no-checkout", "--no-tags", "--", gitURL, checkout); err != nil {
			return fail(fmt.Errorf("clone %s: %w%s", gitURL, err, commandOutput(output)))
		}
		if output, err := runGit(ctx, "-C", checkout, "fetch", "--depth", "1", "origin", ref); err != nil {
			return fail(fmt.Errorf("fetch ref %s: %w%s", ref, err, commandOutput(output)))
		}
		if output, err := runGit(ctx, "-C", checkout, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return fail(fmt.Errorf("check out ref %s: %w%s", ref, err, commandOutput(output)))
		}
	}
	resolvedBytes, err := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", "HEAD").Output()
	if err != nil {
		return fail(fmt.Errorf("resolve cloned revision: %w", err))
	}
	return &PreparedSource{
		Root:        checkout,
		Source:      Source{Type: "git", Location: gitURL, Ref: ref},
		ResolvedRef: strings.TrimSpace(string(resolvedBytes)),
		cleanup:     func() { _ = os.RemoveAll(checkout) },
	}, nil
}

func looksLikeGitURL(value string) bool {
	return strings.HasPrefix(value, "https://") ||
		strings.HasPrefix(value, "http://") ||
		strings.HasPrefix(value, "ssh://") ||
		strings.HasPrefix(value, "git://") ||
		strings.HasPrefix(value, "git@") ||
		strings.HasPrefix(value, "file://")
}

func runGit(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func commandOutput(output string) string {
	if output == "" {
		return ""
	}
	return ": " + output
}

type Candidate struct {
	Name    string
	Path    string
	Subpath string
	Shape   AssetShape
	Suffix  string
}

func DiscoverSkills(root string) ([]Candidate, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill source %s must be a directory", root)
	}
	var candidates []Candidate
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root {
			switch entry.Name() {
			case ".git", ".hg", ".svn", "node_modules":
				return filepath.SkipDir
			}
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		dir := filepath.Dir(path)
		name, err := ValidateSkillFile(path, filepath.Base(dir))
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		if relative == "." {
			relative = ""
		}
		candidates = append(candidates, Candidate{
			Name: name, Path: dir, Subpath: filepath.ToSlash(relative), Shape: ShapeDirectory,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover skills in %s: %w", root, err)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Name == candidates[j].Name {
			return candidates[i].Subpath < candidates[j].Subpath
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates, nil
}

func ValidateSkillFile(path, directoryName string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return "", fmt.Errorf("%s must begin with YAML frontmatter", path)
	}
	var frontmatter []string
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			closed = true
			break
		}
		frontmatter = append(frontmatter, line)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if !closed {
		return "", fmt.Errorf("%s has unterminated YAML frontmatter", path)
	}
	name := ""
	description := ""
	descriptionFound := false
	for index := 0; index < len(frontmatter); index++ {
		line := frontmatter[index]
		key, value, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = unquoteYAMLScalar(strings.TrimSpace(value))
		case "description":
			descriptionFound = true
			description = unquoteYAMLScalar(strings.TrimSpace(value))
			if strings.HasPrefix(description, "|") || strings.HasPrefix(description, ">") {
				var block []string
				for next := index + 1; next < len(frontmatter); next++ {
					if frontmatter[next] != "" && !strings.HasPrefix(frontmatter[next], " ") && !strings.HasPrefix(frontmatter[next], "\t") {
						break
					}
					block = append(block, strings.TrimSpace(frontmatter[next]))
					index = next
				}
				description = strings.Join(block, "\n")
			}
		}
	}
	if name == "" {
		return "", fmt.Errorf("%s frontmatter is missing name", path)
	}
	if !descriptionFound || strings.TrimSpace(description) == "" {
		return "", fmt.Errorf("%s frontmatter description must be non-empty", path)
	}
	if utf8.RuneCountInString(description) > 1024 {
		return "", fmt.Errorf("%s frontmatter description exceeds 1024 characters", path)
	}
	if err := ValidateAssetName(KindSkill, name); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if name != directoryName {
		return "", fmt.Errorf("%s declares name %q but its directory is %q", path, name, directoryName)
	}
	return name, nil
}

func unquoteYAMLScalar(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func SelectSkills(candidates []Candidate, selectors []string, all bool) ([]Candidate, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no valid SKILL.md directories found")
	}
	if all {
		return candidates, nil
	}
	if len(selectors) == 0 {
		if len(candidates) == 1 {
			return candidates, nil
		}
		return nil, fmt.Errorf("source contains %d skills (%s); use --select NAME or --all", len(candidates), candidateNames(candidates))
	}
	selected := make([]Candidate, 0, len(selectors))
	seen := map[string]struct{}{}
	for _, selector := range selectors {
		selector = filepath.ToSlash(strings.TrimSpace(selector))
		matches := make([]Candidate, 0, 1)
		for _, candidate := range candidates {
			skillFile := strings.TrimSuffix(candidate.Subpath, "/") + "/SKILL.md"
			skillFile = strings.TrimPrefix(skillFile, "/")
			if selector == candidate.Name || selector == candidate.Subpath || selector == skillFile {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("skill selector %q was not found (available: %s)", selector, candidateNames(candidates))
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("skill selector %q is ambiguous; select by path", selector)
		}
		key := matches[0].Subpath
		if _, found := seen[key]; !found {
			seen[key] = struct{}{}
			selected = append(selected, matches[0])
		}
	}
	return selected, nil
}

func candidateNames(candidates []Candidate) string {
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		value := candidate.Name
		if candidate.Subpath != "" {
			value += "=" + candidate.Subpath
		}
		values = append(values, value)
	}
	return strings.Join(values, ", ")
}

func ValidateUniqueCandidateNames(candidates []Candidate) error {
	seen := map[string]string{}
	for _, candidate := range candidates {
		if previous, found := seen[candidate.Name]; found {
			return fmt.Errorf("source contains multiple selected skills named %q at %s and %s; select exactly one by path", candidate.Name, previous, candidate.Subpath)
		}
		seen[candidate.Name] = candidate.Subpath
	}
	return nil
}

func ResolveRelativeSource(root, subpath string) (string, string, error) {
	if strings.TrimSpace(subpath) == "" {
		return root, "", nil
	}
	if filepath.IsAbs(subpath) {
		return "", "", fmt.Errorf("--path must be relative to the source root")
	}
	clean := filepath.Clean(filepath.FromSlash(subpath))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("--path may not escape the source root")
	}
	resolved := filepath.Join(root, clean)
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("--path may not escape the source root")
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", "", fmt.Errorf("resolve --path %q: %w", subpath, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve source root symlinks: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve --path %q symlinks: %w", subpath, err)
	}
	expectedPath := filepath.Join(resolvedRoot, clean)
	if filepath.Clean(resolvedPath) != filepath.Clean(expectedPath) {
		return "", "", fmt.Errorf("--path %q traverses a symlink; package symlinks are rejected", subpath)
	}
	canonicalRelative, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || canonicalRelative == ".." || strings.HasPrefix(canonicalRelative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("--path may not escape the source root")
	}
	return resolved, filepath.ToSlash(clean), nil
}
