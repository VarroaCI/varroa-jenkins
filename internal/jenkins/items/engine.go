package items

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

// managedItemsFile is the default path to the managed-items cache on the
// Jenkins PVC.
const managedItemsFile = "/var/jenkins_home/varroa-mite/managed-items.json"

// DeferredDeletion records an item whose deletion was deferred because a build
// is running.
type DeferredDeletion struct {
	Path   string
	Reason string
}

// ApplyResult holds the outcome of an Apply call.
type ApplyResult struct {
	DeferredDeletions []DeferredDeletion
	Written           int
	Skipped           int
}

// Engine manages Jenkins items based on items.yaml declarations.
type Engine struct {
	client *jenkins.Client
	// managedFile is the managed-items cache path; defaults to
	// managedItemsFile and is overridable in tests.
	managedFile string
}

// NewEngine creates a new items engine.
func NewEngine(client *jenkins.Client) *Engine {
	return &Engine{client: client, managedFile: managedItemsFile}
}

// Apply processes an items YAML manifest and reconciles Jenkins items.
func (e *Engine) Apply(ctx context.Context, yamlContent string) (*ApplyResult, error) {
	manifest, err := Parse(yamlContent)
	if err != nil {
		return nil, fmt.Errorf("parse items: %w", err)
	}

	// Auto-create root path if it doesn't exist.
	if manifest.Root != "" {
		if err := e.ensureRootPath(ctx, manifest.Root); err != nil {
			return nil, fmt.Errorf("ensure root path %s: %w", manifest.Root, err)
		}
	}

	strategy := manifest.EffectiveRemoveStrategy()

	return e.applyIncremental(ctx, manifest, strategy)
}

// ensureRootPath creates the root folder hierarchy if it doesn't exist.
func (e *Engine) ensureRootPath(ctx context.Context, root string) error {
	parts := strings.Split(strings.Trim(root, "/"), "/")
	var currentPath string
	for _, part := range parts {
		if currentPath == "" {
			currentPath = part
		} else {
			currentPath = currentPath + "/" + part
		}
		_, exists, err := e.client.GetItemConfig(ctx, currentPath)
		if err != nil {
			return fmt.Errorf("check root %s: %w", currentPath, err)
		}
		if !exists {
			folderXML := `<?xml version="1.0" encoding="UTF-8"?><com.cloudbees.hudson.plugins.folder.Folder plugin="cloudbees-folder"><properties/></com.cloudbees.hudson.plugins.folder.Folder>`
			if err := e.client.CreateItem(ctx, currentPath, "com.cloudbees.hudson.plugins.folder.Folder", folderXML); err != nil {
				return fmt.Errorf("create root folder %s: %w", currentPath, err)
			}
		}
	}
	return nil
}

// applyIncremental walks each item, creates or updates, then handles remove.
// All strategies (NONE, SYNC, REMOVE_SUPPORTED, REMOVE_ALL) use create/update
// in place for declared items and remove only mite-managed items no longer
// declared. Declared items are NEVER delete-recreated.
func (e *Engine) applyIncremental(ctx context.Context, manifest *Manifest, strategy string) (*ApplyResult, error) {
	prev := e.loadManaged()
	next := make(map[string]string)
	res := &ApplyResult{}
	flattened := manifest.Flatten()

	for _, ip := range flattened {
		configXML, err := GenerateConfigXML(ip.Item)
		if err != nil {
			return nil, fmt.Errorf("generate config.xml for %s: %w", ip.Path, err)
		}
		h := hashConfig(configXML)

		_, exists, err := e.client.GetItemConfig(ctx, ip.Path)
		if err != nil {
			return nil, fmt.Errorf("check existence of %s: %w", ip.Path, err)
		}

		if exists && prev[ip.Path] == h {
			res.Skipped++
		} else if exists {
			if err := e.client.UpdateItemConfig(ctx, ip.Path, configXML); err != nil {
				return nil, fmt.Errorf("update %s: %w", ip.Path, err)
			}
			res.Written++
		} else {
			if err := e.client.CreateItem(ctx, ip.Path, ip.Item.JenkinsClass(), configXML); err != nil {
				return nil, fmt.Errorf("create %s: %w", ip.Path, err)
			}
			res.Written++
		}
		next[ip.Path] = h
	}

	// NONE: never remove anything.
	// SYNC/REMOVE_ALL/REMOVE_SUPPORTED: remove mite-managed items no longer
	// declared. Under the current cache-scoped implementation all three share
	// the same removal set (the managed cache only contains mite-authored
	// items, all of which are supported types). The distinction is preserved
	// so adding unmanaged-item listing later differentiates them.
	if strategy != string(RemoveNone) {
		if err := e.removeUndeclared(ctx, next, prev, res); err != nil {
			return nil, fmt.Errorf("remove undeclared: %w", err)
		}
	}

	e.saveManaged(next)
	return res, nil
}

// removeUndeclared deletes managed items not in the current manifest.
// It mutates next and res IN PLACE — do NOT copy next defensively or
// the retention behavior for deferred deletions will be silently dropped.
func (e *Engine) removeUndeclared(ctx context.Context, next, prev map[string]string, res *ApplyResult) error {
	for path := range prev {
		if _, declared := next[path]; declared {
			continue
		}
		building, err := e.client.IsItemBuilding(ctx, path)
		if building {
			reason := "build in progress"
			if err != nil {
				reason = "build-state unknown: " + err.Error()
			}
			res.DeferredDeletions = append(res.DeferredDeletions, DeferredDeletion{Path: path, Reason: reason})
			next[path] = prev[path]
			continue
		}
		if err := e.client.DeleteItem(ctx, path); err != nil {
			if errors.Is(err, jenkins.ErrItemNotFound) {
				continue
			}
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// loadManaged reads the managed-items cache from disk.
// Backward-compatible: tries map[string]string first, then legacy []string,
// then empty map.
func (e *Engine) loadManaged() map[string]string {
	data, err := os.ReadFile(e.managedFile)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		return m
	}
	var legacy []string
	if err := json.Unmarshal(data, &legacy); err == nil {
		m = make(map[string]string, len(legacy))
		for _, p := range legacy {
			m[p] = ""
		}
		return m
	}
	return map[string]string{}
}

// saveManaged writes the managed-items cache to disk.
func (e *Engine) saveManaged(m map[string]string) {
	if err := os.MkdirAll(filepath.Dir(e.managedFile), 0755); err != nil {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(e.managedFile, data, 0600)
}

// hashConfig returns the SHA-256 hex digest of configXML.
func hashConfig(xml string) string {
	sum := sha256.Sum256([]byte(xml))
	return hex.EncodeToString(sum[:])
}

// DropManaged removes a single path from the managed-items cache (no-op if
// absent). Called after an approved out-of-band delete so the path is not
// re-evaluated by the next removeUndeclared pass.
func (e *Engine) DropManaged(path string) error {
	m := e.loadManaged()
	if _, ok := m[path]; !ok {
		return nil
	}
	delete(m, path)
	e.saveManaged(m)
	return nil
}
