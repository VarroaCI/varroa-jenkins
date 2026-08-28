package v1alpha1

import "testing"

// TestJenkinsVersionProfileDeepCopyIsolatesSlices implements the jenkins-version-profile task 1.3
// acceptance: a populated profile (jcasc.requiredPlugins + a condition) is deep-copied, the COPY is
// mutated, and the ORIGINAL must be unchanged.
func TestJenkinsVersionProfileDeepCopyIsolatesSlices(t *testing.T) {
	orig := JenkinsVersionProfileSpec{
		Version:      "2.479.3",
		Channel:      "lts",
		PluginSetRef: &ConfigMapRef{Name: "src-cm"},
		JCasC: &VersionJCasC{
			Content:         "jenkins:\n  systemMessage: hi\n",
			RequiredPlugins: []string{"acme-plugin", "git"},
		},
	}

	cp := new(JenkinsVersionProfileSpec)
	orig.DeepCopyInto(cp)
	cp.JCasC.RequiredPlugins[0] = "mutated"
	cp.JCasC.Content = "changed"
	cp.PluginSetRef.Name = "other-cm"

	if orig.JCasC.RequiredPlugins[0] != "acme-plugin" {
		t.Errorf("requiredPlugins not isolated: original mutated to %q", orig.JCasC.RequiredPlugins[0])
	}
	if orig.JCasC.Content != "jenkins:\n  systemMessage: hi\n" {
		t.Errorf("jcasc content not isolated: original mutated to %q", orig.JCasC.Content)
	}
	if orig.PluginSetRef.Name != "src-cm" {
		t.Errorf("pluginSetRef not isolated: original mutated to %q", orig.PluginSetRef.Name)
	}
}
