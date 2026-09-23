package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import java.util.ArrayList;
import java.util.List;
import java.util.Objects;

/**
 * Immutable value object mirroring the {@code BuildInputParameter} proto
 * message. Field names in {@link #toJson()} follow the proto JSON names
 * exactly, so the mite loopback accepts the body with no translation.
 *
 * <p>This class declares {@link #equals(Object)} and {@link #hashCode()}
 * over every field. The recorder compares one read of a run's pending
 * inputs with the read before it, and emits an event only when the two
 * differ (D1).
 */
public final class BuildInputParameter {

    private final String name;
    private final String type;
    private final String description;
    private final String defaultValue;
    private final List<String> choices;

    private BuildInputParameter(Builder b) {
        this.name = orEmpty(b.name);
        this.type = orEmpty(b.type);
        this.description = orEmpty(b.description);
        this.defaultValue = orEmpty(b.defaultValue);
        this.choices = List.copyOf(b.choices);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getName() {
        return name;
    }

    /** Field 2: the definition class's simple name, for example {@code StringParameterDefinition}. */
    public String getType() {
        return type;
    }

    public String getDescription() {
        return description;
    }

    /**
     * Field 4: the definition's default value, rendered as text. Empty
     * when the definition declares no default.
     */
    public String getDefaultValue() {
        return defaultValue;
    }

    /** Field 5: the definition's choices. Empty for a definition that declares none. */
    public List<String> getChoices() {
        return choices;
    }

    /** Serializes the parameter to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("type", type);
        o.put("description", description);
        o.put("default_value", defaultValue);
        JSONArray choicesArr = new JSONArray();
        choicesArr.addAll(choices);
        o.put("choices", choicesArr);
        return o;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof BuildInputParameter)) {
            return false;
        }
        BuildInputParameter other = (BuildInputParameter) o;
        return name.equals(other.name)
                && type.equals(other.type)
                && description.equals(other.description)
                && defaultValue.equals(other.defaultValue)
                && choices.equals(other.choices);
    }

    @Override
    public int hashCode() {
        return Objects.hash(name, type, description, defaultValue, choices);
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String name;
        private String type;
        private String description;
        private String defaultValue;
        private List<String> choices = new ArrayList<>();

        private Builder() {
        }

        public Builder name(String v) {
            this.name = v;
            return this;
        }

        /** Field 2: the definition class's simple name. */
        public Builder type(String v) {
            this.type = v;
            return this;
        }

        public Builder description(String v) {
            this.description = v;
            return this;
        }

        /** Field 4: the definition's default value, rendered as text. */
        public Builder defaultValue(String v) {
            this.defaultValue = v;
            return this;
        }

        /** Field 5: the definition's choices. */
        public Builder choices(List<String> v) {
            this.choices = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        public BuildInputParameter build() {
            return new BuildInputParameter(this);
        }
    }
}
