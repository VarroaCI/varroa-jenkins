package controller

import (
	"sort"
	"testing"
)

// TestArtifactManagerJCasCKeys_MatchesSpike asserts the constant list holds
// every key path task 0.5's export named (review-log.md, "Task 0.5"),
// including the factory path and the per-cell prefix key.
func TestArtifactManagerJCasCKeys_MatchesSpike(t *testing.T) {
	want := []string{
		"unclassified.artifactManager.artifactManagerFactories[].jclouds.provider",
		"aws.awsCredentials.credentialsId",
		"aws.awsCredentials.region",
		"aws.s3.container",
		"aws.s3.prefix",
		"aws.s3.customEndpoint",
		"aws.s3.usePathStyleUrl",
		"aws.s3.useHttp",
		"aws.s3.disableSessionToken",
		"aws.s3.customSigningRegion",
		"aws.s3.useTransferAcceleration",
	}

	got := append([]string(nil), ArtifactManagerJCasCKeys...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)

	if len(got) != len(sortedWant) {
		t.Fatalf("ArtifactManagerJCasCKeys has %d keys, want %d: got %v", len(got), len(sortedWant), got)
	}
	for i := range got {
		if got[i] != sortedWant[i] {
			t.Fatalf("ArtifactManagerJCasCKeys[%d] = %q, want %q (full: %v)", i, got[i], sortedWant[i], got)
		}
	}

	if got := ArtifactManagerFactoryPath; got != "unclassified.artifactManager.artifactManagerFactories[].jclouds.provider" {
		t.Errorf("ArtifactManagerFactoryPath = %q, want the factory path", got)
	}
	if got := ArtifactManagerS3PrefixKey; got != "aws.s3.prefix" {
		t.Errorf("ArtifactManagerS3PrefixKey = %q, want the prefix key", got)
	}

	found := false
	for _, k := range ArtifactManagerJCasCKeys {
		if k == ArtifactManagerFactoryPath {
			found = true
		}
	}
	if !found {
		t.Error("ArtifactManagerJCasCKeys does not contain ArtifactManagerFactoryPath")
	}

	found = false
	for _, k := range ArtifactManagerJCasCKeys {
		if k == ArtifactManagerS3PrefixKey {
			found = true
		}
	}
	if !found {
		t.Error("ArtifactManagerJCasCKeys does not contain ArtifactManagerS3PrefixKey")
	}
}
