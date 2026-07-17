package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"agentfiles/internal/core"
)

var Version = "0.1.0-dev"

type optionValues struct {
	agents     []string
	selectors  []string
	ref        string
	path       string
	name       string
	revision   string
	from       string
	kind       string
	all        bool
	native     bool
	filesystem bool
	dryRun     bool
	apply      bool
	json       bool
	help       bool
}

type optionDefinition struct {
	name  string
	value bool
}

var optionDefinitions = map[string]optionDefinition{
	"-a": {name: "agent", value: true}, "--agent": {name: "agent", value: true},
	"-s": {name: "select", value: true}, "--select": {name: "select", value: true}, "--skill": {name: "select", value: true},
	"--ref": {name: "ref", value: true}, "--path": {name: "path", value: true}, "--name": {name: "name", value: true},
	"--revision": {name: "revision", value: true}, "--from": {name: "from", value: true}, "--kind": {name: "kind", value: true},
	"--all": {name: "all"}, "--native": {name: "native"}, "--filesystem": {name: "filesystem"},
	"--dry-run": {name: "dry-run"}, "--apply": {name: "apply"}, "--json": {name: "json"},
	"-h": {name: "help"}, "--help": {name: "help"},
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		writeHelp(stdout)
		return nil
	}
	command := args[0]
	remaining := args[1:]
	switch command {
	case "help", "-h", "--help":
		writeHelp(stdout)
		return nil
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, Version)
		return nil
	case "add", "install", "a":
		return runAdd(ctx, remaining, stdout, stderr)
	case "enable", "on":
		return runToggle(ctx, remaining, stdout, true)
	case "disable", "off":
		return runToggle(ctx, remaining, stdout, false)
	case "update", "upgrade":
		return runUpdate(ctx, remaining, stdout)
	case "rollback":
		return runRollback(remaining, stdout)
	case "remove", "rm", "uninstall":
		return runRemove(ctx, remaining, stdout)
	case "list", "ls":
		return runList(remaining, stdout, false)
	case "status":
		return runList(remaining, stdout, true)
	case "agents":
		return runAgents(remaining, stdout)
	case "doctor":
		return runDoctor(remaining, stdout)
	case "migrate":
		return runMigrate(remaining, stdout)
	default:
		return fmt.Errorf("unknown command %q; run `agentfiles help`", command)
	}
}

func openManager() (*core.Manager, error) {
	return core.OpenManager()
}

func runAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	positionals, options, err := parseOptions(args, "agent", "select", "ref", "path", "name", "all", "native", "filesystem", "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		writeAddHelp(stdout)
		return nil
	}
	if len(positionals) == 0 {
		return fmt.Errorf("add requires a source")
	}
	kind := core.KindSkill
	sourceIndex := 0
	if parsed, parseErr := core.ParseKind(positionals[0]); parseErr == nil {
		kind = parsed
		sourceIndex = 1
	}
	if sourceIndex >= len(positionals) {
		return fmt.Errorf("add %s requires a source", kind)
	}
	if len(positionals) != sourceIndex+1 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(positionals[sourceIndex+1:], " "))
	}
	if kind != core.KindExtension && (options.native || options.filesystem) {
		return fmt.Errorf("--native and --filesystem apply only to extensions")
	}
	if kind == core.KindExtension && !options.native && !options.filesystem {
		options.native = true
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	operations, err := manager.Add(ctx, core.AddRequest{
		Kind: kind, Source: positionals[sourceIndex], Ref: options.ref, Path: options.path, Name: options.name,
		Selectors: options.selectors, Agents: options.agents, All: options.all,
		Native: options.native, Filesystem: options.filesystem, DryRun: options.dryRun,
	})
	if err != nil {
		return err
	}
	if kind == core.KindPrompt && containsOption(options.agents, "codex") {
		fmt.Fprintln(stderr, "warning: Codex custom prompts are deprecated; prefer a skill for new reusable workflows")
	}
	return printOperations(stdout, operations, options.json)
}

func runToggle(ctx context.Context, args []string, stdout io.Writer, enable bool) error {
	positionals, options, err := parseOptions(args, "agent", "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		action := "disable"
		if enable {
			action = "enable"
		}
		fmt.Fprintf(stdout, "Usage: agentfiles %s <kind/name...> --agent <agent...> [--dry-run] [--json]\n", action)
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	request := core.ToggleRequest{Refs: positionals, Agents: options.agents, DryRun: options.dryRun}
	var operations []core.Operation
	if enable {
		operations, err = manager.Enable(ctx, request)
	} else {
		operations, err = manager.Disable(ctx, request)
	}
	if err != nil {
		return err
	}
	return printOperations(stdout, operations, options.json)
}

func runUpdate(ctx context.Context, args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles update [kind/name...] [--dry-run] [--json]")
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	operations, err := manager.Update(ctx, core.UpdateRequest{Refs: positionals, DryRun: options.dryRun})
	if err != nil {
		return err
	}
	return printOperations(stdout, operations, options.json)
}

func runRollback(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "revision", "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles rollback <kind/name> [--revision ID] [--dry-run] [--json]")
		return nil
	}
	if len(positionals) != 1 {
		return fmt.Errorf("rollback requires exactly one asset")
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	operations, err := manager.Rollback(core.RollbackRequest{Ref: positionals[0], Revision: options.revision, DryRun: options.dryRun})
	if err != nil {
		return err
	}
	return printOperations(stdout, operations, options.json)
}

func runRemove(ctx context.Context, args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles remove <kind/name...> [--dry-run] [--json]")
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	operations, err := manager.Remove(ctx, core.RemoveRequest{Refs: positionals, DryRun: options.dryRun})
	if err != nil {
		return err
	}
	return printOperations(stdout, operations, options.json)
}

type assetView struct {
	ID              string      `json:"id"`
	Kind            core.Kind   `json:"kind"`
	Shape           string      `json:"shape"`
	Source          core.Source `json:"source"`
	CurrentRevision string      `json:"currentRevision,omitempty"`
	RevisionCount   int         `json:"revisionCount"`
	EnabledFor      []string    `json:"enabledFor"`
	InstalledFor    []string    `json:"installedFor,omitempty"`
}

func runList(args []string, stdout io.Writer, includeStatus bool) error {
	positionals, options, err := parseOptions(args, "agent", "kind", "json", "help")
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return fmt.Errorf("list does not accept positional arguments")
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles list [--kind KIND] [--agent AGENT] [--json]")
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	state, err := manager.State()
	if err != nil {
		return err
	}
	var kindFilter core.Kind
	if options.kind != "" {
		kindFilter, err = core.ParseKind(options.kind)
		if err != nil {
			return err
		}
	}
	views := make([]assetView, 0)
	for _, asset := range state.Assets {
		if kindFilter != "" && asset.Kind != kindFilter {
			continue
		}
		if len(options.agents) > 0 && !matchesAgent(asset.EnabledFor, options.agents) {
			continue
		}
		views = append(views, assetView{
			ID: asset.ID, Kind: asset.Kind, Shape: string(asset.Shape), Source: asset.Source,
			CurrentRevision: asset.CurrentRevision, RevisionCount: len(asset.Revisions),
			EnabledFor: nonNilStrings(asset.EnabledFor), InstalledFor: nonNilStrings(asset.InstalledFor),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	if options.json {
		result := map[string]any{"assets": views}
		if includeStatus {
			findings, doctorErr := manager.Doctor()
			if doctorErr != nil {
				return doctorErr
			}
			result["findings"] = findings
		}
		return writeJSON(stdout, result)
	}
	if len(views) == 0 {
		fmt.Fprintln(stdout, "No managed assets.")
	} else {
		writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "ASSET\tREVISION\tENABLED FOR\tSOURCE")
		for _, view := range views {
			revision := view.CurrentRevision
			if revision == "" {
				revision = "native"
			}
			enabled := strings.Join(view.EnabledFor, ",")
			if enabled == "" {
				enabled = "disabled"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", view.ID, revision, enabled, view.Source.Location)
		}
		_ = writer.Flush()
	}
	if includeStatus {
		findings, err := manager.Doctor()
		if err != nil {
			return err
		}
		printFindings(stdout, findings)
	}
	return nil
}

type agentView struct {
	Name       string `json:"name"`
	Display    string `json:"displayName"`
	Detected   bool   `json:"detected"`
	Skills     string `json:"skills,omitempty"`
	Prompts    string `json:"prompts,omitempty"`
	Extensions string `json:"extensions,omitempty"`
}

func runAgents(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "json", "help")
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return fmt.Errorf("agents does not accept positional arguments")
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles agents [--json]")
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	var views []agentView
	for _, name := range core.SortedProfileNames(manager.Profiles) {
		profile := manager.Profiles[name]
		views = append(views, agentView{
			Name: name, Display: profile.DisplayName, Detected: profile.Detected(),
			Skills: targetDescription(profile.Skills), Prompts: targetDescription(profile.Prompts), Extensions: targetDescription(profile.Extensions),
		})
	}
	if options.json {
		return writeJSON(stdout, views)
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "AGENT\tDETECTED\tSKILLS\tPROMPTS\tEXTENSIONS")
	for _, view := range views {
		fmt.Fprintf(writer, "%s\t%t\t%s\t%s\t%s\n", view.Name, view.Detected, emptyDash(view.Skills), emptyDash(view.Prompts), emptyDash(view.Extensions))
	}
	return writer.Flush()
}

func runDoctor(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "json", "help")
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return fmt.Errorf("doctor does not accept positional arguments")
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles doctor [--json]")
		return nil
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	findings, err := manager.Doctor()
	if err != nil {
		return err
	}
	if options.json {
		if err := writeJSON(stdout, findings); err != nil {
			return err
		}
	} else {
		printFindings(stdout, findings)
	}
	errorCount := 0
	for _, finding := range findings {
		if finding.Severity == "error" {
			errorCount++
		}
	}
	if errorCount > 0 {
		return fmt.Errorf("doctor found %d error(s)", errorCount)
	}
	return nil
}

func runMigrate(args []string, stdout io.Writer) error {
	positionals, options, err := parseOptions(args, "from", "agent", "apply", "dry-run", "json", "help")
	if err != nil {
		return err
	}
	if options.help {
		fmt.Fprintln(stdout, "Usage: agentfiles migrate skills [--from DIR] --agent AGENT... [--apply] [--json]")
		return nil
	}
	if len(positionals) != 1 || positionals[0] != "skills" {
		return fmt.Errorf("migrate currently supports exactly `migrate skills`")
	}
	if options.apply && options.dryRun {
		return fmt.Errorf("--apply and --dry-run are mutually exclusive")
	}
	manager, err := openManager()
	if err != nil {
		return err
	}
	operations, err := manager.MigrateSkills(core.MigrateRequest{From: options.from, Agents: options.agents, Apply: options.apply})
	if err != nil {
		return err
	}
	return printOperations(stdout, operations, options.json)
}

func parseOptions(args []string, allowedNames ...string) ([]string, optionValues, error) {
	allowed := map[string]struct{}{}
	for _, name := range allowedNames {
		allowed[name] = struct{}{}
	}
	var options optionValues
	var positionals []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals = append(positionals, argument)
			continue
		}
		flagName, inlineValue, hasInline := strings.Cut(argument, "=")
		definition, found := optionDefinitions[flagName]
		if !found {
			return nil, options, fmt.Errorf("unknown option %s", flagName)
		}
		if _, found := allowed[definition.name]; !found {
			return nil, options, fmt.Errorf("option %s is not valid for this command", flagName)
		}
		value := ""
		if definition.value {
			if hasInline {
				value = inlineValue
			} else {
				index++
				if index >= len(args) {
					return nil, options, fmt.Errorf("option %s requires a value", flagName)
				}
				value = args[index]
			}
			if value == "" {
				return nil, options, fmt.Errorf("option %s requires a non-empty value", flagName)
			}
		} else if hasInline {
			return nil, options, fmt.Errorf("boolean option %s does not take a value", flagName)
		}
		switch definition.name {
		case "agent":
			options.agents = append(options.agents, value)
		case "select":
			options.selectors = append(options.selectors, value)
		case "ref":
			options.ref = value
		case "path":
			options.path = value
		case "name":
			options.name = value
		case "revision":
			options.revision = value
		case "from":
			options.from = value
		case "kind":
			options.kind = value
		case "all":
			options.all = true
		case "native":
			options.native = true
		case "filesystem":
			options.filesystem = true
		case "dry-run":
			options.dryRun = true
		case "apply":
			options.apply = true
		case "json":
			options.json = true
		case "help":
			options.help = true
		}
	}
	return positionals, options, nil
}

func printOperations(writer io.Writer, operations []core.Operation, jsonOutput bool) error {
	if jsonOutput {
		if operations == nil {
			operations = []core.Operation{}
		}
		return writeJSON(writer, operations)
	}
	if len(operations) == 0 {
		fmt.Fprintln(writer, "Nothing to do.")
		return nil
	}
	for _, operation := range operations {
		marker := "-"
		if operation.Changed {
			marker = "+"
		}
		detail := operation.Asset
		if operation.Agent != "" {
			detail += " for " + operation.Agent
		}
		if operation.Revision != "" {
			detail += " @ " + operation.Revision
		}
		if operation.Target != "" {
			detail += " -> " + operation.Target
		}
		if operation.Message != "" {
			detail += " (" + oneLine(operation.Message) + ")"
		}
		fmt.Fprintf(writer, "%s %s %s\n", marker, operation.Action, detail)
	}
	return nil
}

func printFindings(writer io.Writer, findings []core.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(writer, "Doctor: healthy")
		return
	}
	for _, finding := range findings {
		scope := finding.Asset
		if finding.Agent != "" {
			if scope != "" {
				scope += "/"
			}
			scope += finding.Agent
		}
		if scope == "" {
			scope = finding.Path
		}
		fmt.Fprintf(writer, "%s %s %s: %s\n", strings.ToUpper(finding.Severity), finding.Code, scope, finding.Message)
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func matchesAgent(enabled, filters []string) bool {
	for _, filterGroup := range filters {
		for _, filter := range strings.Split(filterGroup, ",") {
			filter = core.CanonicalAgentName(filter)
			if filter == "all" || filter == "*" || core.Contains(enabled, filter) {
				return true
			}
		}
	}
	return false
}

func containsOption(values []string, expected string) bool {
	for _, group := range values {
		for _, value := range strings.Split(group, ",") {
			if strings.TrimSpace(value) == expected {
				return true
			}
		}
	}
	return false
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func targetDescription(target *core.Target) string {
	if target == nil {
		return ""
	}
	if target.IsNative() {
		return "native:" + target.NativeDriver
	}
	return target.Directory
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func writeHelp(writer io.Writer) {
	fmt.Fprint(writer, `agentfiles - local-first manager for agent skills, prompts, and extensions

Usage:
  agentfiles <command> [options]

Commands:
  add        Install into the central library and optionally enable per agent
  enable     Enable retained assets for selected agents
  disable    Disable assets without uninstalling central content
  update     Refresh assets from their tracked source
  rollback   Switch a filesystem asset to an earlier revision
  remove     Disable and move managed content to recoverable trash
  list       List desired state (alias: ls)
  status     List assets and report drift
  agents     Show built-in and custom agent profiles
  doctor     Check state, revisions, and generated links
  migrate    Adopt an existing ~/.agents/skills layout; dry-run by default
  version    Print the version

Run agentfiles <command> --help for command-specific usage.
`)
}

func writeAddHelp(writer io.Writer) {
	fmt.Fprint(writer, `Usage:
  agentfiles add [skill] <git-or-local-source> [--select NAME] [--agent AGENT]
  agentfiles add prompt <source> --path FILE [--name NAME] [--agent AGENT]
  agentfiles add extension <native-id-or-url> --native --agent AGENT
  agentfiles add extension <source> --filesystem [--path PATH] --agent AGENT

Options:
  -a, --agent AGENT       Agent profile; repeatable or comma-separated
  -s, --select NAME       Skill name/path; repeatable
      --all               Install all discovered skills
      --ref REF           Git branch, tag, or commit
      --path PATH         Relative path inside the source
      --name NAME         Name for prompt/extension assets
      --native            Delegate extension lifecycle to the agent CLI
      --filesystem        Snapshot and symlink a filesystem extension
      --dry-run           Resolve and validate without changing desired state
      --json              Machine-readable output
`)
}
