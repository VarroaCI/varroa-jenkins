package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.model.InvisibleAction;
import hudson.model.Run;

import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.Map;

import jenkins.model.RunAction2;

import org.jenkinsci.plugins.varroamiteauth.VarroaRunRecorder;

/**
 * Carries the outcome of resolving a marker or an items-declared template
 * against {@link VarroaPipelineTemplateStore} (D8): the template name, the
 * resolved version, the marker path, the marker revision, the validated
 * values, and a {@code rejected} flag with its message. One class serves
 * both the resolved and the rejected state, so no eighth class is needed.
 *
 * <p>Implements {@link RunAction2} so {@link #onAttached} fires the moment
 * a {@link org.jenkinsci.plugins.workflow.flow.FlowDefinition} calls {@code
 * run.addAction(this)} — the signal {@link VarroaRunRecorder} needs to
 * release a held {@code STARTED} event (D8a, {@code build-event-capture}).
 */
public final class VarroaTemplateValuesAction extends InvisibleAction implements RunAction2 {

    private final String templateName;
    private final String templateVersion;
    private final String markerPath;
    private final String markerRevision;
    private final Map<String, Object> values;
    private final boolean rejected;
    private final String message;

    private VarroaTemplateValuesAction(String templateName, String templateVersion, String markerPath,
            String markerRevision, Map<String, Object> values, boolean rejected, String message) {
        this.templateName = orEmpty(templateName);
        this.templateVersion = orEmpty(templateVersion);
        this.markerPath = orEmpty(markerPath);
        this.markerRevision = orEmpty(markerRevision);
        this.values = values != null
                ? Collections.unmodifiableMap(new LinkedHashMap<>(values))
                : Collections.emptyMap();
        this.rejected = rejected;
        this.message = orEmpty(message);
    }

    /** A run whose marker or item resolved and validated. */
    public static VarroaTemplateValuesAction resolved(String templateName, String templateVersion,
            String markerPath, String markerRevision, Map<String, Object> values) {
        return new VarroaTemplateValuesAction(templateName, templateVersion, markerPath, markerRevision, values,
                false, "");
    }

    /**
     * A run whose marker or item failed steps 1 to 4. Carries only the
     * fields the definition read before the failure; the rest are empty.
     */
    public static VarroaTemplateValuesAction rejected(String templateName, String templateVersion,
            String markerPath, String markerRevision, String message) {
        return new VarroaTemplateValuesAction(templateName, templateVersion, markerPath, markerRevision, null, true,
                message);
    }

    public String getTemplateName() {
        return templateName;
    }

    public String getTemplateVersion() {
        return templateVersion;
    }

    public String getMarkerPath() {
        return markerPath;
    }

    public String getMarkerRevision() {
        return markerRevision;
    }

    /** The validated, defaulted values. Empty for a rejected action. */
    public Map<String, Object> getValues() {
        return values;
    }

    public boolean isRejected() {
        return rejected;
    }

    /** The rejection reason. Empty when {@link #isRejected} is false. */
    public String getMessage() {
        return message;
    }

    @Override
    public void onAttached(Run<?, ?> r) {
        VarroaRunRecorder.releaseHeldStartedEvent(r, this);
    }

    @Override
    public void onLoad(Run<?, ?> r) {
        // No side effect: a run reloaded from disk already sent (or
        // released) its STARTED event during the run that attached this
        // action.
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }
}
