package org.jenkinsci.plugins.varroamiteauth.template;

import static org.hamcrest.MatcherAssert.assertThat;
import static org.hamcrest.Matchers.containsString;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import io.jenkins.plugins.casc.ConfigurationContext;
import io.jenkins.plugins.casc.ConfiguratorRegistry;
import io.jenkins.plugins.casc.misc.ConfiguredWithCode;
import io.jenkins.plugins.casc.misc.JenkinsConfiguredWithCodeRule;
import io.jenkins.plugins.casc.misc.Util;
import io.jenkins.plugins.casc.model.CNode;

import org.junit.Rule;
import org.junit.Test;

/**
 * JCasC round-trip test for {@link VarroaPipelineTemplateStore} (task
 * 5.3): a delivered template imports under the {@code
 * varroaPipelineTemplates} symbol, resolves, and exports back out.
 */
public class VarroaPipelineTemplateStoreCascTest {

    @Rule
    public JenkinsConfiguredWithCodeRule j = new JenkinsConfiguredWithCodeRule();

    @Test
    @ConfiguredWithCode("varroa-pipeline-templates.yml")
    public void jcascImportsTheDeliveredTemplateSet() throws Exception {
        VarroaPipelineTemplateStore store = VarroaPipelineTemplateStore.get();
        assertNotNull(store);
        assertEquals(1, store.getTemplates().size());

        VarroaPipelineTemplateStore.TemplateEntry entry = store.getTemplates().get(0);
        assertEquals("go-service", entry.getName());
        assertEquals("2.1.3", entry.getVersion());
        assertEquals("", entry.getFolder());
        assertThat(entry.getParameters(), containsString("type: object"));
        assertEquals("pipeline { agent any }", entry.getJenkinsfile());

        VarroaPipelineTemplateStore.TemplateEntry resolved;
        try {
            resolved = store.resolve("", "go-service", "");
        } catch (VarroaPipelineTemplateStore.TemplateResolutionException e) {
            fail("expected go-service to resolve: " + e.getMessage());
            return;
        }
        assertEquals("2.1.3", resolved.getVersion());
    }

    @Test
    @ConfiguredWithCode("varroa-pipeline-templates.yml")
    public void jcascExportsTheDeliveredTemplateSet() throws Exception {
        ConfiguratorRegistry registry = ConfiguratorRegistry.get();
        ConfigurationContext context = new ConfigurationContext(registry);
        CNode node = Util.getUnclassifiedRoot(context).get("varroaPipelineTemplates");
        assertNotNull("JCasC must export a varroaPipelineTemplates node", node);
        String exportedYaml = Util.toYamlString(node);

        assertThat(exportedYaml, containsString("go-service"));
        assertThat(exportedYaml, containsString("2.1.3"));
    }
}
