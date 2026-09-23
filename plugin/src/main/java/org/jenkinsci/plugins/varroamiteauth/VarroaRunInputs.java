package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.ParameterDefinition;
import hudson.model.ParameterValue;
import hudson.model.Run;

import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Reads the pending {@code input} steps of a run and calls the step's own
 * settle paths (D1, D2, D11).
 *
 * <p>The plugin holds no compile dependency on {@code pipeline-input-step},
 * so every read and every call here goes through reflection and matches the
 * classes by name. A read fails closed: a missing class, a missing method, a
 * null result, and a thrown exception each answer an empty value, so a
 * controller without the plugin reports no input and throws nothing into a
 * Jenkins listener.
 *
 * <p>A call that decides a response code fails open: {@link #call} unwraps an
 * {@link InvocationTargetException} and rethrows a {@link RuntimeException}
 * cause as itself, so {@code hudson.model.Failure} reaches the calling route.
 *
 * <p>The package-private surface is {@link #readPendingInputs},
 * {@link #inputActionOf}, {@link #executionFor}, {@link #isSettled},
 * {@link #parameterDefinitions}, {@link #submitterParameter},
 * {@link #preSubmissionCheck}, {@link #proceed}, {@link #abort}, and
 * {@link #capInputs}. {@code capInputs} applies the event caps and is
 * package-private so a test can cap a set it built itself.
 */
final class VarroaRunInputs {

    private static final Logger LOGGER = Logger.getLogger(VarroaRunInputs.class.getName());

    /** The run action the {@code input} step attaches when it pauses. */
    private static final String INPUT_ACTION_CLASS =
            "org.jenkinsci.plugins.workflow.support.steps.input.InputAction";

    private static final Class<?>[] NO_ARGS = new Class<?>[0];

    /** Cap on the inputs one event carries. The oldest inputs survive. */
    static final int MAX_INPUTS = 20;
    /** Cap on the parameters one input carries. */
    static final int MAX_INPUT_PARAMETERS = 20;
    /** Cap on the choices one parameter carries. */
    static final int MAX_INPUT_CHOICES = 20;
    /** Cap on every string field, in UTF-8 bytes. */
    static final int MAX_INPUT_STRING_BYTES = 512;

    private VarroaRunInputs() {
    }

    /**
     * Returns the run's pending inputs, capped. The list is empty when the
     * run holds no input action, which is the case on a controller without
     * the input-step plugin.
     *
     * <p>An execution whose settled state the plugin cannot read stays out
     * of the set: a read that cannot answer must not put an input on a page
     * that no route can then settle. An execution with an empty identifier
     * stays out too, because no route can address it.
     */
    static List<BuildInput> readPendingInputs(Run<?, ?> run) {
        Object action = inputActionOf(run);
        if (action == null) {
            return List.of();
        }
        Object executions = read(action, "getExecutions");
        if (!(executions instanceof Iterable)) {
            return List.of();
        }
        List<BuildInput> found = new ArrayList<>();
        for (Object execution : (Iterable<?>) executions) {
            if (execution == null || isSettled(execution)) {
                continue;
            }
            Object step = read(execution, "getInput");
            String id = stringOf(read(execution, "getId"));
            if (id.isEmpty()) {
                id = stringOf(read(step, "getId"));
            }
            id = cut(id);
            if (id.isEmpty()) {
                continue;
            }
            found.add(inputValue(id, step));
        }
        return capInputs(found);
    }

    /** Returns the run's input action, or {@code null}. */
    static Object inputActionOf(Run<?, ?> run) {
        if (run == null) {
            return null;
        }
        return VarroaRunRecorder.findActionByClassName(run, INPUT_ACTION_CLASS);
    }

    /**
     * Returns one execution of the input action by its identifier, or
     * {@code null}. The lookup uses the action's own
     * {@code getExecution(String)}.
     */
    static Object executionFor(Object inputAction, String inputId) {
        if (inputAction == null || inputId == null || inputId.isEmpty()) {
            return null;
        }
        return call(inputAction, "getExecution", new Class<?>[] {String.class}, inputId);
    }

    /**
     * Reports whether the execution is settled. It answers {@code true} for
     * an unreadable settled state, so an input the plugin cannot settle
     * never reaches a page.
     */
    static boolean isSettled(Object execution) {
        Object settled = read(execution, "isSettled");
        return !(settled instanceof Boolean) || ((Boolean) settled).booleanValue();
    }

    /** Returns the step's own parameter definitions, never the job's. */
    static List<ParameterDefinition> parameterDefinitions(Object execution) {
        return definitionsOf(read(read(execution, "getInput"), "getParameters"));
    }

    /** Returns the step's submitter parameter name, or the empty string. */
    static String submitterParameter(Object execution) {
        return cut(stringOf(read(read(execution, "getInput"), "getSubmitterParameter")));
    }

    /**
     * Runs the step's own submission check. A refused caller reaches the
     * calling route as a {@code hudson.model.Failure}.
     */
    static void preSubmissionCheck(Object execution) {
        call(execution, "preSubmissionCheck", NO_ARGS);
    }

    /**
     * Proceeds the input with the supplied values. The map holds unwrapped
     * values, which is what the step expects.
     */
    static void proceed(Object execution, Map<String, Object> values) {
        call(execution, "proceed", new Class<?>[] {Map.class}, values);
    }

    /** Runs the step's own abort path. That path runs its own permission check. */
    static void abort(Object execution) {
        call(execution, "doAbort", NO_ARGS);
    }

    /**
     * Applies the event caps to one read: at most {@link #MAX_INPUTS}
     * inputs keeping the oldest, at most {@link #MAX_INPUT_PARAMETERS}
     * parameters for each input keeping the first, at most
     * {@link #MAX_INPUT_CHOICES} choices for each parameter, and at most
     * {@link #MAX_INPUT_STRING_BYTES} UTF-8 bytes for every string.
     *
     * <p>An input that waited longest is the one a user most needs to
     * answer, so the input cap drops the newest and never the oldest.
     */
    static List<BuildInput> capInputs(List<BuildInput> inputs) {
        if (inputs == null || inputs.isEmpty()) {
            return List.of();
        }
        List<BuildInput> capped = new ArrayList<>(Math.min(inputs.size(), MAX_INPUTS));
        for (BuildInput input : inputs) {
            if (input == null) {
                continue;
            }
            if (capped.size() == MAX_INPUTS) {
                break;
            }
            capped.add(capInput(input));
        }
        return capped;
    }

    // ---- The value mapping ----

    private static BuildInput inputValue(String id, Object step) {
        return BuildInput.builder()
                .id(id)
                .message(cut(stringOf(read(step, "getMessage"))))
                .ok(cut(stringOf(read(step, "getOk"))))
                .submitter(cut(stringOf(read(step, "getSubmitter"))))
                .submitterParameter(cut(stringOf(read(step, "getSubmitterParameter"))))
                .parameters(parameterValues(read(step, "getParameters")))
                .build();
    }

    private static List<BuildInputParameter> parameterValues(Object parameters) {
        List<BuildInputParameter> values = new ArrayList<>();
        if (!(parameters instanceof Iterable)) {
            return values;
        }
        for (Object element : (Iterable<?>) parameters) {
            if (values.size() == MAX_INPUT_PARAMETERS) {
                break;
            }
            if (element instanceof ParameterDefinition) {
                values.add(parameterValue((ParameterDefinition) element));
            }
        }
        return values;
    }

    private static BuildInputParameter parameterValue(ParameterDefinition definition) {
        ParameterValue defaultValue = definition.getDefaultParameterValue();
        // A sensitive default, such as a password, never reaches the
        // event: the record store keeps the set and every reader of the
        // run would see the plain text.
        Object value = defaultValue != null && !defaultValue.isSensitive() ? defaultValue.getValue() : null;
        return BuildInputParameter.builder()
                .name(cut(definition.getName()))
                .type(cut(definition.getClass().getSimpleName()))
                .description(cut(definition.getDescription()))
                .defaultValue(cut(value != null ? String.valueOf(value) : ""))
                .choices(choiceValues(definition))
                .build();
    }

    /**
     * Reads a definition's choices. {@code getChoices} sits on the choice
     * definition alone, so the read is reflective and a definition without
     * it answers an empty list.
     */
    private static List<String> choiceValues(ParameterDefinition definition) {
        List<String> choices = new ArrayList<>();
        Object raw = read(definition, "getChoices");
        if (!(raw instanceof Iterable)) {
            return choices;
        }
        for (Object element : (Iterable<?>) raw) {
            if (choices.size() == MAX_INPUT_CHOICES) {
                break;
            }
            if (element != null) {
                choices.add(cut(String.valueOf(element)));
            }
        }
        return choices;
    }

    private static List<ParameterDefinition> definitionsOf(Object raw) {
        List<ParameterDefinition> definitions = new ArrayList<>();
        if (!(raw instanceof Iterable)) {
            return definitions;
        }
        for (Object element : (Iterable<?>) raw) {
            if (element instanceof ParameterDefinition) {
                definitions.add((ParameterDefinition) element);
            }
        }
        return definitions;
    }

    // ---- The caps ----

    private static BuildInput capInput(BuildInput input) {
        BuildInput capped = BuildInput.builder()
                .id(cut(input.getId()))
                .message(cut(input.getMessage()))
                .ok(cut(input.getOk()))
                .submitter(cut(input.getSubmitter()))
                .submitterParameter(cut(input.getSubmitterParameter()))
                .parameters(capParameters(input.getParameters()))
                .build();
        return capped.equals(input) ? input : capped;
    }

    private static List<BuildInputParameter> capParameters(List<BuildInputParameter> parameters) {
        List<BuildInputParameter> capped = new ArrayList<>(Math.min(parameters.size(), MAX_INPUT_PARAMETERS));
        for (BuildInputParameter parameter : parameters) {
            if (capped.size() == MAX_INPUT_PARAMETERS) {
                break;
            }
            if (parameter != null) {
                capped.add(capParameter(parameter));
            }
        }
        return capped;
    }

    private static BuildInputParameter capParameter(BuildInputParameter parameter) {
        BuildInputParameter capped = BuildInputParameter.builder()
                .name(cut(parameter.getName()))
                .type(cut(parameter.getType()))
                .description(cut(parameter.getDescription()))
                .defaultValue(cut(parameter.getDefaultValue()))
                .choices(capChoices(parameter.getChoices()))
                .build();
        return capped.equals(parameter) ? parameter : capped;
    }

    private static List<String> capChoices(List<String> choices) {
        List<String> capped = new ArrayList<>(Math.min(choices.size(), MAX_INPUT_CHOICES));
        for (String choice : choices) {
            if (capped.size() == MAX_INPUT_CHOICES) {
                break;
            }
            capped.add(cut(choice));
        }
        return capped;
    }

    /** Cuts one string to the field cap, at a whole character boundary. */
    private static String cut(String s) {
        return VarroaRunRecorder.truncateUtf8Bytes(s != null ? s : "", MAX_INPUT_STRING_BYTES);
    }

    private static String stringOf(Object value) {
        return value instanceof String ? (String) value : "";
    }

    // ---- The two reflective paths ----

    /**
     * Reads one no-argument value. It answers {@code null} for a missing
     * method, a null result, and any thrown exception. Every read of a
     * pending input takes this path, so an absent plugin reports an empty
     * set.
     */
    private static Object read(Object target, String method) {
        if (target == null) {
            return null;
        }
        try {
            Method m = target.getClass().getMethod(method);
            return m.invoke(target);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunInputs: reflective read " + method + " failed", e);
            return null;
        }
    }

    /**
     * Calls one method and lets its own failure through, so a refused
     * settle never answers a success to its route. An
     * {@link InvocationTargetException} is unwrapped: a
     * {@link RuntimeException} cause is rethrown as itself, so
     * {@code hudson.model.Failure} reaches the route, and any other cause
     * is wrapped in a {@link RuntimeException}. Every method this path
     * calls is public upstream; a method the class does not declare public
     * is not callable.
     */
    private static Object call(Object target, String method, Class<?>[] types, Object... args) {
        if (target == null) {
            throw new IllegalArgumentException("no target for " + method);
        }
        Method m = findMethod(target.getClass(), method, types);
        if (m == null) {
            throw new IllegalStateException(method + " is not callable on " + target.getClass().getName());
        }
        try {
            return m.invoke(target, args);
        } catch (InvocationTargetException e) {
            Throwable cause = e.getCause();
            if (cause instanceof RuntimeException) {
                throw (RuntimeException) cause;
            }
            throw new RuntimeException(method + " failed", cause);
        } catch (ReflectiveOperationException e) {
            throw new RuntimeException(method + " is not callable", e);
        }
    }

    private static Method findMethod(Class<?> target, String name, Class<?>[] types) {
        try {
            return target.getMethod(name, types);
        } catch (NoSuchMethodException e) {
            LOGGER.log(Level.FINE, "VarroaRunInputs: no method " + name + " on " + target.getName(), e);
            return null;
        }
    }
}
