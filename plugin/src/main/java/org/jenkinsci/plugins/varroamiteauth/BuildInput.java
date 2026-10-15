package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import java.util.ArrayList;
import java.util.List;
import java.util.Objects;

/**
 * Immutable value object mirroring the {@code BuildInput} proto message.
 * One value describes one pending {@code input} step of a run. Field names
 * in {@link #toJson()} follow the proto JSON names exactly, so the mite
 * loopback accepts the body with no translation.
 *
 * <p>This class declares {@link #equals(Object)} and {@link #hashCode()}
 * over every field. The recorder compares one read of a run's pending
 * inputs with the read before it, and emits an event only when the two
 * differ (D1).
 */
public final class BuildInput {

    private final String id;
    private final String message;
    private final String ok;
    private final String submitter;
    private final String submitterParameter;
    private final List<BuildInputParameter> parameters;

    private BuildInput(Builder b) {
        this.id = orEmpty(b.id);
        this.message = orEmpty(b.message);
        this.ok = orEmpty(b.ok);
        this.submitter = orEmpty(b.submitter);
        this.submitterParameter = orEmpty(b.submitterParameter);
        this.parameters = List.copyOf(b.parameters);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    /** Field 1: the identifier of the run's input step execution. */
    public String getId() {
        return id;
    }

    public String getMessage() {
        return message;
    }

    /** Field 3: the label of the step's proceed control. */
    public String getOk() {
        return ok;
    }

    /** Field 4: the step's submitter specification. Empty when the step declares none. */
    public String getSubmitter() {
        return submitter;
    }

    /** Field 5: the name of the step's submitter parameter. Empty when the step declares none. */
    public String getSubmitterParameter() {
        return submitterParameter;
    }

    /** Field 6: the step's own parameter definitions, never the job's. */
    public List<BuildInputParameter> getParameters() {
        return parameters;
    }

    /** Serializes the input to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("id", id);
        o.put("message", message);
        o.put("ok", ok);
        o.put("submitter", submitter);
        o.put("submitter_parameter", submitterParameter);
        JSONArray parametersArr = new JSONArray();
        for (BuildInputParameter parameter : parameters) {
            parametersArr.add(parameter.toJson());
        }
        o.put("parameters", parametersArr);
        return o;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof BuildInput)) {
            return false;
        }
        BuildInput other = (BuildInput) o;
        return id.equals(other.id)
                && message.equals(other.message)
                && ok.equals(other.ok)
                && submitter.equals(other.submitter)
                && submitterParameter.equals(other.submitterParameter)
                && parameters.equals(other.parameters);
    }

    @Override
    public int hashCode() {
        return Objects.hash(id, message, ok, submitter, submitterParameter, parameters);
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String id;
        private String message;
        private String ok;
        private String submitter;
        private String submitterParameter;
        private List<BuildInputParameter> parameters = new ArrayList<>();

        private Builder() {
        }

        public Builder id(String v) {
            this.id = v;
            return this;
        }

        public Builder message(String v) {
            this.message = v;
            return this;
        }

        /** Field 3: the label of the step's proceed control. */
        public Builder ok(String v) {
            this.ok = v;
            return this;
        }

        /** Field 4: the step's submitter specification. */
        public Builder submitter(String v) {
            this.submitter = v;
            return this;
        }

        /** Field 5: the name of the step's submitter parameter. */
        public Builder submitterParameter(String v) {
            this.submitterParameter = v;
            return this;
        }

        /** Field 6: the step's own parameter definitions. */
        public Builder parameters(List<BuildInputParameter> v) {
            this.parameters = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        public BuildInput build() {
            return new BuildInput(this);
        }
    }
}
