package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.model.Action;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.util.List;
import java.util.Map;

import org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateValuesAction;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;

/**
 * A test double standing in for {@code VarroaMarkerFlowDefinition} and
 * {@code VarroaTemplateFlowDefinition} (D8), neither of which this batch
 * implements. Registered with {@link
 * VarroaRunRecorder#setTemplateDefinitionClassNamesForTesting} so {@link
 * VarroaRunRecorder.Runs#onStarted} holds the {@code STARTED} event the
 * way it would for the real flow definitions.
 */
final class FakeVarroaFlowDefinition extends FlowDefinition {

    private final String script;
    private final boolean rejected;
    private final boolean attachNothing;

    /**
     * @param script         the pipeline script the fake definition delegates to
     * @param rejected       attach a rejected {@link VarroaTemplateValuesAction} and fail the build
     * @param attachNothing  attach no action at all, exercising the terminal-state fallback release
     */
    FakeVarroaFlowDefinition(String script, boolean rejected, boolean attachNothing) {
        this.script = script;
        this.rejected = rejected;
        this.attachNothing = attachNothing;
    }

    @Override
    public FlowExecution create(FlowExecutionOwner owner, TaskListener listener, List<? extends Action> actions)
            throws Exception {
        if (!attachNothing) {
            Run<?, ?> run = (Run<?, ?>) owner.getExecutable();
            VarroaTemplateValuesAction action = rejected
                    ? VarroaTemplateValuesAction.rejected("go-service", "2.1.4", ".varroa/pipeline.yaml",
                            "abc123", "values.goVersion: not one of the allowed values")
                    : VarroaTemplateValuesAction.resolved("go-service", "2.1.4", ".varroa/pipeline.yaml",
                            "abc123", Map.of("goVersion", "1.24"));
            run.addAction(action);
            if (rejected) {
                throw new AbortException("the marker was rejected");
            }
        }
        return new CpsFlowDefinition(script, true).create(owner, listener, actions);
    }
}
