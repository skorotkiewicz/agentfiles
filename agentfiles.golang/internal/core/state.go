package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func LoadState(paths Paths) (*State, error) {
	data, err := os.ReadFile(paths.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(), nil
		}
		return nil, fmt.Errorf("read %s: %w", paths.StateFile, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.StateFile, err)
	}
	if err := state.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", paths.StateFile, err)
	}
	return &state, nil
}

func SaveState(paths Paths, state *State) error {
	if err := state.Validate(); err != nil {
		return fmt.Errorf("refuse to save invalid state: %w", err)
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	file, err := os.CreateTemp(paths.Root, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create state staging file: %w", err)
	}
	tempName := file.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	defer cleanup()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("set state permissions: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tempName, paths.StateFile); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	// The rename is already committed at this point. Sync the directory as a
	// best-effort durability step, but do not report a failure that callers
	// could mistakenly "roll back" after state.json has changed.
	if directory, openErr := os.Open(paths.Root); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func cloneState(state *State) (*State, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("clone desired state: %w", err)
	}
	var clone State
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, fmt.Errorf("clone desired state: %w", err)
	}
	return &clone, nil
}

type Lock struct {
	path    string
	content string
}

func AcquireLock(paths Paths) (*Lock, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(paths.LockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			data, _ := os.ReadFile(paths.LockFile)
			detail := strings.TrimSpace(string(data))
			if detail != "" {
				return nil, fmt.Errorf("another agentfiles operation holds %s (%s)", paths.LockFile, detail)
			}
			return nil, fmt.Errorf("another agentfiles operation holds %s", paths.LockFile)
		}
		return nil, fmt.Errorf("create operation lock: %w", err)
	}
	detail := "pid=" + strconv.Itoa(os.Getpid()) + " started=" + time.Now().UTC().Format(time.RFC3339) + "\n"
	if _, err := file.WriteString(detail); err != nil {
		_ = file.Close()
		_ = os.Remove(paths.LockFile)
		return nil, fmt.Errorf("write operation lock: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(paths.LockFile)
		return nil, fmt.Errorf("close operation lock: %w", err)
	}
	return &Lock{path: paths.LockFile, content: detail}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	data, readErr := os.ReadFile(l.path)
	if os.IsNotExist(readErr) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if string(data) != l.content {
		return fmt.Errorf("operation lock %s changed ownership; refusing to remove it", l.path)
	}
	err := os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
