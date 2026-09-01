package pluginresolve

import (
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// AssertCoreFloor gates rootHPI's own RequiredCore against target. It is the
// root-plugin counterpart to checkCoreFloor: rootHPI is baked into the image
// rather than a resolved Closure member, so a floor breach here is reported as
// ErrRootCoreFloorExceeded rather than ErrCoreFloorExceeded.
func AssertCoreFloor(target string, rootHPI []byte) error {
	mf, err := hpi.ParseHPIBytes(rootHPI)
	if err != nil {
		return fmt.Errorf("parsing root HPI: %w", err)
	}
	if mf.RequiredCore == "" {
		return nil
	}
	atLeast, ok := jenkinsver.AtLeast(target, mf.RequiredCore)
	if !ok {
		return fmt.Errorf("%w: %s@%s requires core %q", ErrInvalidVersion, mf.ShortName, mf.Version, mf.RequiredCore)
	}
	if !atLeast {
		return fmt.Errorf("%w: %s@%s requires core >= %q, target is %q", ErrRootCoreFloorExceeded, mf.ShortName, mf.Version, mf.RequiredCore, target)
	}
	return nil
}
