package core

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", command, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", command, strings.Join(args, " "), err)
	}
	return text, nil
}

func nativeInstall(ctx context.Context, runner CommandRunner, driver, source, name, ref string) (string, error) {
	switch driver {
	case "codex":
		return runner.Run(ctx, "codex", "plugin", "add", source)
	case "claude":
		return runner.Run(ctx, "claude", "plugin", "install", source, "--scope", "user")
	case "gemini":
		args := []string{"extensions", "install", source}
		if ref != "" {
			args = append(args, "--ref", ref)
		}
		return runner.Run(ctx, "gemini", args...)
	default:
		return "", fmt.Errorf("unsupported native extension driver %q", driver)
	}
}

func nativeEnable(ctx context.Context, runner CommandRunner, driver, source, name, ref string, installed bool) (stillInstalled bool, output string, err error) {
	if !installed || driver == "codex" {
		output, err = nativeInstall(ctx, runner, driver, source, name, ref)
		return err == nil, output, err
	}
	switch driver {
	case "claude":
		output, err = runner.Run(ctx, "claude", "plugin", "enable", source, "--scope", "user")
	case "gemini":
		output, err = runner.Run(ctx, "gemini", "extensions", "enable", name, "--scope", "user")
	default:
		err = fmt.Errorf("unsupported native extension driver %q", driver)
	}
	return installed, output, err
}

func nativeDisable(ctx context.Context, runner CommandRunner, driver, source, name string) (stillInstalled bool, output string, err error) {
	switch driver {
	case "codex":
		output, err = runner.Run(ctx, "codex", "plugin", "remove", source)
		return false, output, err
	case "claude":
		output, err = runner.Run(ctx, "claude", "plugin", "disable", source, "--scope", "user")
		return true, output, err
	case "gemini":
		output, err = runner.Run(ctx, "gemini", "extensions", "disable", name, "--scope", "user")
		return true, output, err
	default:
		return true, "", fmt.Errorf("unsupported native extension driver %q", driver)
	}
}

func nativeUpdate(ctx context.Context, runner CommandRunner, driver, source, name string) (string, error) {
	switch driver {
	case "codex":
		separator := strings.LastIndex(source, "@")
		if separator <= 0 || separator == len(source)-1 {
			return "", fmt.Errorf("Codex plugin updates require a plugin@marketplace source (got %q)", source)
		}
		marketplace := source[separator+1:]
		if _, err := runner.Run(ctx, "codex", "plugin", "marketplace", "upgrade", marketplace); err != nil {
			return "", err
		}
		if _, err := runner.Run(ctx, "codex", "plugin", "remove", source); err != nil {
			return "", err
		}
		return runner.Run(ctx, "codex", "plugin", "add", source)
	case "claude":
		return runner.Run(ctx, "claude", "plugin", "update", source, "--scope", "user")
	case "gemini":
		return runner.Run(ctx, "gemini", "extensions", "update", name)
	default:
		return "", fmt.Errorf("unsupported native extension driver %q", driver)
	}
}

func nativeUninstall(ctx context.Context, runner CommandRunner, driver, source, name string) (string, error) {
	switch driver {
	case "codex":
		return runner.Run(ctx, "codex", "plugin", "remove", source)
	case "claude":
		return runner.Run(ctx, "claude", "plugin", "uninstall", source, "--scope", "user", "--keep-data", "--yes")
	case "gemini":
		return runner.Run(ctx, "gemini", "extensions", "uninstall", name)
	default:
		return "", fmt.Errorf("unsupported native extension driver %q", driver)
	}
}
