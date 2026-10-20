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
	// Failed lists each item that Jenkins refused. The walk continues past
	// a refused item, so one apply can report several failures.
	Failed []*ItemError
	// SaveError is the failure to write the managed-items cache after a
	// completed walk. The written items are live in Jenkins, but the next
	// apply writes every item again.
	SaveError error
}

// FailureText joins the refusals of Failed into one message, separated by "; ".
func (r *ApplyResult) FailureText() string {
	parts := make([]string, 0, len(r.Failed))
	for _, f := range r.Failed {
		parts = append(parts, f.Error())
	}
	return strings.Join(parts, "; ")
}

// Engine manages Jenkins items based on items.yaml declarations.
type Engine struct {
	client *jenkins.Client
	// managedFile is the path of the managed-items cache.
	managedFile string
}

// NewEngine creates an items engine that keeps its managed-items cache at managedFile.
func NewEngine(client *jenkins.Client, managedFile string) *Engine {
	return &Engine{client: client, managedFile: managedFile}
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

// The steps that fail an item apply. ItemError.Op holds one of them.
const (
	ItemOpCreate   = "create"
	ItemOpUpdate   = "update"
	ItemOpGenerate = "generate config.xml for"
	ItemOpCheck    = "check existence of"
)

// ItemError is the failure of one declared item. Path is the full item path.
// Op names the step that failed.
type ItemError struct {
	Op   string
	Path string
	Err  error
}

func (e *ItemError) Error() string { return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err) }

// Refusal reports whether the failed step judged the item itself. A check of
// existence fails on the connection instead, so it does not name the item.
func (e *ItemError) Refusal() bool {
	return e.Op == ItemOpCreate || e.Op == ItemOpUpdate || e.Op == ItemOpGenerate
}

// Unwrap returns the underlying error.
func (e *ItemError) Unwrap() error { return e.Err }

// applyIncremental walks each item, creates or updates, then handles remove.
// A failed cache save after a completed walk sets res.SaveError.
// All strategies (NONE, SYNC, REMOVE_SUPPORTED, REMOVE_ALL) use create/update
// in place for declared items and remove only mite-managed items no longer
// declared. Declared items are NEVER delete-recreated.
//
// A refused item (generate, create, or update) does not stop the walk. It is
// recorded in res.Failed, and it keeps its previous managed hash, so the remove
// step does not delete a live item. The descendants of a refused generate or
// create are skipped and keep their previous hash too, because the parent
// refusal covers them. A failed check of existence and a cancelled context abort the walk with
// an error, and nothing is saved.
func (e *Engine) applyIncremental(ctx context.Context, manifest *Manifest, strategy string) (*ApplyResult, error) {
	prev := e.loadManaged()
	next := make(map[string]string)
	res := &ApplyResult{}
	flattened := manifest.Flatten()
	var failedCreates []string

	for _, ip := range flattened {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if underFailedCreate(ip.Path, failedCreates) {
			keepPrevious(next, prev, ip.Path)
			continue
		}

		configXML, err := GenerateConfigXML(ip.Item)
		if err != nil {
			res.Failed = append(res.Failed, &ItemError{Op: ItemOpGenerate, Path: ip.Path, Err: err})
			failedCreates = append(failedCreates, ip.Path)
			keepPrevious(next, prev, ip.Path)
			continue
		}
		h := hashConfig(configXML)

		_, exists, err := e.client.GetItemConfig(ctx, ip.Path)
		if err != nil {
			return nil, &ItemError{Op: ItemOpCheck, Path: ip.Path, Err: err}
		}

		if exists && prev[ip.Path] == h {
			res.Skipped++
		} else if exists {
			if err := e.client.UpdateItemConfig(ctx, ip.Path, configXML); err != nil {
				res.Failed = append(res.Failed, &ItemError{Op: ItemOpUpdate, Path: ip.Path, Err: err})
				keepPrevious(next, prev, ip.Path)
				continue
			}
			res.Written++
		} else {
			if err := e.client.CreateItem(ctx, ip.Path, ip.Item.JenkinsClass(), configXML); err != nil {
				res.Failed = append(res.Failed, &ItemError{Op: ItemOpCreate, Path: ip.Path, Err: err})
				failedCreates = append(failedCreates, ip.Path)
				keepPrevious(next, prev, ip.Path)
				continue
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
	// A retained path is outside this apply: its managed items stay in the
	// cache with their previous hash, so the remove step below keeps them.
	for path, h := range prev {
		if _, declared := next[path]; !declared && manifest.retains(path) {
			next[path] = h
		}
	}

	if strategy != string(RemoveNone) {
		if err := e.removeUndeclared(ctx, next, prev, res); err != nil {
			return nil, fmt.Errorf("remove undeclared: %w", err)
		}
	}

	res.SaveError = e.saveManaged(next)
	return res, nil
}

// underFailedCreate reports whether path is a refused create or lies below one.
func underFailedCreate(path string, failedCreates []string) bool {
	for _, fc := range failedCreates {
		if path == fc || strings.HasPrefix(path, fc+"/") {
			return true
		}
	}
	return false
}

// keepPrevious copies the previous managed hash of path into next, when one
// exists. The remove step then keeps an item that the apply did not write.
func keepPrevious(next, prev map[string]string, path string) {
	if h, ok := prev[path]; ok {
		next[path] = h
	}
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

// saveManaged writes the managed-items cache to disk. It returns every
// failure, wrapped with the cache path.
func (e *Engine) saveManaged(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(e.managedFile), 0755); err != nil {
		return fmt.Errorf("save managed-items cache %s: %w", e.managedFile, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("save managed-items cache %s: %w", e.managedFile, err)
	}
	if err := os.WriteFile(e.managedFile, data, 0600); err != nil {
		return fmt.Errorf("save managed-items cache %s: %w", e.managedFile, err)
	}
	return nil
}

// hashConfig returns the SHA-256 hex digest of configXML.
func hashConfig(xml string) string {
	sum := sha256.Sum256([]byte(xml))
	return hex.EncodeToString(sum[:])
}
