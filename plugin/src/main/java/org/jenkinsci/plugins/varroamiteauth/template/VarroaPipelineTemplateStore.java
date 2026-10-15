package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.Extension;
import hudson.model.AbstractDescribableImpl;
import hudson.model.Descriptor;
import jenkins.model.GlobalConfiguration;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

import org.jenkinsci.Symbol;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

/**
 * Holds the pipeline template set the operator delivers through JCasC (D7,
 * D8). JCasC symbol {@code varroaPipelineTemplates}, key {@code templates}.
 *
 * <p>{@link #resolve} applies two rules together: D5's version resolution
 * (the highest delivered version matching a marker's constraint) and D7's
 * folder-scoping rule (a template resolves only when its {@code folder} is
 * empty, meaning controller-wide, or equals the requesting job's tenant
 * folder).
 */
@Extension
@Symbol("varroaPipelineTemplates")
public class VarroaPipelineTemplateStore extends GlobalConfiguration {

    private List<TemplateEntry> templates = new ArrayList<>();

    public VarroaPipelineTemplateStore() {
        load();
    }

    /** Returns the singleton instance, the way every {@code GlobalConfiguration} is looked up. */
    public static VarroaPipelineTemplateStore get() {
        return GlobalConfiguration.all().get(VarroaPipelineTemplateStore.class);
    }

    public List<TemplateEntry> getTemplates() {
        return Collections.unmodifiableList(templates);
    }

    @DataBoundSetter
    public void setTemplates(List<TemplateEntry> templates) {
        this.templates = templates != null ? new ArrayList<>(templates) : new ArrayList<>();
        save();
    }

    /**
     * Resolves {@code name} against {@code constraint} (D5), scoped to
     * {@code folderFullName} (D7): a candidate template resolves only when
     * its own {@code folder} is empty or equals {@code folderFullName}.
     * Never fetches a template outside the delivered set.
     *
     * @param folderFullName the requesting job's tenant folder; the empty
     *                        string for a controller with no tenant folder
     * @param name            the marker's {@code template} value
     * @param constraint      the marker's {@code templateVersion} value, or
     *                        the empty string for no constraint
     * @throws TemplateResolutionException naming the reason: the template
     *         is not delivered at all, the template is not delivered to
     *         the requesting folder, or no delivered version matches the
     *         constraint
     */
    public TemplateEntry resolve(String folderFullName, String name, String constraint)
            throws TemplateResolutionException {
        String folder = folderFullName != null ? folderFullName : "";
        List<TemplateEntry> named = new ArrayList<>();
        for (TemplateEntry t : templates) {
            if (name.equals(t.getName())) {
                named.add(t);
            }
        }
        if (named.isEmpty()) {
            throw new TemplateResolutionException("template \"" + name + "\" is not delivered");
        }

        List<TemplateEntry> scoped = new ArrayList<>();
        for (TemplateEntry t : named) {
            String f = t.getFolder();
            if (f == null || f.isEmpty() || f.equals(folder)) {
                scoped.add(t);
            }
        }
        if (scoped.isEmpty()) {
            throw new TemplateResolutionException(
                    "template \"" + name + "\" is not delivered to \"" + folder + "\"");
        }

        TemplateVersion.Constraint c;
        try {
            c = TemplateVersion.parseConstraint(constraint != null ? constraint : "");
        } catch (IllegalArgumentException e) {
            throw new TemplateResolutionException(e.getMessage());
        }

        TemplateEntry best = null;
        TemplateVersion bestVersion = null;
        List<String> deliveredVersions = new ArrayList<>();
        for (TemplateEntry t : scoped) {
            TemplateVersion v;
            try {
                v = TemplateVersion.parse(t.getVersion());
            } catch (IllegalArgumentException e) {
                continue;
            }
            deliveredVersions.add(t.getVersion());
            if (!c.matches(v)) {
                continue;
            }
            if (best == null || v.compareTo(bestVersion) > 0) {
                best = t;
                bestVersion = v;
            }
        }
        if (best == null) {
            throw new TemplateResolutionException("no delivered version of \"" + name + "\" matches \""
                    + c + "\", delivered: " + String.join(", ", deliveredVersions));
        }
        return best;
    }

    /**
     * Resolves {@code entry}'s {@code scmDefinition.credentialsId} in the
     * credential store its {@code folder} allows (D7): only that tenant
     * folder's own store when {@code folder} names a tenant, or the
     * system store when {@code folder} is empty. Fails rather than
     * widening the search to an ancestor store. A no-op when {@code
     * entry} carries no {@code scmDefinition} or an empty {@code
     * credentialsId}.
     */
    public void resolveCredentialScope(TemplateEntry entry) throws TemplateResolutionException {
        ScmDefinitionEntry scm = entry.getScmDefinition();
        if (scm == null) {
            return;
        }
        TemplateCredentialResolver.Result result =
                TemplateCredentialResolver.resolve(entry.getFolder(), scm.getCredentialsId());
        if (!result.found) {
            throw new TemplateResolutionException(result.message);
        }
    }

    @Override
    public String getDisplayName() {
        return "Varroa Pipeline Templates";
    }

    /** A resolution failure names the exact reason: D5's or D7's rule that failed. */
    public static final class TemplateResolutionException extends Exception {
        public TemplateResolutionException(String message) {
            super(message);
        }
    }

    /** One delivered template (D7's JCasC fragment shape). */
    public static final class TemplateEntry extends AbstractDescribableImpl<TemplateEntry> {

        private final String name;
        private final String version;
        private String folder = "";
        private String parameters = "";
        private String uiSchema = "";
        private String jenkinsfile = "";
        private ScmDefinitionEntry scmDefinition;

        @DataBoundConstructor
        public TemplateEntry(String name, String version) {
            this.name = name;
            this.version = version;
        }

        public String getName() {
            return name;
        }

        public String getVersion() {
            return version;
        }

        public String getFolder() {
            return folder;
        }

        @DataBoundSetter
        public void setFolder(String folder) {
            this.folder = folder != null ? folder : "";
        }

        /** The template's {@code spec.parameters} JSON Schema, as YAML text (D7). */
        public String getParameters() {
            return parameters;
        }

        @DataBoundSetter
        public void setParameters(String parameters) {
            this.parameters = parameters != null ? parameters : "";
        }

        /** The template's {@code spec.uiSchema}, as YAML text (D7). */
        public String getUiSchema() {
            return uiSchema;
        }

        @DataBoundSetter
        public void setUiSchema(String uiSchema) {
            this.uiSchema = uiSchema != null ? uiSchema : "";
        }

        /** The template's {@code spec.jenkinsfile}. Empty when {@link #getScmDefinition} is set. */
        public String getJenkinsfile() {
            return jenkinsfile;
        }

        @DataBoundSetter
        public void setJenkinsfile(String jenkinsfile) {
            this.jenkinsfile = jenkinsfile != null ? jenkinsfile : "";
        }

        /** The template's {@code spec.scmDefinition}. {@code null} when {@link #getJenkinsfile} is set. */
        public ScmDefinitionEntry getScmDefinition() {
            return scmDefinition;
        }

        @DataBoundSetter
        public void setScmDefinition(ScmDefinitionEntry scmDefinition) {
            this.scmDefinition = scmDefinition;
        }

        @Extension
        public static final class DescriptorImpl extends Descriptor<TemplateEntry> {
            @Override
            public String getDisplayName() {
                return "Pipeline Template";
            }
        }
    }

    /** A template's {@code spec.scmDefinition}: a script in the template repository. */
    public static final class ScmDefinitionEntry extends AbstractDescribableImpl<ScmDefinitionEntry> {

        private final String remote;
        private final String scriptPath;
        private String credentialsId = "";
        private String revision = "";

        @DataBoundConstructor
        public ScmDefinitionEntry(String remote, String scriptPath) {
            this.remote = remote;
            this.scriptPath = scriptPath;
        }

        public String getRemote() {
            return remote;
        }

        public String getScriptPath() {
            return scriptPath;
        }

        public String getCredentialsId() {
            return credentialsId;
        }

        @DataBoundSetter
        public void setCredentialsId(String credentialsId) {
            this.credentialsId = credentialsId != null ? credentialsId : "";
        }

        public String getRevision() {
            return revision;
        }

        @DataBoundSetter
        public void setRevision(String revision) {
            this.revision = revision != null ? revision : "";
        }

        @Extension
        public static final class DescriptorImpl extends Descriptor<ScmDefinitionEntry> {
            @Override
            public String getDisplayName() {
                return "SCM Definition";
            }
        }
    }
}
