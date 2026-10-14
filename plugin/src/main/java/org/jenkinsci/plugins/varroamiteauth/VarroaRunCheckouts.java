package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import hudson.model.Job;
import hudson.model.Run;
import jenkins.branch.Branch;
import jenkins.branch.MultiBranchProject;
import jenkins.scm.api.SCMSource;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty;

import java.util.ArrayList;
import java.util.List;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Picks the git checkout that is the run's own repository. A shared
 * library checkout can precede the job's checkout of its own repository,
 * so the first git checkout of a run is not always the job's. The job's
 * SCM names that repository. A job with no SCM, such as an inline
 * script, takes the first checkout that is no library checkout.
 *
 * <p>The git plugin and the pipeline-groovy-lib are not compile
 * dependencies, so their reads are reflective. Every method answers an
 * empty value or null when a read fails.
 */
final class VarroaRunCheckouts {

    private static final Logger LOGGER = Logger.getLogger(VarroaRunCheckouts.class.getName());

    private VarroaRunCheckouts() {
    }

    /**
     * Answers the SCM of the run's job. A branch job answers its branch's
     * SCM. Any other workflow job answers the SCM of its definition, when
     * the definition declares one. Answers null when neither exists.
     */
    static Object jobScm(Run<?, ?> run) {
        try {
            Job<?, ?> parent = run.getParent();
            BranchJobProperty property = parent.getProperty(BranchJobProperty.class);
            if (property != null) {
                Branch branch = property.getBranch();
                if (branch != null && branch.getScm() != null) {
                    return branch.getScm();
                }
            }
            if (parent instanceof WorkflowJob) {
                Object definition = ((WorkflowJob) parent).getDefinition();
                return definition == null ? null : VarroaRunRecorder.invokeNoArg(definition, "getScm");
            }
            return null;
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunCheckouts: the job SCM read failed", e);
            return null;
        }
    }

    /**
     * Answers the simple class name of the SCM source that owns the run's
     * job. A branch job of a multibranch project answers its source class.
     * Answers an empty string when no source owns the job.
     */
    static String jobSourceClassName(Run<?, ?> run) {
        try {
            Job<?, ?> parent = run.getParent();
            BranchJobProperty property = parent.getProperty(BranchJobProperty.class);
            Branch branch = property == null ? null : property.getBranch();
            if (branch == null || !(parent.getParent() instanceof MultiBranchProject)) {
                return "";
            }
            SCMSource source = ((MultiBranchProject<?, ?>) parent.getParent()).getSCMSource(branch.getSourceId());
            return source == null ? "" : source.getClass().getSimpleName();
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunCheckouts: the job source read failed", e);
            return "";
        }
    }

    /**
     * Answers the forge that hosts a run's revision. The multibranch source
     * class names the forge first. The host of the URL names it otherwise.
     * Answers an empty string when neither names one.
     */
    static String forgeFor(String sourceSimpleName, String url) {
        String forge = VarroaLibrarySources.forgeFromSourceClass(sourceSimpleName == null ? "" : sourceSimpleName);
        return forge.isEmpty() ? VarroaLibrarySources.forgeForHost(VarroaLibrarySources.hostOf(url)) : forge;
    }

    /** Answers the raw remote of the run's job SCM. Answers an empty string when none exists. */
    static String jobRemote(Run<?, ?> run) {
        try {
            return VarroaLibrarySources.remoteOf(jobScm(run));
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunCheckouts: the job remote read failed", e);
            return "";
        }
    }

    /**
     * Answers the raw remote of every library the run loaded that has a
     * configured repository. A library with no configuration adds nothing.
     */
    static List<String> libraryRemotes(Run<?, ?> run) {
        List<String> remotes = new ArrayList<>();
        try {
            Object librariesAction = null;
            for (Action action : run.getAllActions()) {
                if (action != null && VarroaRunRecorder.hasSimpleNameInHierarchy(action.getClass(), "LibrariesAction")) {
                    librariesAction = action;
                    break;
                }
            }
            if (librariesAction == null) {
                return remotes;
            }
            Object records = VarroaRunRecorder.invokeNoArg(librariesAction, "getLibraries");
            if (!(records instanceof Iterable)) {
                return remotes;
            }
            for (Object record : (Iterable<?>) records) {
                Object nameValue = VarroaRunRecorder.invokeNoArg(record, "getName");
                String name = nameValue instanceof String ? (String) nameValue : "";
                Object trustedFlag = VarroaRunRecorder.invokeNoArg(record, "isTrusted");
                Boolean trusted = trustedFlag instanceof Boolean ? (Boolean) trustedFlag : null;
                VarroaLibrarySources.Source source = VarroaLibrarySources.sourceFor(run, name, trusted);
                if (source != null && !source.rawRemote.isEmpty()) {
                    remotes.add(source.rawRemote);
                }
            }
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunCheckouts: the library remote read failed", e);
        }
        return remotes;
    }

    /**
     * Picks the checkout that is the run's repository from the run's git
     * checkouts, in action order. A non-empty job remote picks the first
     * checkout that names that remote, or null when none does. An empty
     * job remote picks the first checkout that names no library remote,
     * or null when none does. The library remotes are read only on that
     * second path.
     */
    static Object select(String jobRemote, Supplier<List<String>> libraryRemotes, List<Object> buildDatas) {
        if (buildDatas == null || buildDatas.isEmpty()) {
            return null;
        }
        if (jobRemote != null && !jobRemote.isEmpty()) {
            String want = VarroaLibrarySources.normalizeForMatch(jobRemote);
            for (Object buildData : buildDatas) {
                if (VarroaLibrarySources.remotesContain(buildData, want)) {
                    return buildData;
                }
            }
            return null;
        }
        List<String> libraries = libraryRemotes.get();
        for (Object buildData : buildDatas) {
            if (!namesALibrary(buildData, libraries)) {
                return buildData;
            }
        }
        return null;
    }

    /** Answers whether a checkout names any of the library remotes. */
    private static boolean namesALibrary(Object buildData, List<String> libraryRemotes) {
        for (String remote : libraryRemotes) {
            if (VarroaLibrarySources.remotesContain(buildData, VarroaLibrarySources.normalizeForMatch(remote))) {
                return true;
            }
        }
        return false;
    }
}
