package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.Extension;

import java.util.logging.Level;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.flow.StepListener;
import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;

/**
 * Stops the Job DSL step at run time on a cell.
 *
 * <p>A cell home is temporary. A cell has one member only. A seed job
 * can write jobs outside the tenant folder. The lint rejects an inline
 * script that calls the step. This listener covers every other script
 * source, such as a Jenkinsfile in SCM and a pipeline template.
 *
 * <p>The listener runs before the step starts. A thrown exception does not
 * stop the step, because {@code DSL.invokeStep} only logs it. This listener
 * therefore calls {@link StepContext#onFailure(Throwable)} to fail the step.
 *
 * <p>The listener is active only when {@code VARROA_CELL} is {@code true}.
 * It reads that value once. A change needs a pod restart.
 *
 * <p>The listener finds the Job DSL builder by reflection. The
 * {@code workflow-basic-steps} classes are test scope only in this module.
 */
@Extension
public class VarroaCellJobDslBlock implements StepListener {

    private static final Logger LOGGER = Logger.getLogger(VarroaCellJobDslBlock.class.getName());

    static final String CELL_ENV = "VARROA_CELL";
    static final String CORE_STEP = "org.jenkinsci.plugins.workflow.steps.CoreStep";
    static final String JOB_DSL_BUILDER = "javaposse.jobdsl.plugin.ExecuteDslScripts";
    static final String MESSAGE = "The jobDsl step is not allowed on a cell. Declare each job in items.yaml.";

    private static volatile boolean cellEnabled = readCellEnv();

    private static boolean readCellEnv() {
        return "true".equals(System.getenv(CELL_ENV));
    }

    @Override
    public void notifyOfNewStep(Step s, StepContext context) {
        if (!cellEnabled || !isJobDsl(s)) {
            return;
        }
        context.onFailure(new AbortException(MESSAGE));
    }

    /**
     * Reports whether a step runs the Job DSL builder.
     *
     * <p>The check reads the {@code delegate} field of a {@code CoreStep}.
     * A reflection failure answers true, so the step fails closed.
     */
    static boolean isJobDsl(Step s) {
        if (!CORE_STEP.equals(s.getClass().getName())) {
            return false;
        }
        Object delegate;
        try {
            delegate = s.getClass().getField("delegate").get(s);
        } catch (ReflectiveOperationException | RuntimeException e) {
            LOGGER.log(Level.WARNING, "Cannot read the CoreStep delegate. Blocking the step on a cell.", e);
            return true;
        }
        for (Class<?> c = delegate == null ? null : delegate.getClass(); c != null; c = c.getSuperclass()) {
            if (JOB_DSL_BUILDER.equals(c.getName())) {
                return true;
            }
        }
        return false;
    }

    /** Sets the cell flag for a test. */
    static void setCellForTest(boolean v) {
        cellEnabled = v;
    }

    /** Restores the cell flag from the environment after a test. */
    static void resetCellForTest() {
        cellEnabled = readCellEnv();
    }
}
