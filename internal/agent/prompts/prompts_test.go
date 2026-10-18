package prompts

import "testing"

// TestRegistryPinsIDsAndFlags pins both prompt ids and their startable flags.
func TestRegistryPinsIDsAndFlags(t *testing.T) {
	t.Parallel()

	explain, ok := Get("explain-failure")
	if !ok || explain.ID != ExplainFailure || !explain.Startable {
		t.Fatalf("explain-failure = (%+v, %v), want startable", explain, ok)
	}
	propose, ok := Get("propose-pipeline")
	if !ok || propose.ID != ProposePipeline || propose.Startable {
		t.Fatalf("propose-pipeline = (%+v, %v), want not startable", propose, ok)
	}
	if _, ok := Get("missing"); ok {
		t.Fatal("an unknown id was found")
	}
	all := All()
	if len(all) != 2 || all[0].ID != ExplainFailure || all[1].ID != ProposePipeline {
		t.Fatalf("All = %+v, want two prompts sorted by id", all)
	}
}
