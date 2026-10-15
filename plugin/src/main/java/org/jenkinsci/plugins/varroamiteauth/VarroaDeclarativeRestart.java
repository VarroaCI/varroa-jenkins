package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import hudson.model.Actionable;
import hudson.model.Cause;
import hudson.model.Run;

import java.lang.reflect.Constructor;
import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

import jenkins.model.Jenkins;

/**
 * Reads and drives the declarative pipeline restart of a completed run,
 * through reflection only. The declarative plugin is a test-scope
 * dependency of this plugin, so its classes load at run time and are
 * never linked at compile time.
 *
 * <p>{@link #inspect} answers the stages a run can restart from, or the
 * reason it cannot. {@link #newRestartFlowFactoryAction} and {@link
 * #newRestartCause} build the two objects that Jenkins' own {@code
 * RestartDeclarativePipelineAction.run(String)} would build. That method
 * schedules the build itself and cannot carry a {@link VarroaIntentAction},
 * so the restart route schedules the build with these objects.
 */
final class VarroaDeclarativeRestart {

    /** The most stage names {@link #inspect} answers. */
    static final int MAX_RESTART_STAGES = 200;

    static final String REFUSAL_NOT_DECLARATIVE = "not-declarative";
    static final String REFUSAL_DISABLED = "disabled";
    static final String REFUSAL_UNAVAILABLE = "unavailable";
    static final String REFUSAL_NO_RESTARTABLE_STAGE = "no-restartable-stage";

    private static final String PACKAGE = "org.jenkinsci.plugins.pipeline.modeldefinition.";
    private static final String EXECUTION_MODEL = PACKAGE + "actions.ExecutionModelAction";
    private static final String RESTART_ACTION = PACKAGE + "actions.RestartDeclarativePipelineAction";
    private static final String DISABLE_ACTION = PACKAGE + "actions.DisableRestartFromStageAction";
    private static final String FLOW_FACTORY = PACKAGE + "actions.RestartFlowFactoryAction";
    private static final String CAUSE = PACKAGE + "causes.RestartDeclarativePipelineCause";

    private VarroaDeclarativeRestart() {
    }

    /** The answer of {@link #inspect}: the restartable stages, or the refusal reason. */
    static final class Outcome {
        private final List<String> stages;
        private final String refusal;

        private Outcome(List<String> stages, String refusal) {
            this.stages = stages;
            this.refusal = refusal;
        }

        /** The stages a restart can begin at, in run order. Empty when refused. */
        List<String> getStages() {
            return stages;
        }

        /** The reason the run cannot restart, or {@code null} when it can. */
        String getRefusal() {
            return refusal;
        }
    }

    /**
     * Answers the restartable stages of a completed run, or the refusal
     * reason. Any failure to load or call the declarative classes answers
     * {@link #REFUSAL_UNAVAILABLE}.
     */
    static Outcome inspect(Run<?, ?> run) {
        try {
            load(FLOW_FACTORY);
            load(CAUSE);
            Class<?> executionModel = load(EXECUTION_MODEL);
            Class<?> disableAction = load(DISABLE_ACTION);
            Class<?> restartAction = load(RESTART_ACTION);
            if (findAction(run, executionModel) == null) {
                return refused(REFUSAL_NOT_DECLARATIVE);
            }
            if (findAction(run.getParent(), disableAction) != null) {
                return refused(REFUSAL_DISABLED);
            }
            Object restart = findAction(run, restartAction);
            if (restart == null) {
                return refused(REFUSAL_NO_RESTARTABLE_STAGE);
            }
            if (!Boolean.TRUE.equals(invoke(restart, "isRestartEnabled"))) {
                return refused(REFUSAL_DISABLED);
            }
            List<String> stages = stageNames(restart);
            if (stages.isEmpty()) {
                return refused(REFUSAL_NO_RESTARTABLE_STAGE);
            }
            return new Outcome(stages, null);
        } catch (Throwable t) {
            return refused(REFUSAL_UNAVAILABLE);
        }
    }

    /**
     * Builds the {@code RestartFlowFactoryAction} that makes the new build
     * run the restart flow of {@code origin}.
     *
     * @throws IllegalStateException when the declarative classes do not load
     */
    static Action newRestartFlowFactoryAction(Run<?, ?> origin) {
        try {
            Constructor<?> ctor = load(FLOW_FACTORY).getConstructor(String.class);
            return (Action) ctor.newInstance(origin.getExternalizableId());
        } catch (ReflectiveOperationException | LinkageError e) {
            throw new IllegalStateException("declarative restart is unavailable", e);
        }
    }

    /**
     * Builds the {@code RestartDeclarativePipelineCause} that names {@code
     * origin} and {@code stage}.
     *
     * @throws IllegalStateException when the declarative classes do not load
     */
    static Cause newRestartCause(Run<?, ?> origin, String stage) {
        try {
            Constructor<?> ctor = load(CAUSE).getConstructor(Run.class, String.class);
            return (Cause) ctor.newInstance(origin, stage);
        } catch (ReflectiveOperationException | LinkageError e) {
            throw new IllegalStateException("declarative restart is unavailable", e);
        }
    }

    private static Outcome refused(String reason) {
        return new Outcome(Collections.emptyList(), reason);
    }

    private static Class<?> load(String name) throws ClassNotFoundException {
        return Jenkins.get().getPluginManager().uberClassLoader.loadClass(name);
    }

    /**
     * Answers the first action of {@code type} on {@code owner}, or {@code
     * null}. The search covers transient actions, because the declarative
     * plugin adds its restart action through a transient factory.
     */
    private static Object findAction(Actionable owner, Class<?> type) {
        for (Action action : owner.getAllActions()) {
            if (type.isInstance(action)) {
                return action;
            }
        }
        return null;
    }

    private static Object invoke(Object target, String method) throws ReflectiveOperationException {
        Method m = target.getClass().getMethod(method);
        return m.invoke(target);
    }

    private static List<String> stageNames(Object restart) throws ReflectiveOperationException {
        Object raw = invoke(restart, "getRestartableStages");
        List<String> names = new ArrayList<>();
        if (!(raw instanceof List)) {
            return names;
        }
        for (Object name : (List<?>) raw) {
            if (names.size() == MAX_RESTART_STAGES) {
                break;
            }
            names.add(String.valueOf(name));
        }
        return names;
    }
}
