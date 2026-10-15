package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Run;

import java.util.Set;

import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.jenkinsci.plugins.workflow.steps.SynchronousStepExecution;
import org.kohsuke.stapler.DataBoundConstructor;

/**
 * {@code varroaSummary}: attaches one summary line to the run that calls
 * it. The whole body is a local action attach on the run's own
 * object, with no network round trip and no I/O, so {@link
 * SynchronousStepExecution} keeps it on the calling step's own thread.
 * An empty text attaches nothing, so the run keeps the last text that
 * was not empty.
 */
public class VarroaSummaryStep extends Step {

    private final String text;

    @DataBoundConstructor
    public VarroaSummaryStep(String text) {
        this.text = text;
    }

    public String getText() {
        return text;
    }

    @Override
    public StepExecution start(StepContext context) {
        return new Execution(this, context);
    }

    @Extension
    public static final class DescriptorImpl extends StepDescriptor {
        @Override
        public String getFunctionName() {
            return "varroaSummary";
        }

        @Override
        public String getDisplayName() {
            return "Attach a summary line to this run";
        }

        @Override
        public Set<Class<?>> getRequiredContext() {
            return Set.of(Run.class);
        }
    }

    static final class Execution extends SynchronousStepExecution<Void> {

        private static final long serialVersionUID = 1L;

        private final String rawText;

        Execution(VarroaSummaryStep step, StepContext context) {
            super(context);
            this.rawText = step.getText();
        }

        @Override
        protected Void run() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            String cut = VarroaRunRecorder.truncateUtf8Bytes(rawText == null ? "" : rawText, 512);
            // An empty text attaches nothing, so the run keeps the last text
            // that was not empty and the next carrier event still reports it.
            if (cut.isEmpty()) {
                return null;
            }
            run.addOrReplaceAction(new VarroaSummaryAction(cut));
            VarroaRunRecorder.recordSummary(run, cut);
            return null;
        }
    }
}
