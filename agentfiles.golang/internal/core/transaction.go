package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
)

// restoreActivations reconciles side effects back to a cloned pre-operation
// state. Filesystem assets are exact because agentfiles owns the links. Native
// extensions are best effort because their lifecycle is delegated to another
// process.
func (m *Manager) restoreActivations(ctx context.Context, current, desired *State, assetIDs []string) error {
	var failures []error
	for _, id := range assetIDs {
		desiredAsset := desired.Assets[id]
		if desiredAsset == nil {
			continue
		}
		currentAsset := current.Assets[id]
		currentEnabled := []string(nil)
		currentInstalled := []string(nil)
		if currentAsset != nil {
			currentEnabled = currentAsset.EnabledFor
			currentInstalled = currentAsset.InstalledFor
		}
		agents := AddSorted(
			append(append([]string{}, desiredAsset.EnabledFor...), desiredAsset.InstalledFor...),
			append(append([]string{}, currentEnabled...), currentInstalled...)...,
		)
		for _, agentName := range agents {
			profile, found := m.Profiles[agentName]
			if !found {
				failures = append(failures, fmt.Errorf("restore %s: unknown agent %s", id, agentName))
				continue
			}
			target := profile.Target(desiredAsset.Kind)
			if target == nil {
				failures = append(failures, fmt.Errorf("restore %s for %s: unsupported asset kind", id, agentName))
				continue
			}
			wantEnabled := Contains(desiredAsset.EnabledFor, agentName)
			haveEnabled := Contains(currentEnabled, agentName)
			if !target.IsNative() {
				if wantEnabled && !haveEnabled {
					if _, _, err := EnableLink(m.Paths, profile, desiredAsset); err != nil {
						failures = append(failures, fmt.Errorf("restore link for %s/%s: %w", id, agentName, err))
					}
				} else if !wantEnabled && haveEnabled {
					keepShared := false
					path, pathErr := TargetPath(profile, desiredAsset)
					if pathErr == nil {
						for _, desiredAgent := range desiredAsset.EnabledFor {
							if desiredAgent == agentName {
								continue
							}
							other, found := m.Profiles[desiredAgent]
							if !found || other.Target(desiredAsset.Kind) == nil || other.Target(desiredAsset.Kind).IsNative() {
								continue
							}
							otherPath, otherErr := TargetPath(other, desiredAsset)
							if otherErr == nil && sameTargetPath(otherPath, path) {
								keepShared = true
								break
							}
						}
					}
					if !keepShared {
						if _, _, err := DisableLink(m.Paths, profile, desiredAsset); err != nil {
							failures = append(failures, fmt.Errorf("remove restored link for %s/%s: %w", id, agentName, err))
						}
					}
				}
				continue
			}

			wantInstalled := Contains(desiredAsset.InstalledFor, agentName)
			haveInstalled := Contains(currentInstalled, agentName)
			if !wantInstalled {
				if haveInstalled || haveEnabled {
					if _, err := nativeUninstall(ctx, m.Runner, target.NativeDriver, desiredAsset.Source.Location, desiredAsset.Name); err != nil {
						failures = append(failures, fmt.Errorf("restore uninstall for %s/%s: %w", id, agentName, err))
					}
				}
				continue
			}
			if !haveInstalled {
				if _, err := nativeInstall(ctx, m.Runner, target.NativeDriver, desiredAsset.Source.Location, desiredAsset.Name, desiredAsset.Source.Ref); err != nil {
					failures = append(failures, fmt.Errorf("restore install for %s/%s: %w", id, agentName, err))
					continue
				}
				haveInstalled = true
				haveEnabled = true
			}
			if wantEnabled && !haveEnabled {
				if _, _, err := nativeEnable(ctx, m.Runner, target.NativeDriver, desiredAsset.Source.Location, desiredAsset.Name, desiredAsset.Source.Ref, haveInstalled); err != nil {
					failures = append(failures, fmt.Errorf("restore enable for %s/%s: %w", id, agentName, err))
				}
			} else if !wantEnabled && haveEnabled {
				if _, _, err := nativeDisable(ctx, m.Runner, target.NativeDriver, desiredAsset.Source.Location, desiredAsset.Name); err != nil {
					failures = append(failures, fmt.Errorf("restore disable for %s/%s: %w", id, agentName, err))
				}
			}
		}
	}
	return errors.Join(failures...)
}

func assetIDs(assets []*Asset) []string {
	ids := make([]string, 0, len(assets))
	for _, asset := range assets {
		ids = append(ids, asset.ID)
	}
	sort.Strings(ids)
	return ids
}

func withRollbackError(cause, rollback error) error {
	if rollback == nil {
		return cause
	}
	return fmt.Errorf("%w (rollback also failed: %v)", cause, rollback)
}

func (m *Manager) restoreFilesystemUpdates(current, desired *State, created map[string][]string) error {
	ids := make([]string, 0, len(created))
	for id := range created {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var failures []error
	for _, id := range ids {
		before := desired.Assets[id]
		after := current.Assets[id]
		if before == nil || after == nil {
			continue
		}
		if after.CurrentRevision != before.CurrentRevision && before.CurrentRevision != "" {
			if err := SwitchCurrent(m.Paths, before, before.CurrentRevision); err != nil {
				failures = append(failures, fmt.Errorf("restore current revision for %s: %w", id, err))
			}
		}
		for _, revision := range created[id] {
			if revisionKnown(before.Revisions, revision) {
				continue
			}
			if err := os.RemoveAll(m.Paths.RevisionPath(after, revision)); err != nil {
				failures = append(failures, fmt.Errorf("remove staged revision %s/%s: %w", id, revision, err))
			}
		}
	}
	return errors.Join(failures...)
}

func revisionKnown(revisions []Revision, id string) bool {
	for _, revision := range revisions {
		if revision.ID == id {
			return true
		}
	}
	return false
}
