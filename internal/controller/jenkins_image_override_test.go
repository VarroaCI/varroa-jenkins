package controller

import "testing"

// TestJenkinsImageForVersion_RepositoryOverride verifies that the chart's
// jenkins.image.repository reaches the Jenkins controller image, and that the
// core version stays the tag.
func TestJenkinsImageForVersion_RepositoryOverride(t *testing.T) {
	t.Run("unset keeps the public repository", func(t *testing.T) {
		t.Setenv("VARROA_JENKINS_IMAGE_REPOSITORY", "")
		if got := jenkinsImageForVersion("2.570", nil); got != "jenkins/jenkins:2.570" {
			t.Errorf("image = %q, want jenkins/jenkins:2.570", got)
		}
	})
	t.Run("set replaces the repository only", func(t *testing.T) {
		t.Setenv("VARROA_JENKINS_IMAGE_REPOSITORY", "registry.internal/mirror/jenkins")
		if got := jenkinsImageForVersion("2.570", nil); got != "registry.internal/mirror/jenkins:2.570" {
			t.Errorf("image = %q, want registry.internal/mirror/jenkins:2.570", got)
		}
	})
}
