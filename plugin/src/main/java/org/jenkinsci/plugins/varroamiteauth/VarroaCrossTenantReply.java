package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;

import java.util.Map;

/**
 * Parses a {@link MiteLoopbackClient.Response} from a cross-tenant route
 * (D2, D4, D14) into either a failure code and message, or the reply's
 * fields.
 *
 * <p>A transport failure and a structured {@code {"code":...}} reply map to
 * the same failure shape, so a caller checks {@link #isFailure()} and
 * {@link #code} once, whatever kind of failure produced them. A step shows
 * {@link #describe()} for a failure and {@link #describeVeto()} for a veto,
 * so the build log names the recorder's code and its text.
 */
final class VarroaCrossTenantReply {

    /** The failure code every mite-generated transport failure maps to. */
    static final String CODE_UNAVAILABLE = "UNAVAILABLE";

    /** The code the recorder answers for a run identifier or an artifact path it does not hold. */
    static final String CODE_NOT_FOUND = "NOT_FOUND";

    private static final Map<String, Object> NO_FIELDS = Map.of();

    /** Non-null on any failure: a transport failure or a structured error reply. */
    final String code;
    /**
     * The failure text: the reply's {@code error}, or the transport error
     * text. Empty when none was carried.
     */
    final String message;
    /** The reply's own fields, empty on a failure. */
    final Map<String, Object> fields;

    private VarroaCrossTenantReply(String code, String message, Map<String, Object> fields) {
        this.code = code;
        this.message = message;
        this.fields = fields;
    }

    boolean isFailure() {
        return code != null;
    }

    @SuppressWarnings("unchecked")
    static VarroaCrossTenantReply parse(MiteLoopbackClient.Response resp) {
        if (resp.isTransportFailure()) {
            return new VarroaCrossTenantReply(CODE_UNAVAILABLE, resp.error, NO_FIELDS);
        }
        Map<String, Object> parsed;
        try {
            Object decoded = new JsonSlurper().parseText(resp.body);
            parsed = (decoded instanceof Map) ? (Map<String, Object>) decoded : NO_FIELDS;
        } catch (Exception e) {
            return new VarroaCrossTenantReply(CODE_UNAVAILABLE, "the reply did not decode", NO_FIELDS);
        }
        Object code = parsed.get("code");
        if (code != null && !String.valueOf(code).isEmpty()) {
            Object error = parsed.get("error");
            return new VarroaCrossTenantReply(String.valueOf(code),
                    error != null ? String.valueOf(error) : "", NO_FIELDS);
        }
        return new VarroaCrossTenantReply(null, "", parsed);
    }

    /** The failure as {@code <code>: <text>}, or {@code <code>} alone when the text is empty. */
    String describe() {
        return message == null || message.isEmpty() ? code : code + ": " + message;
    }

    /** The reply's {@code reason_code}, or the empty string when the reply is no veto. */
    String reasonCode() {
        return getString("reason_code", "");
    }

    /**
     * {@code true} for a veto that waiting does not clear. {@code NOT_READY}
     * and {@code THROTTLED} are the two retryable reasons.
     */
    boolean isPermanentVeto() {
        String reason = reasonCode();
        return !reason.isEmpty() && !"NOT_READY".equals(reason) && !"THROTTLED".equals(reason);
    }

    /** The veto as {@code <reason_code>: <message>}, or the reason code alone when no message came. */
    String describeVeto() {
        String text = getString("message", "");
        return text.isEmpty() ? reasonCode() : reasonCode() + ": " + text;
    }

    /**
     * The trigger failure of a {@code TRIGGER_FAILED} run as
     * {@code " (<trigger_reason_code>: <trigger_error>)"}, or the empty string
     * when the reply carries no {@code trigger_reason_code}. A run that failed
     * to trigger holds no build, so these two fields tell the caller why.
     */
    String describeTriggerFailure() {
        String code = getString("trigger_reason_code", "");
        if (code.isEmpty()) {
            return "";
        }
        String text = getString("trigger_error", "");
        return text.isEmpty() ? " (" + code + ")" : " (" + code + ": " + text + ")";
    }

    String getString(String key, String def) {
        Object v = fields.get(key);
        return v != null ? String.valueOf(v) : def;
    }

    long getLong(String key, long def) {
        Object v = fields.get(key);
        if (v instanceof Number) {
            return ((Number) v).longValue();
        }
        if (v == null) {
            return def;
        }
        try {
            return Long.parseLong(String.valueOf(v));
        } catch (NumberFormatException e) {
            return def;
        }
    }
}
