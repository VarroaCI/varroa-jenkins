package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildLibrary} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 */
public final class BuildLibrary {

    private final String name;
    private final String version;
    private final String revision;
    private final String repositoryUrl;
    private final String repositoryForge;

    private BuildLibrary(Builder b) {
        this.name = orEmpty(b.name);
        this.version = orEmpty(b.version);
        this.revision = orEmpty(b.revision);
        this.repositoryUrl = orEmpty(b.repositoryUrl);
        this.repositoryForge = orEmpty(b.repositoryForge);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getName() {
        return name;
    }

    public String getVersion() {
        return version;
    }

    public String getRevision() {
        return revision;
    }

    /** The library repository URL. It carries no credentials and is empty when unknown. */
    public String getRepositoryUrl() {
        return repositoryUrl;
    }

    /** The forge of {@link #getRepositoryUrl()}: github, gitlab, or gitea. Empty when unknown. */
    public String getRepositoryForge() {
        return repositoryForge;
    }

    /** Serializes the library to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("version", version);
        o.put("revision", revision);
        o.put("repository_url", repositoryUrl);
        o.put("repository_forge", repositoryForge);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String name;
        private String version;
        private String revision;
        private String repositoryUrl;
        private String repositoryForge;

        private Builder() {
        }

        public Builder name(String v) {
            this.name = v;
            return this;
        }

        public Builder version(String v) {
            this.version = v;
            return this;
        }

        public Builder revision(String v) {
            this.revision = v;
            return this;
        }

        public Builder repositoryUrl(String v) {
            this.repositoryUrl = v;
            return this;
        }

        public Builder repositoryForge(String v) {
            this.repositoryForge = v;
            return this;
        }

        public BuildLibrary build() {
            return new BuildLibrary(this);
        }
    }
}
