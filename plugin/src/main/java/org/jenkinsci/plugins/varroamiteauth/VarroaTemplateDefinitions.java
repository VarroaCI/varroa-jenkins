package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Job;

import java.util.Set;

import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;

/**
 * The Varroa flow definitions that own their own script. A build of such a
 * job reads a governed template, so a replay may not replace its script and
 * the STARTED event waits for the template action (D8).
 *
 * <p>The build recorder and the replay route both read this class, so the
 * two readers cannot disagree about which job is template defined.
 */
final class VarroaTemplateDefinitions {

    /**
     * The two {@code FlowDefinition} classes that attach a {@link
     * org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateValuesAction}.
     * Named by class rather than by type: this package cannot hold a
     * compile dependency on the template package's flow definitions. Keep
     * this set in sync with every FlowDefinition that attaches the action.
     */
    private static final Set<String> DEFAULT_CLASS_NAMES = Set.of(
            "org.jenkinsci.plugins.varroamiteauth.template.VarroaMarkerFlowDefinition",
            "org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateFlowDefinition");

    private static volatile Set<String> classNamesOverrideForTesting;

    private VarroaTemplateDefinitions() {
    }

    /**
     * Reports whether {@code job} is defined by a Varroa template flow
     * definition. A job that is not a {@link WorkflowJob}, or that holds no
     * definition, is not template defined.
     */
    static boolean isTemplateDefined(Job<?, ?> job) {
        if (!(job instanceof WorkflowJob)) {
            return false;
        }
        FlowDefinition definition = ((WorkflowJob) job).getDefinition();
        if (definition == null) {
            return false;
        }
        Set<String> names = classNamesOverrideForTesting;
        if (names == null) {
            names = DEFAULT_CLASS_NAMES;
        }
        return names.contains(definition.getClass().getName());
    }

    /**
     * Registers the {@code FlowDefinition} class names {@link
     * #isTemplateDefined} reads, replacing the default set. Pass {@code
     * null} to restore the default set. Package-private for testing: a test
     * double stands in for the real flow definitions, which this package
     * does not depend on.
     */
    static void setClassNamesForTesting(Set<String> names) {
        classNamesOverrideForTesting = names;
    }
}
