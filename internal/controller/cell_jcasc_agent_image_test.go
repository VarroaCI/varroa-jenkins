package controller

import "testing"

// TestCellJCasC_AgentImageOverride verifies that the jnlp agent image follows
// VARROA_JENKINS_AGENT_IMAGE and falls back to the pinned default.
func TestCellJCasC_AgentImageOverride(t *testing.T) {
	t.Run("unset keeps the pinned default", func(t *testing.T) {
		t.Setenv("VARROA_JENKINS_AGENT_IMAGE", "")
		if got := tenantAgentJnlpImage(); got != defaultTenantAgentJnlpImage {
			t.Errorf("agent image = %q, want %q", got, defaultTenantAgentJnlpImage)
		}
	})
	t.Run("set replaces the image", func(t *testing.T) {
		t.Setenv("VARROA_JENKINS_AGENT_IMAGE", "registry.internal/mirror/inbound-agent:3384-jdk21")
		if got := tenantAgentJnlpImage(); got != "registry.internal/mirror/inbound-agent:3384-jdk21" {
			t.Errorf("agent image = %q, want the override", got)
		}
	})
}
