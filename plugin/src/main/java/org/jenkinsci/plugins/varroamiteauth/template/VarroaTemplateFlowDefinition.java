package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.AbortException;
import hudson.Extension;
import hudson.Functions;
import hudson.model.Action;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.util.List;
import java.util.Map;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowDefinitionDescriptor;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;

import org.kohsuke.stapler.DataBoundConstructor;

/**
 * The items-declared form of a pipeline template (D8, D10, {@code
 * pipeline-marker-file}): {@code template}, {@code templateVersion}, and
 * {@code values} come from the item's own {@code config.xml}, rendered by
 * {@code GenerateConfigXML}, never from a marker in a consumer repository.
 * Its values are already validated at composition time (L2); step 6 is
 * identical to {@link VarroaMarkerFlowDefinition}'s.
 */
public class VarroaTemplateFlowDefinition extends FlowDefinition {

    private final String template;
    private final String templateVersion;
    private final String values;

    @DataBoundConstructor
    public VarroaTemplateFlowDefinition(String template, String templateVersion, String values) {
        this.template = template != null ? template : "";
        this.templateVersion = templateVersion != null ? templateVersion : "";
        this.values = values != null ? values : "";
    }

    public String getTemplate() {
        return template;
    }

    public String getTemplateVersion() {
        return templateVersion;
    }

    public String getValues() {
        return values;
    }

    @Override
    public FlowExecution create(FlowExecutionOwner owner, TaskListener listener, List<? extends Action> actions)
            throws Exception {
        Run<?, ?> run = VarroaMarkerFlowDefinition.requireRun(owner);
        try {
            VarroaPipelineTemplateStore store = VarroaPipelineTemplateStore.get();
            String tenantFolder = VarroaMarkerFlowDefinition.tenantFolderOf(run);
            VarroaPipelineTemplateStore.TemplateEntry entry;
            try {
                entry = store.resolve(tenantFolder, template, templateVersion);
            } catch (VarroaPipelineTemplateStore.TemplateResolutionException e) {
                throw new AbortException(e.getMessage());
            }

            Map<String, Object> schema = VarroaMarkerFlowDefinition.parseSchema(entry);
            Map<String, Object> declaredValues = parseValues();
            VarroaMarkerValidator.ValuesResult result = VarroaMarkerValidator.validateValues(schema, declaredValues);
            if (!result.getFindings().isEmpty()) {
                throw new AbortException(VarroaMarkerFlowDefinition.valuesFailureMessage(
                        "", entry.getName(), result.getFindings(), result.getDefaulted()));
            }

            run.addAction(VarroaTemplateValuesAction.resolved(
                    entry.getName(), entry.getVersion(), "", "", result.getDefaulted()));

            return VarroaMarkerFlowDefinition.buildExecution(store, entry, owner, listener, actions);
        } catch (Exception e) {
            String message = e.getMessage() != null ? e.getMessage() : e.toString();
            if (e instanceof AbortException) {
                listener.error(message);
            } else {
                listener.error(message).println(Functions.printThrowable(e).trim());
            }
            run.addAction(VarroaTemplateValuesAction.rejected(template, templateVersion, "", "", message));
            throw e instanceof AbortException ? e : new AbortException(message);
        }
    }

    @SuppressWarnings("unchecked")
    private Map<String, Object> parseValues() throws AbortException {
        if (values.isEmpty()) {
            return Map.of();
        }
        try {
            return JSONObject.fromObject(values);
        } catch (RuntimeException e) {
            throw new AbortException("item template \"" + template + "\": values are not valid JSON: " + e.getMessage());
        }
    }

    @Extension
    public static final class DescriptorImpl extends FlowDefinitionDescriptor {
        @Override
        public String getDisplayName() {
            return "Varroa pipeline template (items-declared)";
        }
    }
}
