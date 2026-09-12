package controller

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// branchBuildStrategiesPlugin supplies
// jenkins.branch.buildstrategies.basic.SkipInitialBuildOnFirstBranchIndexing,
// which the items engine renders into every multibranch item and
// organization folder item (design D12).
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
	for _, ip := range manifest.Flatten() {
		if ip.Item.Kind == "multibranch" || ip.Item.Kind == "organizationFolder" {
			return fmt.Errorf("item %q (kind=%s) needs plugin %q, which the resolved plugin set omits",
				ip.Path, ip.Item.Kind, branchBuildStrategiesPlugin)
		}
	}
	return nil
}
