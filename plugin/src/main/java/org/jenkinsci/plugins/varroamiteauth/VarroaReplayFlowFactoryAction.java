package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import hudson.model.InvisibleAction;
import hudson.model.Queue;
import hudson.model.Run;

import java.io.IOException;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowExecution;
import org.jenkinsci.plugins.workflow.cps.CpsFlowFactoryAction2;
import org.jenkinsci.plugins.workflow.flow.DurabilityHintProvider;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowDurabilityHint;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.flow.GlobalDefaultFlowDurabilityLevel;

/**
 * Supplies one replayed build's Groovy script (D9).
 *
 * <p>CpsFlowDefinition.create and CpsScmFlowDefinition.create both look for
 * a CpsFlowFactoryAction2 in the scheduled actions before they read their
 * own definition, so this action replaces the job's script for this build
 * alone.
 *
 * <p>The action implements no Queue.QueueAction. Jenkins schedules a second
 * queue item when any QueueAction of the new actions answers true, and
 * VarroaIntentAction answers false for an item carrying the same run id.
 * That guard is what stops a redelivered command from queueing two builds.
 *
 * <p>The execution is always sandboxed. The BFF refuses a source run whose
 * stored sandbox flag is not true, and an unsandboxed replay needs
 * ADMINISTER, which no tenant holds.
 */
final class VarroaReplayFlowFactoryAction extends InvisibleAction implements CpsFlowFactoryAction2 {

    private final String script;

    VarroaReplayFlowFactoryAction(String script) {
        this.script = script != null ? script : "";
    }

    /** Answers the script the replayed build runs. */
    String getScript() {
        return script;
    }

    @Override
    public CpsFlowExecution create(FlowDefinition def, FlowExecutionOwner owner,
            List<? extends Action> actions) throws IOException {
        Queue.Executable exec = owner.getExecutable();
        // The run-state mesh replicates a build only at the most durable
        // level, so the replay takes the job's own hint.
        FlowDurabilityHint hint = (exec instanceof Run)
                ? DurabilityHintProvider.suggestedFor(((Run<?, ?>) exec).getParent())
                : GlobalDefaultFlowDurabilityLevel.getDefaultDurabilityHint();
        return new CpsFlowExecution(script, true, owner, hint);
    }
}
