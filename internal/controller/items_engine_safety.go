package controller

import (
	"errors"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// errItemsRefused marks a refusal of resolved items content. Its text is the
// "assemble items" prefix, so a wrapped refusal keeps its message unchanged.
var errItemsRefused = errors.New("assemble items")

// branchBuildStrategiesPlugin supplies
// jenkins.branch.buildstrategies.basic.SkipInitialBuildOnFirstBranchIndexing,
// which the items engine renders into every multibranch item and
// organization folder item (design D12). The Varroa pushed-branch strategy
// comes from varroa-mite-auth, which every pod loads from the init container,
// so this check does not test for it.
const branchBuildStrategiesPlugin = "basic-branch-build-strategies"

// checkBranchBuildStrategiesPlugin refuses to assemble items when itemsYAML
// holds a multibranch item or an organization folder item and coreSet omits
// basic-branch-build-strategies. A missing class makes the item create fail,
// or makes Jenkins drop the buildStrategies element, so a silent omission
// would produce an initial-index build storm with no warning.
//
// The refusal applies to every controller, not only a cell: the caller runs
// it on the final items content, after a cell's own tenant merge.
//
// A parse failure returns no error. This check gates only a manifest that
// parses; a parse failure is reported through the caller's own error path.
func checkBranchBuildStrategiesPlugin(itemsYAML string, coreSet []pluginlock.PluginEntry) error {
	if pluginPinned(coreSet, branchBuildStrategiesPlugin) {
		return nil
	}
	manifest, err := items.Parse(itemsYAML)
	if err != nil {
		return nil //nolint:nilerr // a parse failure is reported through the caller's own error path, not this check
	}
	return checkBranchBuildStrategiesManifest(manifest, coreSet)
}

// checkBranchBuildStrategiesManifest is checkBranchBuildStrategiesPlugin
// for a manifest that is already parsed. A cell runs it on each tenant's
// re-rooted manifest, so one tenant's item holds only that tenant.
func checkBranchBuildStrategiesManifest(manifest *items.Manifest, coreSet []pluginlock.PluginEntry) error {
	if pluginPinned(coreSet, branchBuildStrategiesPlugin) {
		return nil
	}
	for _, ip := range manifest.Flatten() {
		if ip.Item.Kind == "multibranch" || ip.Item.Kind == "organizationFolder" {
			return fmt.Errorf("item %q (kind=%s) needs plugin %q, which the resolved plugin set omits",
				ip.Path, ip.Item.Kind, branchBuildStrategiesPlugin)
		}
	}
	return nil
}

// checkFolderLibraryPlugins refuses a manifest whose folder libraries need a
// plugin that coreSet omits. A parse failure returns no error, as in
// checkBranchBuildStrategiesPlugin.
func checkFolderLibraryPlugins(itemsYAML string, coreSet []pluginlock.PluginEntry) error {
	manifest, err := items.Parse(itemsYAML)
	if err != nil {
		return nil //nolint:nilerr // a parse failure is reported through the caller's own error path, not this check
	}
	return checkFolderLibraryPluginsManifest(manifest, coreSet)
}

// checkFolderLibraryPluginsManifest is checkFolderLibraryPlugins for a
// manifest that is already parsed. It checks each library's RequiredPlugins
// in item order and reports the first plugin that coreSet omits.
func checkFolderLibraryPluginsManifest(manifest *items.Manifest, coreSet []pluginlock.PluginEntry) error {
	for _, ip := range manifest.Flatten() {
		for _, p := range ip.Item.Properties {
			if p.FolderLibraries == nil {
				continue
			}
			for _, lc := range p.FolderLibraries.Libraries {
				for _, plugin := range lc.LibraryConfiguration.RequiredPlugins() {
					if !pluginPinned(coreSet, plugin) {
						return fmt.Errorf("item %q: folder library %q needs plugin %q, which the resolved plugin set omits",
							ip.Path, lc.LibraryConfiguration.Name, plugin)
					}
				}
			}
		}
	}
	return nil
}

// giteaPlugin supplies the Gitea and Forgejo branch source and navigator
// classes that the items engine renders for a gitea source or navigator.
const giteaPlugin = "gitea"

// checkGiteaPlugin refuses a manifest whose gitea sources or navigators need
// the gitea plugin when coreSet omits it. A parse failure returns no error,
// as in checkBranchBuildStrategiesPlugin.
func checkGiteaPlugin(itemsYAML string, coreSet []pluginlock.PluginEntry) error {
	manifest, err := items.Parse(itemsYAML)
	if err != nil {
		return nil //nolint:nilerr // a parse failure is reported through the caller's own error path, not this check
	}
	return checkGiteaPluginManifest(manifest, coreSet)
}

// checkGiteaPluginManifest is checkGiteaPlugin for a manifest that is already
// parsed.
func checkGiteaPluginManifest(manifest *items.Manifest, coreSet []pluginlock.PluginEntry) error {
	if pluginPinned(coreSet, giteaPlugin) {
		return nil
	}
	for _, ip := range manifest.Flatten() {
		for _, entry := range ip.Item.SourcesList {
			if entry.BranchSource.Source.Gitea != nil {
				return fmt.Errorf("item %q (kind=%s) has a gitea source, which needs plugin %q; the resolved plugin set omits it",
					ip.Path, ip.Item.Kind, giteaPlugin)
			}
		}
		for _, nav := range ip.Item.Navigators {
			if nav.Gitea != nil {
				return fmt.Errorf("item %q (kind=%s) has a gitea navigator, which needs plugin %q; the resolved plugin set omits it",
					ip.Path, ip.Item.Kind, giteaPlugin)
			}
		}
	}
	return nil
}
