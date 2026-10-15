package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildPodTemplate} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 */
public final class BuildPodTemplate {

    private final String name;
    private final String label;
    private final String image;

    private BuildPodTemplate(Builder b) {
        this.name = orEmpty(b.name);
        this.label = orEmpty(b.label);
        this.image = orEmpty(b.image);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getName() {
        return name;
    }

    public String getLabel() {
        return label;
    }

    public String getImage() {
        return image;
    }

    /** Serializes the pod template to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("label", label);
        o.put("image", image);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String name;
        private String label;
        private String image;

        private Builder() {
        }

        public Builder name(String v) {
            this.name = v;
            return this;
        }

        public Builder label(String v) {
            this.label = v;
            return this;
        }

        public Builder image(String v) {
            this.image = v;
            return this;
        }

        public BuildPodTemplate build() {
            return new BuildPodTemplate(this);
        }
    }
}
