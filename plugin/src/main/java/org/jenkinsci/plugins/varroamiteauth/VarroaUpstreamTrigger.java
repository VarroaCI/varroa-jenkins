package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Item;
import hudson.model.Job;
import hudson.triggers.Trigger;
import hudson.triggers.TriggerDescriptor;

import java.util.List;

import org.jenkinsci.Symbol;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

/**
 * {@code varroaUpstream} (D9): declares that this job runs after a named
 * job on another (or the caller's own) tenant reaches a result at or above
 * a threshold.
 *
 * <p>A catalog item declares the same relationship through the items YAML
 * key {@code varroaUpstream}; this class is the Pipeline-syntax path,
 * reached through the {@code varroaUpstream} symbol in a declarative
 * {@code triggers} block or a scripted {@code pipelineTriggers([...])}
 * step.
 *
 * <p>This trigger starts no timer and polls nothing. The recorder's job-
 * schedule export finds the declaration and fans a build out directly when
 * the named job finishes; no code in this class fires a build.
 */
public class VarroaUpstreamTrigger extends Trigger<Job<?, ?>> {

    /** The allowed result thresholds, in the order the error message lists them. */
    static final List<String> ALLOWED_RESULTS = List.of("SUCCESS", "UNSTABLE", "FAILURE");

    /** The threshold {@link #setResult(String)} applies when the caller sets none. */
    static final String DEFAULT_RESULT = "SUCCESS";

    private final String tenant;
    private final String job;
    private String result = DEFAULT_RESULT;

    @DataBoundConstructor
    public VarroaUpstreamTrigger(String tenant, String job) {
        this.tenant = tenant != null ? tenant : "";
        this.job = job != null ? job : "";
    }

    public String getTenant() {
        return tenant;
    }

    public String getJob() {
        return job;
    }

    public String getResult() {
        return result;
    }

    /**
     * Sets the result threshold. Rejects a value outside {@link #ALLOWED_RESULTS}.
     * Pipeline data binding runs no form validation, so this setter is the only
     * place that can refuse a bad value.
     */
    @DataBoundSetter
    public void setResult(String result) {
        String value = (result == null || result.isEmpty()) ? DEFAULT_RESULT : result;
        if (!ALLOWED_RESULTS.contains(value)) {
            throw new IllegalArgumentException(
                    "varroaUpstream: result must be one of " + ALLOWED_RESULTS + ", got \"" + value + "\"");
        }
        this.result = value;
    }

    @Extension
    @Symbol("varroaUpstream")
    public static final class DescriptorImpl extends TriggerDescriptor {
        @Override
        public boolean isApplicable(Item item) {
            return item instanceof Job;
        }

        @Override
        public String getDisplayName() {
            return "Varroa upstream trigger";
        }
    }
}
