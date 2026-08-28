package org.jenkinsci.plugins.varroamiteauth;

import java.time.Instant;
import java.time.format.DateTimeFormatter;

/**
 * Immutable value object representing a normalized activity event in the Varroa
 * activity feed schema.
 *
 * <p>Every event carries the fields {@code timestamp}, {@code type}, {@code source},
 * {@code actor}, {@code itemPath}, {@code buildNumber}, {@code result}, {@code url},
 * and {@code message}. The downstream-stamped fields {@code controller},
 * {@code namespace}, {@code phase}, and {@code reason} are always {@code ""} and
 * are present in the serialized JSON shape so the mite/operator can stamp them.
 *
 * <p>All String fields default to {@code ""} (never null). {@code buildNumber}
 * defaults to {@code 0}. {@code source} is always {@code "jenkins"}.
 */
public final class ActivityEvent {

    private final String timestamp;
    private final String type;
    private final String source;
    private final String actor;
    private final String itemPath;
    private final int buildNumber;
    private final String result;
    private final String url;
    private final String message;
    // Downstream-stamped fields — always empty in the plugin.
    private final String controller;
    private final String namespace;
    private final String phase;
    private final String reason;

    private ActivityEvent(Builder builder) {
        this.timestamp = builder.timestamp != null ? builder.timestamp : nowRfc3339();
        this.type = builder.type != null ? builder.type : "";
        this.source = "jenkins";
        this.actor = builder.actor != null ? builder.actor : "";
        this.itemPath = builder.itemPath != null ? builder.itemPath : "";
        this.buildNumber = builder.buildNumber;
        this.result = builder.result != null ? builder.result : "";
        this.url = builder.url != null ? builder.url : "";
        this.message = builder.message != null ? builder.message : "";
        this.controller = "";
        this.namespace = "";
        this.phase = "";
        this.reason = "";
    }

    // --- Getters (used by the endpoint's explicit JSON serialization) ---

    public String getTimestamp() { return timestamp; }
    public String getType() { return type; }
    public String getSource() { return source; }
    public String getActor() { return actor; }
    public String getItemPath() { return itemPath; }
    public int getBuildNumber() { return buildNumber; }
    public String getResult() { return result; }
    public String getUrl() { return url; }
    public String getMessage() { return message; }
    public String getController() { return controller; }
    public String getNamespace() { return namespace; }
    public String getPhase() { return phase; }
    public String getReason() { return reason; }

    // --- Builder ---

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String timestamp;
        private String type;
        private String actor;
        private String itemPath;
        private int buildNumber;
        private String result;
        private String url;
        private String message;

        private Builder() {}

        public Builder timestamp(String timestamp) { this.timestamp = timestamp; return this; }
        public Builder type(String type) { this.type = type; return this; }
        public Builder actor(String actor) { this.actor = actor; return this; }
        public Builder itemPath(String itemPath) { this.itemPath = itemPath; return this; }
        public Builder buildNumber(int buildNumber) { this.buildNumber = buildNumber; return this; }
        public Builder result(String result) { this.result = result; return this; }
        public Builder url(String url) { this.url = url; return this; }
        public Builder message(String message) { this.message = message; return this; }
        public ActivityEvent build() { return new ActivityEvent(this); }
    }

    /**
     * Returns the current instant formatted as an RFC3339/ISO-8601 UTC string,
     * e.g. {@code "2025-01-15T14:30:00Z"}.
     */
    public static String nowRfc3339() {
        return DateTimeFormatter.ISO_INSTANT.format(Instant.now());
    }
}
