package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.Failure;
import hudson.model.FileParameterDefinition;
import hudson.model.Item;
import hudson.model.Job;
import hudson.model.Node;
import hudson.model.ParameterDefinition;
import hudson.model.ParameterValue;
import hudson.model.ParametersAction;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.Queue;
import hudson.model.RootAction;
import hudson.model.Run;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.lang.reflect.Method;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Collectors;

import jenkins.branch.MultiBranchProject;
import jenkins.model.Jenkins;
import jenkins.model.ParameterizedJobMixIn;
import jenkins.model.lazy.LazyBuildMixIn;

import org.jenkinsci.plugins.workflow.cps.replay.ReplayAction;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionList;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.kohsuke.stapler.WebMethod;

import org.springframework.security.core.GrantedAuthority;

/**
 * Protected {@link RootAction} at {@code /varroa-run}, the mite's local call for a
 * manual build trigger, a manual build cancel, the two input-approval commands, and
 * a replay with an edited script (D3, D4, D8, D10, D11, run-trigger-endpoint).
 *
 * <p>Requires the authority {@code ROLE:varroa:system-mite}, the same check {@link
 * ActivityEndpoint} performs. Only the mite calls these routes, over the Jenkins
 * bearer token the operator signs.
 *
 * <p>Every refusal goes through {@link PlainTextRefusal#refuse}, so its body is the plain
 * message and never the Jenkins HTML error page.
 *
 * <p>{@code POST /varroa-run/load-build} loads one numbered build, because Jenkins
 * loads builds on demand and a build restored onto disk is otherwise invisible.
 *
 * <p>A route whose name holds more than one word carries {@link WebMethod}. Stapler
 * derives a URL token from the method name and never writes a dash, so {@code
 * doLoadBuild} would serve {@code loadBuild} and the dashed name every caller and
 * every specification uses would answer {@code 404}.
 *
 * <p>{@code POST /varroa-run/trigger} stamps a caller-supplied run identifier and
 * intent key onto the queue item before it schedules, so the claim gate observes the
 * caller's key instead of minting its own. {@code POST /varroa-run/cancel} resolves
 * its target by run identifier, never by job path alone, because one job can hold
 * several queued items and several running builds.
 *
 * <p>{@code POST /varroa-run/proceed-input} answers one pending {@code input} step as
 * the acting user, and {@code POST /varroa-run/abort-input} rejects one. Both read
 * the acting user from the body, because the request authenticates the mite and never
 * the person, and both run every Jenkins call after the resolution inside that user's
 * own authentication.
 *
 * <p>{@code POST /varroa-run/replay} schedules one build of a Pipeline job with a
 * caller-supplied script, as the acting user. It refuses a template-defined job,
 * because a template owns its own script. The route name holds one word, so it
 * carries no {@link WebMethod}.
 *
 * <p>{@code POST /varroa-run/restart} reruns a completed declarative build from one
 * stage, as the acting user. It refuses the restart when the source build is no
 * longer on this cell, when the job's home moved off this cell, and when the stage
 * is not restartable. The route name holds one word, so it carries no {@link
 * WebMethod}.
 */
@Extension
public class VarroaRunTriggerAction implements RootAction {

    private static final Logger LOGGER = Logger.getLogger(VarroaRunTriggerAction.class.getName());

    @Override
    public String getUrlName() {
        return "varroa-run";
    }

    @Override
    public String getIconFileName() {
        return null; // no UI surface
    }

    @Override
    public String getDisplayName() {
        return null; // no UI surface
    }

    /**
     * Handles {@code POST /varroa-run/trigger}. Reads {@code runId}, {@code
     * intentKey}, {@code jobPath}, {@code parameters}, and {@code actor},
     * normalizes the actor through {@link VarroaUserCause#normalizeActor},
     * narrows the named item to a parameterized job, builds a {@link
     * ParametersAction} from its declared definitions, stamps a {@link
     * VarroaIntentAction} carrying the caller's identity, attaches a {@link
     * VarroaUserCause} for a non-empty actor, and schedules through {@link
     * ParameterizedJobMixIn}.
     */
    public void doTrigger(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String intentKey = body.optString("intentKey", "");
            String jobPath = body.optString("jobPath", "");
            // The body supplies the actor, so the route normalizes it once.
            // The intent action and the Varroa cause then carry one value.
            String actor = VarroaUserCause.normalizeActor(body.optString("actor", ""));
            JSONObject parameters = body.optJSONObject("parameters");

            // Look the item up untyped first: Jenkins.getItemByFullName(path, Job.class)
            // answers null both for a missing item and for one that exists but is not
            // a Job (e.g. a folder), and those two cases carry different response codes.
            Item rawItem = Jenkins.get().getItemByFullName(jobPath);
            if (rawItem == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no item named " + jobPath);
                return;
            }
            if (!(rawItem instanceof Job) || !(rawItem instanceof ParameterizedJobMixIn.ParameterizedJob)) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is a " + rawItem.getClass().getName()
                        + ", not a parameterized job");
                return;
            }
            Job<?, ?> job = (Job<?, ?>) rawItem;

            List<String> droppedParameters = new ArrayList<>();
            List<Action> actions = new ArrayList<>();
            actions.add(buildParametersAction(job, req, parameters, droppedParameters));
            actions.add(stampedIntentAction(runId, intentKey, actor));
            if (!actor.isEmpty()) {
                // The Varroa cause names the actor itself. A UserIdCause
                // answers "anonymous" on a headless cell, which holds no
                // Jenkins User record for the actor.
                actions.add(new CauseAction(new VarroaUserCause(actor)));
            }

            // A redelivered command can carry the run id of a build that
            // already left the queue. The queue fold cannot see that build,
            // so the route schedules nothing and answers as it did the first time.
            Run<?, ?> existing = findBuildForRunId(job, runId);
            if (existing != null) {
                logRepeat("trigger", runId, existing);
                writeAccepted(rsp, runId, droppedParameters);
                return;
            }

            Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(job, 0, actions.toArray(new Action[0]));
            if (item == null) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: the queue refused to schedule " + jobPath);
                return;
            }

            writeAccepted(rsp, runId, droppedParameters);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doTrigger caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/replay} (D8). Reads {@code runId},
     * {@code intentKey}, {@code jobPath}, {@code sourceRunId}, {@code script},
     * {@code parameters}, and {@code actingUser}, narrows the named item to a
     * parameterized Pipeline job that no Varroa template defines, and then,
     * inside the acting user's own authentication, checks {@link
     * ReplayAction#REPLAY} on the job and schedules one build.
     *
     * <p>The build carries the parameters action, a stamped {@link
     * VarroaIntentAction}, a cause action holding a {@link VarroaUserCause}
     * and a {@link VarroaReplayCause}, and a {@link
     * VarroaReplayFlowFactoryAction} over the supplied script. The route
     * trusts no sandbox flag from the body: the execution is always
     * sandboxed.
     *
     * <p>The route name holds one word, so Stapler derives it from the method
     * name and the method carries no {@link WebMethod}.
     */
    public void doReplay(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String intentKey = body.optString("intentKey", "");
            String jobPath = body.optString("jobPath", "");
            String sourceRunId = body.optString("sourceRunId", "");
            String script = body.optString("script", "");
            JSONObject parameters = body.optJSONObject("parameters");
            // The request authenticates the mite, so the acting user comes
            // from the body alone.
            VarroaActingUser actingUser = VarroaActingUser.fromJson(body);
            // The record's replay_of value comes from sourceRunId, so a
            // replay that names no source run is refused.
            if (runId.isEmpty() || sourceRunId.isEmpty() || jobPath.isEmpty() || script.isEmpty()
                    || actingUser == null) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no run, no source run, no job, no script,"
                        + " or no acting user");
                return;
            }

            // Look the item up untyped: a missing item and an item of the
            // wrong kind carry different response codes.
            Item rawItem = Jenkins.get().getItemByFullName(jobPath);
            if (rawItem == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no item named " + jobPath);
                return;
            }
            if (!(rawItem instanceof Job) || !(rawItem instanceof ParameterizedJobMixIn.ParameterizedJob)) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is a " + rawItem.getClass().getName()
                        + ", not a parameterized job");
                return;
            }
            if (!(rawItem instanceof WorkflowJob)) {
                // A replay replaces a Groovy script, and a job of another
                // kind holds none.
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is not a pipeline job");
                return;
            }
            Job<?, ?> job = (Job<?, ?>) rawItem;
            if (VarroaTemplateDefinitions.isTemplateDefined(job)) {
                // A template definition builds through a core definition that
                // honours the replay factory action. A replay would therefore
                // replace the governed template with the caller's script.
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is template defined");
                return;
            }

            String actor = VarroaUserCause.normalizeActor(actingUser.getName());
            List<String> droppedParameters = new ArrayList<>();

            // A redelivered replay can name a build that already left the
            // queue. The check runs before the permission check, because a
            // repeat schedules nothing.
            Run<?, ?> existing = findBuildForRunId(job, runId);
            if (existing != null) {
                try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                    buildParametersAction(job, req, parameters, droppedParameters);
                }
                logRepeat("replay", runId, existing);
                writeAccepted(rsp, runId, droppedParameters);
                return;
            }

            try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                // The check reads the job, not the source build: a cell is
                // disposable, and the source build can be gone from it.
                if (!job.hasPermission(ReplayAction.REPLAY)) {
                    PlainTextRefusal.refuse(rsp, 403, "Forbidden: the acting user may not replay this job");
                    return;
                }

                List<Action> actions = new ArrayList<>();
                actions.add(buildParametersAction(job, req, parameters, droppedParameters));
                actions.add(stampedIntentAction(runId, intentKey, actor));
                List<Cause> causes = new ArrayList<>();
                if (!actor.isEmpty()) {
                    causes.add(new VarroaUserCause(actor));
                }
                causes.add(new VarroaReplayCause(sourceRunId));
                actions.add(new CauseAction(causes.toArray(new Cause[0])));
                actions.add(new VarroaReplayFlowFactoryAction(script));

                Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(job, 0, actions.toArray(new Action[0]));
                if (item == null) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the queue refused to schedule " + jobPath);
                    return;
                }
            }

            writeAccepted(rsp, runId, droppedParameters);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doReplay caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/scan}. Reads {@code scanId}, {@code
     * jobPath}, and {@code actingUser}, narrows the named item to a
     * multibranch project, and then, inside the acting user's own
     * authentication, checks {@link Item#BUILD} and schedules one folder
     * computation. {@link VarroaBranchScans} records the scan before the
     * computation starts and reports its result.
     *
     * <p>Answers {@code 405} for a method other than {@code POST}, {@code 403}
     * without the mite authority or for an acting user without {@link Item#BUILD}
     * (the refusal is recorded), {@code 400} for a body that does not decode or
     * names no scan, job, or acting user, {@code 404} for no item at the job path,
     * {@code 409} for an item that is not a multibranch project or a queue that
     * refuses the computation, and {@code 202} on success.
     */
    public void doScan(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String scanId = body.optString("scanId", "");
            String jobPath = body.optString("jobPath", "");
            VarroaActingUser actingUser = VarroaActingUser.fromJson(body);
            if (scanId.isEmpty() || jobPath.isEmpty() || actingUser == null) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no scan, no job, or no acting user");
                return;
            }

            Item rawItem = Jenkins.get().getItemByFullName(jobPath);
            if (rawItem == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no item named " + jobPath);
                return;
            }
            if (!(rawItem instanceof MultiBranchProject)) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is not a multibranch project");
                return;
            }
            MultiBranchProject project = (MultiBranchProject) rawItem;
            String actor = VarroaUserCause.normalizeActor(actingUser.getName());

            // The first request of a scan reserves its identifier, atomically.
            // A redelivered request then answers 202 before the permission
            // check, so a permission change made after the first accept cannot
            // overwrite the running scan, and no second computation starts.
            int claimed = VarroaBranchScans.claim(jobPath, scanId);
            if (claimed == VarroaBranchScans.CLAIM_IN_FLIGHT) {
                // The first request has not decided yet. Answering 202 here
                // would acknowledge a scan that may still fail.
                PlainTextRefusal.refuse(rsp, 409, "Conflict: the scan " + scanId + " is already being requested");
                return;
            }
            if (claimed == VarroaBranchScans.CLAIM_ACCEPTED) {
                JSONObject repeat = new JSONObject();
                repeat.put("scanId", scanId);
                rsp.setContentType("application/json");
                rsp.setStatus(202);
                rsp.getWriter().write(repeat.toString());
                return;
            }

            boolean queued = false;
            try {
            try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                // Every refusal below releases the reservation through
                // recordRefused, so a later delivery of the same scan can retry.
                if (!project.hasPermission(Item.BUILD)) {
                    VarroaBranchScans.recordRefused(jobPath, scanId, actor,
                            "the acting user may not scan this job");
                    PlainTextRefusal.refuse(rsp, 403, "Forbidden: the acting user may not scan this job");
                    return;
                }
                List<Action> actions = new ArrayList<>();
                if (!actor.isEmpty()) {
                    actions.add(new CauseAction(new VarroaUserCause(actor)));
                }
                VarroaBranchScans.arm(project, scanId, actor);
                Object scheduled = project.scheduleBuild2(0, actions.toArray(new Action[0]));
                if (scheduled == null) {
                    VarroaBranchScans.recordRefused(jobPath, scanId, actor,
                            "the queue refused to scan " + jobPath);
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the queue refused to scan " + jobPath);
                    return;
                }
                queued = true;
                VarroaBranchScans.markAccepted(jobPath, scanId);
            }
            } catch (RuntimeException | Error e) {
                // A failure before the queue took the scan releases the
                // reservation, so a later delivery can retry. After that, the
                // scan is accepted and its reservation stays.
                if (!queued) {
                    VarroaBranchScans.recordRefused(jobPath, scanId, actor,
                            "the scan failed before the queue answered");
                }
                throw e;
            }

            JSONObject out = new JSONObject();
            out.put("scanId", scanId);
            rsp.setContentType("application/json");
            rsp.setStatus(202);
            rsp.getWriter().write(out.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doScan caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/restart}. Reruns a completed declarative
     * build from one stage, as the acting user, and keeps the parameter values
     * of the source build.
     *
     * <p>The source build must still sit on this cell. The route refuses when
     * the job's home moved to another slot, when the source number no longer
     * names a build with the source run identifier, when the source build has
     * not ended, when the run does not restart, and when the stage is not in the
     * restartable list. A repeated run identifier schedules nothing and answers
     * {@code 202}.
     *
     * <p>The route builds the restart objects itself. Jenkins' own restart
     * method schedules the build without a {@link VarroaIntentAction}, so the
     * claim gate could not see the run identity.
     */
    public void doRestart(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String intentKey = body.optString("intentKey", "");
            String jobPath = body.optString("jobPath", "");
            String sourceRunId = body.optString("sourceRunId", "");
            String stage = body.optString("stage", "");
            long sourceBuildNumber = body.optLong("sourceBuildNumber", 0L);
            VarroaActingUser actingUser = VarroaActingUser.fromJson(body);
            if (runId.isEmpty() || jobPath.isEmpty() || sourceRunId.isEmpty() || stage.isEmpty()
                    || sourceBuildNumber <= 0 || sourceBuildNumber > Integer.MAX_VALUE || actingUser == null) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no run, no job, no source run,"
                        + " no source build number, no stage, or no acting user");
                return;
            }

            Item rawItem = Jenkins.get().getItemByFullName(jobPath);
            if (rawItem == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no item named " + jobPath);
                return;
            }
            if (!(rawItem instanceof WorkflowJob)) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is not a pipeline job");
                return;
            }
            Job<?, ?> job = (Job<?, ?>) rawItem;
            if (VarroaTemplateDefinitions.isTemplateDefined(job)) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: " + jobPath + " is template defined");
                return;
            }

            String actor = VarroaUserCause.normalizeActor(actingUser.getName());

            // A redelivered restart can name a build that already left the
            // queue. The check runs before the home check, because a repeat
            // schedules nothing.
            Run<?, ?> existing = findBuildForRunId(job, runId);
            if (existing != null) {
                logRepeat("restart", runId, existing);
                writeAccepted(rsp, runId, List.of());
                return;
            }

            // A redelivered restart can also name a queued item that no build
            // carries yet. The restart flow factory action does not suppress
            // a second item, so the queue is checked here.
            Queue.Item queued = queuedItemForRunId(runId);
            if (queued != null) {
                LOGGER.log(Level.INFO, "VarroaRunTriggerAction: restart for run {0} repeats a queued item; scheduled nothing",
                        new Object[] {runId});
                writeAccepted(rsp, runId, List.of());
                return;
            }

            if (VarroaHomeCache.get().forwardSlotFor(jobPath).isPresent()) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: the home of " + jobPath + " moved off this cell");
                return;
            }

            // The acting user's own rights decide every check below. The
            // declarative restart check reads Item.BUILD of the current
            // identity, so the inspection must run as the acting user too.
            try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                if (!job.hasPermission(Item.BUILD)) {
                    PlainTextRefusal.refuse(rsp, 403, "Forbidden: the acting user may not build this job");
                    return;
                }

                int number = (int) sourceBuildNumber;
                Run<?, ?> origin = job.getBuildByNumber(number);
                VarroaIntentAction originIntent = origin == null ? null : origin.getAction(VarroaIntentAction.class);
                if (origin == null || originIntent == null || !sourceRunId.equals(originIntent.getRunId())) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: build " + number + " of " + jobPath
                            + " is no longer on this cell");
                    return;
                }
                if (origin.isBuilding()) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the source build has not ended");
                    return;
                }

                VarroaDeclarativeRestart.Outcome outcome = VarroaDeclarativeRestart.inspect(origin);
                if (outcome.getRefusal() != null) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: " + outcome.getRefusal());
                    return;
                }
                if (!outcome.getStages().contains(stage)) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: stage " + stage + " is not restartable");
                    return;
                }

                List<ParameterValue> sourceValues = new ArrayList<>();
                ParametersAction sourceParameters = origin.getAction(ParametersAction.class);
                if (sourceParameters != null) {
                    sourceValues.addAll(sourceParameters.getParameters());
                }

                List<Action> actions = new ArrayList<>();
                actions.add(new ParametersAction(sourceValues));
                actions.add(stampedIntentAction(runId, intentKey, actor));
                List<Cause> causes = new ArrayList<>();
                if (!actor.isEmpty()) {
                    causes.add(new VarroaUserCause(actor));
                }
                causes.add(VarroaDeclarativeRestart.newRestartCause(origin, stage));
                causes.add(new VarroaRestartCause(sourceRunId, stage));
                actions.add(new CauseAction(causes.toArray(new Cause[0])));
                actions.add(VarroaDeclarativeRestart.newRestartFlowFactoryAction(origin));

                Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(job, 0, actions.toArray(new Action[0]));
                if (item == null) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the queue refused to schedule " + jobPath);
                    return;
                }
            }

            writeAccepted(rsp, runId, List.of());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doRestart caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/cancel}. Reads {@code runId}, {@code
     * jobPath}, and {@code buildNumber}, then resolves the target by run
     * identifier: a numbered build is stopped only when its {@link
     * VarroaIntentAction} carries the same run identifier, and a zero build
     * number searches the queue instead.
     */
    public void doCancel(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String jobPath = body.optString("jobPath", "");
            long buildNumber = body.optLong("buildNumber", 0L);

            boolean cancelled;
            if (buildNumber > 0) {
                Job<?, ?> job = Jenkins.get().getItemByFullName(jobPath, Job.class);
                cancelled = job != null && cancelRunningBuild(job, runId, (int) buildNumber);
            } else {
                cancelled = cancelQueuedItem(runId);
            }

            if (!cancelled) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no build or queue item carries run id " + runId);
                return;
            }
            rsp.setStatus(202);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doCancel caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    // ---- Trigger helpers ----

    /** The most builds {@link #findBuildForRunId} reads, newest first. */
    static final int REPEAT_SCAN_MAX_BUILDS = 100;

    /**
     * The margin below the run id's own time at which {@link
     * #findBuildForRunId} stops. It covers clock skew between the minting
     * host and this Jenkins.
     */
    static final long REPEAT_SCAN_MARGIN_MILLIS = 5L * 60 * 1000;

    /**
     * Returns the build of {@code job} whose {@link VarroaIntentAction}
     * carries {@code runId}, or {@code null} when no build carries it.
     *
     * <p>The walk reads the newest build first. It stops after {@link
     * #REPEAT_SCAN_MAX_BUILDS} builds, and at the first build scheduled
     * more than {@link #REPEAT_SCAN_MARGIN_MILLIS} before the time the run
     * id carries: the run id is minted before its build is scheduled, so no
     * older build can carry it. A run id that is not a version-7 UUID
     * applies the count bound alone.
     */
    static Run<?, ?> findBuildForRunId(Job<?, ?> job, String runId) {
        if (runId == null || runId.isEmpty()) {
            return null;
        }
        long minted = Uuid7.timestampMillis(runId);
        long floor = minted < 0 ? Long.MIN_VALUE : minted - REPEAT_SCAN_MARGIN_MILLIS;
        int read = 0;
        for (Run<?, ?> run = job.getLastBuild(); run != null && read < REPEAT_SCAN_MAX_BUILDS;
                run = run.getPreviousBuild()) {
            read++;
            if (run.getTimeInMillis() < floor) {
                return null;
            }
            VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
            if (action != null && runId.equals(action.getRunId())) {
                return run;
            }
        }
        return null;
    }

    /**
     * Answers the queued item whose {@link VarroaIntentAction} carries {@code
     * runId}, or {@code null} when no queued item carries it.
     */
    static Queue.Item queuedItemForRunId(String runId) {
        for (Queue.Item item : Queue.getInstance().getItems()) {
            for (VarroaIntentAction action : item.getActions(VarroaIntentAction.class)) {
                if (runId.equals(action.getRunId())) {
                    return item;
                }
            }
        }
        return null;
    }

    /** Logs one info line for a repeated command that scheduled nothing. */
    private static void logRepeat(String route, String runId, Run<?, ?> existing) {
        LOGGER.log(Level.INFO, "VarroaRunTriggerAction: {0} for run {1} repeats build {2}; scheduled nothing",
                new Object[] {route, runId, existing.getExternalizableId()});
    }

    /** Writes the {@code 202} answer the trigger and replay routes share. */
    private static void writeAccepted(StaplerResponse2 rsp, String runId, List<String> droppedParameters)
            throws IOException {
        JSONObject out = new JSONObject();
        out.put("runId", runId);
        JSONArray dropped = new JSONArray();
        dropped.addAll(droppedParameters);
        out.put("droppedParameters", dropped);

        rsp.setContentType("application/json");
        rsp.setStatus(202);
        rsp.getWriter().write(out.toString());
    }

    /**
     * Stamps a fresh {@link VarroaIntentAction} carrying the caller's run
     * identifier, intent key, and actor. The action's origin is {@code
     * VARROA_INTENT}: the claim gate reads a non-empty intent key straight
     * off the action rather than deriving one (see {@link IntentKey#derive}).
     * The actor is the normalized value the caller supplied, even when empty.
     */
    private static VarroaIntentAction stampedIntentAction(String runId, String intentKey, String actor) {
        VarroaIntentAction action = new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
        action.setOrigin(IntentKey.ORIGIN_VARROA_INTENT);
        action.setIntentKey(intentKey);
        action.setActor(actor);
        return action;
    }

    /**
     * Builds a {@link ParametersAction} from the job's declared parameter
     * definitions: a supplied value it declares is bound through the
     * definition, an omitted one takes the declared default, and a supplied
     * name it does not declare is dropped and recorded in {@code dropped}.
     */
    private static ParametersAction buildParametersAction(Job<?, ?> job, StaplerRequest2 req,
            JSONObject suppliedParameters, List<String> dropped) {
        List<ParameterValue> values = new ArrayList<>();
        ParametersDefinitionProperty pp = job.getProperty(ParametersDefinitionProperty.class);
        if (pp == null) {
            if (suppliedParameters != null) {
                for (Object key : suppliedParameters.keySet()) {
                    dropped.add(String.valueOf(key));
                }
            }
            return new ParametersAction(values);
        }

        for (ParameterDefinition definition : pp.getParameterDefinitions()) {
            String name = definition.getName();
            if (suppliedParameters != null && suppliedParameters.has(name)) {
                JSONObject jo = new JSONObject();
                jo.put("name", name);
                jo.put("value", suppliedParameters.getString(name));
                ParameterValue value = definition.createValue(req, jo);
                if (value != null) {
                    values.add(value);
                }
            } else {
                ParameterValue defaultValue = definition.getDefaultParameterValue();
                if (defaultValue != null) {
                    values.add(defaultValue);
                }
            }
        }

        if (suppliedParameters != null) {
            for (Object key : suppliedParameters.keySet()) {
                String name = String.valueOf(key);
                if (pp.getParameterDefinition(name) == null) {
                    dropped.add(name);
                }
            }
        }
        return new ParametersAction(values);
    }

    // ---- Cancel helpers ----

    /**
     * Stops the numbered build only when it is still building and its {@link
     * VarroaIntentAction} carries the caller's run identifier. A build that
     * already ended, or one whose identity does not match, is left alone.
     */
    private static boolean cancelRunningBuild(Job<?, ?> job, String runId, int buildNumber) throws Exception {
        Run<?, ?> run = job.getBuildByNumber(buildNumber);
        if (run == null || !run.isBuilding()) {
            return false;
        }
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        if (action == null || !runId.equals(action.getRunId())) {
            return false;
        }
        stopRun(run);
        return true;
    }

    /**
     * Cancels the one queue item whose actions hold a {@link
     * VarroaIntentAction} carrying {@code runId}. A second queued item of the
     * same job with a different run identifier is left alone.
     */
    private static boolean cancelQueuedItem(String runId) {
        for (Queue.Item item : Queue.getInstance().getItems()) {
            VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
            if (action != null && runId.equals(action.getRunId())) {
                Queue.getInstance().cancel(item);
                return true;
            }
        }
        return false;
    }

    /**
     * Calls {@code doStop()} on the run by reflection. {@code Run} does not
     * declare this method: {@code AbstractBuild} and {@code WorkflowRun} each
     * implement it independently, with different declared exceptions.
     */
    private static void stopRun(Run<?, ?> run) throws Exception {
        Method m = run.getClass().getMethod("doStop");
        m.invoke(run);
    }

    // ---- Input-approval routes ----

    /**
     * Handles {@code POST /varroa-run/proceed-input}. Reads {@code runId},
     * {@code jobPath}, {@code buildNumber}, {@code inputId}, {@code
     * parameters}, and {@code actingUser}, resolves the build by its run
     * identifier and the execution by its input identifier, and answers that
     * input (D10, D11).
     *
     * <p>The route reads the acting user from the body, because the request
     * authenticates the mite and never the person. Every Jenkins call after
     * the resolution runs inside that user's own authentication, so the input
     * step's {@code submitter} check reads the acting user's name and its one
     * authority per group.
     *
     * <p>Answers {@code 405} for a method other than {@code POST},
     * {@code 403} without the mite authority and for an input the acting user
     * may not answer, {@code 400} for a body that does not decode and for a
     * body that names no run, no input, or no acting user, {@code 404} for no
     * build that carries the run identifier and for an input identifier the
     * run does not hold, {@code 409} for a settled input, and {@code 202} on
     * success. The {@code 202} body names every supplied parameter the route
     * dropped.
     */
    @WebMethod(name = "proceed-input")
    public void doProceedInput(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String jobPath = body.optString("jobPath", "");
            long buildNumber = body.optLong("buildNumber", 0L);
            String inputId = body.optString("inputId", "");
            VarroaActingUser actingUser = VarroaActingUser.fromJson(body);
            if (runId.isEmpty() || inputId.isEmpty() || actingUser == null) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no run, no input, or no acting user");
                return;
            }

            Run<?, ?> run = resolveRunByRunId(jobPath, buildNumber, runId);
            if (run == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no build carries run id " + runId);
                return;
            }
            Object execution = VarroaRunInputs.executionFor(VarroaRunInputs.inputActionOf(run), inputId);
            if (execution == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no input carries id " + inputId);
                return;
            }

            JSONObject supplied = body.optJSONObject("parameters");
            List<String> dropped = new ArrayList<>();
            try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                // The explicit settled read separates a 409 from the 403 the
                // submission check answers for the same input, so it runs first.
                if (VarroaRunInputs.isSettled(execution)) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the input is already settled");
                    return;
                }
                try {
                    VarroaRunInputs.preSubmissionCheck(execution);
                } catch (Failure refused) {
                    PlainTextRefusal.refuse(rsp, 403, "Forbidden: the acting user may not answer the input");
                    return;
                }
                VarroaRunInputs.proceed(execution, inputParameterValues(req, execution, supplied, dropped));
            }

            VarroaRunRecorder.recheckInputs(run);

            JSONObject out = new JSONObject();
            out.put("runId", runId);
            JSONArray droppedParameters = new JSONArray();
            droppedParameters.addAll(dropped);
            out.put("droppedParameters", droppedParameters);

            rsp.setContentType("application/json");
            rsp.setStatus(202);
            rsp.getWriter().write(out.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doProceedInput caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/abort-input}. Reads {@code runId},
     * {@code jobPath}, {@code buildNumber}, {@code inputId}, and {@code
     * actingUser}, resolves the build and the execution the way the proceed
     * route resolves them, and rejects that input.
     *
     * <p>The route runs no permission check of its own. The input step's own
     * abort path checks the build-cancel permission and the submitter
     * identity, and a copy of that rule here would drift from it.
     *
     * <p>Answers the same codes the proceed route answers, without a
     * {@code parameters} field and without a response body.
     */
    @WebMethod(name = "abort-input")
    public void doAbortInput(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String jobPath = body.optString("jobPath", "");
            long buildNumber = body.optLong("buildNumber", 0L);
            String inputId = body.optString("inputId", "");
            VarroaActingUser actingUser = VarroaActingUser.fromJson(body);
            if (runId.isEmpty() || inputId.isEmpty() || actingUser == null) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no run, no input, or no acting user");
                return;
            }

            Run<?, ?> run = resolveRunByRunId(jobPath, buildNumber, runId);
            if (run == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no build carries run id " + runId);
                return;
            }
            Object execution = VarroaRunInputs.executionFor(VarroaRunInputs.inputActionOf(run), inputId);
            if (execution == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no input carries id " + inputId);
                return;
            }

            try (ACLContext ignored = ACL.as2(actingUser.authentication())) {
                if (VarroaRunInputs.isSettled(execution)) {
                    PlainTextRefusal.refuse(rsp, 409, "Conflict: the input is already settled");
                    return;
                }
                try {
                    VarroaRunInputs.abort(execution);
                } catch (Failure refused) {
                    PlainTextRefusal.refuse(rsp, 403, "Forbidden: the acting user may not reject the input");
                    return;
                }
            }

            VarroaRunRecorder.recheckInputs(run);
            rsp.setStatus(202);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doAbortInput caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Resolves the run one of the two input routes acts on, the way the cancel
     * route resolves its target. It reads the numbered build of the named job,
     * and the build answers only when its own {@link VarroaIntentAction}
     * carries the caller's run identifier.
     *
     * <p>A job path that is empty, a build number at or below zero, a job or a
     * build that does not resolve, and an identity that does not match each
     * answer null, and the route then answers {@code 404}. An input exists only
     * on a running build, never on a queue item.
     */
    private static Run<?, ?> resolveRunByRunId(String jobPath, long buildNumber, String runId) {
        if (jobPath.isEmpty() || buildNumber <= 0) {
            return null;
        }
        Job<?, ?> job = Jenkins.get().getItemByFullName(jobPath, Job.class);
        if (job == null) {
            return null;
        }
        Run<?, ?> run = job.getBuildByNumber((int) buildNumber);
        if (run == null) {
            return null;
        }
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        return action != null && runId.equals(action.getRunId()) ? run : null;
    }

    /**
     * Builds the values the input step proceeds with, from the step's own
     * parameter definitions and never the job's.
     *
     * <p>A supplied name the step declares is bound through that name's own
     * definition, and a name the request omits takes the declared default. The
     * map holds each definition's resolved value and never its
     * {@link ParameterValue} wrapper, because that is what the step's proceed
     * path expects.
     *
     * <p>A supplied name the step does not declare is dropped and named in
     * {@code dropped}. A supplied value whose definition takes a file is
     * dropped and named too, because a file value cannot ride a map of strings.
     *
     * <p>The step's submitter parameter is added last, valued with the current
     * authentication's name. That read runs inside the acting scope, so it
     * answers the acting user's own name.
     */
    private static Map<String, Object> inputParameterValues(StaplerRequest2 req, Object execution,
            JSONObject supplied, List<String> dropped) {
        Map<String, Object> values = new LinkedHashMap<>();
        List<String> declared = new ArrayList<>();
        for (ParameterDefinition definition : VarroaRunInputs.parameterDefinitions(execution)) {
            String name = definition.getName();
            declared.add(name);
            if (definition instanceof FileParameterDefinition) {
                // A file parameter carries a binary value the step copies from
                // its own upload. A supplied value for such a name is dropped.
                if (supplied != null && supplied.has(name)) {
                    dropped.add(name);
                }
                continue;
            }
            ParameterValue value;
            if (supplied != null && supplied.has(name)) {
                JSONObject jo = new JSONObject();
                jo.put("name", name);
                jo.put("value", supplied.getString(name));
                value = definition.createValue(req, jo);
            } else {
                value = definition.getDefaultParameterValue();
            }
            if (value != null) {
                values.put(name, value.getValue());
            }
        }
        if (supplied != null) {
            for (Object key : supplied.keySet()) {
                String name = String.valueOf(key);
                if (!declared.contains(name)) {
                    dropped.add(name);
                }
            }
        }
        String submitterParameter = VarroaRunInputs.submitterParameter(execution);
        if (!submitterParameter.isEmpty()) {
            values.put(submitterParameter, Jenkins.getAuthentication2().getName());
        }
        return values;
    }

    /**
     * Handles {@code POST /varroa-run/adopt-node}. Reads {@code name} and
     * adds the node {@code nodes/<name>/config.xml} describes, so a build
     * that resumed on this process finds the agent it was running on.
     * Jenkins reads {@code nodes/} at boot only, and nothing else loads a
     * file written after that.
     *
     * <p>Answers {@code 405} for a method other than {@code POST},
     * {@code 403} without the mite authority, {@code 400} for a body that
     * does not name a usable node, {@code 409} for a node this process
     * already holds, {@code 404} for a missing file, and {@code 202} on
     * success.
     */
    @WebMethod(name = "adopt-node")
    public void doAdoptNode(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String name = body.optString("name", "");
            if (!isAdoptableNodeName(name)) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no usable node");
                return;
            }

            Jenkins jenkins = Jenkins.get();
            if (jenkins.getNode(name) != null) {
                PlainTextRefusal.refuse(rsp, 409, "Conflict: this process already holds the node " + name);
                return;
            }

            File config = new File(new File(jenkins.getRootDir(), "nodes"), name + "/config.xml");
            if (!config.isFile()) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no configuration file for the node " + name);
                return;
            }

            Object loaded;
            try (InputStream in = Files.newInputStream(config.toPath())) {
                loaded = Jenkins.XSTREAM.fromXML(in);
            } catch (IOException | RuntimeException e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the configuration file does not load");
                return;
            }
            if (!(loaded instanceof Node)) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the configuration file is not a node");
                return;
            }

            jenkins.addNode((Node) loaded);
            rsp.setStatus(202);
        } catch (IOException | RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doAdoptNode caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (IOException ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code POST /varroa-run/load-build}. Reads {@code jobPath}
     * and {@code buildNumber}, loads that build, and answers whether it
     * is running.
     *
     * <p>Jenkins loads a job's builds one at a time, on demand. A build
     * directory written after the job was loaded is therefore invisible
     * until something asks for that build, and a reload of the job
     * config does not ask. The resume machinery runs inside that load:
     * the run registers with {@link FlowExecutionList} and its execution
     * resumes. Without this call a restored build waits until an
     * unrelated read touches it, which on a live cell was several
     * minutes, long past the resume deadline.
     *
     * <p>A body carrying {@code "probe": true} loads nothing. It reads
     * the flow execution list and the job's loaded builds, and reports
     * whether that build already runs here and whether it already ended
     * here. The mite asks this before it writes any file,
     * because the command transport redelivers a command whose result
     * was lost, and rewriting a running build's files would corrupt it.
     *
     * <p>Answers {@code 405} for a method other than {@code POST},
     * {@code 403} without the mite authority, {@code 400} for a body
     * that names no job or no build, {@code 404} when the job or the
     * build does not load, and {@code 200} with
     * {@code {"building": ..., "registered": ...}} on success. A probe
     * answers {@code 404} for an unknown job alone: a build that is not
     * on the list is reported as not running, never as missing.
     */
    @WebMethod(name = "load-build")
    public void doLoadBuild(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: body does not decode");
                return;
            }

            String jobPath = body.optString("jobPath", "");
            long buildNumber = body.optLong("buildNumber", 0L);
            if (jobPath.isEmpty() || buildNumber <= 0) {
                PlainTextRefusal.refuse(rsp, 400, "Bad Request: the body names no job and build");
                return;
            }

            Job<?, ?> job = Jenkins.get().getItemByFullName(jobPath, Job.class);
            if (job == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no job " + jobPath);
                return;
            }

            if (body.optBoolean("probe", false)) {
                probeAnswer(job, (int) buildNumber, rsp);
                return;
            }

            // This read is the load. It runs the run's own onLoad, which
            // resumes a pipeline that was still running when its file
            // was written.
            Run<?, ?> run = job.getBuildByNumber((int) buildNumber);
            if (run == null) {
                PlainTextRefusal.refuse(rsp, 404, "Not Found: no build " + buildNumber + " of " + jobPath);
                return;
            }

            boolean building = run.isBuilding();
            writeLoadAnswer(rsp, building, building && registerExecution(run));
        } catch (IOException | RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doLoadBuild caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (IOException ignored) {
                // Best-effort error response.
            }
        }
    }

    /** Writes the load route's own answer. */
    private static void writeLoadAnswer(StaplerResponse2 rsp, boolean building, boolean registered)
            throws IOException {
        writeAnswer(rsp, building, registered, false);
    }

    /** Writes one answer of the load route, probe or load. */
    private static void writeAnswer(StaplerResponse2 rsp, boolean building, boolean registered,
            boolean finished) throws IOException {
        JSONObject answer = new JSONObject();
        answer.put("building", building);
        answer.put("registered", registered);
        answer.put("finished", finished);
        rsp.setStatus(200);
        rsp.setContentType("application/json");
        rsp.getWriter().write(answer.toString());
    }

    /**
     * Answers the probe: whether the named build runs here, and whether
     * it already ended here.
     *
     * <p>Both readings come from what this process already holds. The
     * flow execution list names the running pipelines. A build this
     * process loaded earlier is read from the job's loaded set, which
     * loads nothing: a resume that already completed leaves a loaded
     * build that no longer runs, and the caller needs that reading to
     * know its command is a repeat rather than a fresh rescue.
     */
    private static void probeAnswer(Job<?, ?> job, int number, StaplerResponse2 rsp) throws IOException {
        Run<?, ?> loaded = loadedBuild(job, number);
        boolean running = runsOnTheList(job, number) || (loaded != null && loaded.isBuilding());
        boolean finished = !running && loaded != null && !loaded.isBuilding();
        writeAnswer(rsp, running, running, finished);
    }

    /**
     * Answers the numbered build of {@code job} when this process
     * already holds it, and null otherwise. It never loads a build:
     * Jenkins loads builds on demand, and a probe that loaded one would
     * resume it before the command wrote its files and adopted its
     * nodes.
     */
    private static Run<?, ?> loadedBuild(Job<?, ?> job, int number) {
        if (!(job instanceof LazyBuildMixIn.LazyLoadingJob)) {
            return null;
        }
        LazyBuildMixIn<?, ?> mixIn = ((LazyBuildMixIn.LazyLoadingJob<?, ?>) job).getLazyBuildMixIn();
        return mixIn._getRuns().getLoadedBuilds().get(number);
    }

    /**
     * Reports whether a build of {@code job} numbered {@code number}
     * already runs in this process, reading the flow execution list
     * alone.
     *
     * <p>The list holds the running pipelines this process knows, so a
     * build that is on it is loaded and running. Reading the list loads
     * nothing, which is why the probe uses it: asking the job for the
     * build would load that build, and a probe must not resume one
     * before the command has written its files and adopted its nodes.
     */
    private static boolean runsOnTheList(Job<?, ?> job, int number) {
        for (FlowExecution execution : FlowExecutionList.get()) {
            Run<?, ?> run;
            try {
                Queue.Executable executable = execution.getOwner().getExecutable();
                if (!(executable instanceof Run)) {
                    continue;
                }
                run = (Run<?, ?>) executable;
            } catch (IOException | RuntimeException e) {
                LOGGER.log(Level.FINE, "VarroaRunTriggerAction: an execution owner did not resolve", e);
                continue;
            }
            if (run.getNumber() == number
                    && run.getParent().getFullName().equals(job.getFullName())
                    && run.isBuilding()) {
                return true;
            }
        }
        return false;
    }

    /**
     * Reports whether {@code run} is registered with {@link
     * FlowExecutionList}, registering it when it is not. A pipeline that
     * is not on that list runs unsupervised: nothing saves its program
     * and nothing resumes it after the next restart.
     *
     * <p>The run's own load registers it, so this is a confirmation.
     * Registering a run twice logs a warning, so the list is read first.
     * A build that is not a pipeline needs no list and answers false.
     */
    private static boolean registerExecution(Run<?, ?> run) {
        if (!(run instanceof WorkflowRun)) {
            return false;
        }
        FlowExecutionOwner owner = ((WorkflowRun) run).asFlowExecutionOwner();
        FlowExecutionList list = FlowExecutionList.get();
        for (FlowExecution execution : list) {
            try {
                if (owner.equals(execution.getOwner())) {
                    return true;
                }
            } catch (RuntimeException e) {
                LOGGER.log(Level.FINE, "VarroaRunTriggerAction: an execution owner did not resolve", e);
            }
        }
        list.register(owner);
        return true;
    }

    /**
     * Reports whether {@code name} may be joined onto the {@code nodes/}
     * directory. A name holding a path separator, or starting with a
     * dot, can escape that directory, so the route refuses it before it
     * builds any path.
     */
    static boolean isAdoptableNodeName(String name) {
        if (name == null || name.isEmpty()) {
            return false;
        }
        if (name.startsWith(".")) {
            return false;
        }
        return name.indexOf('/') < 0 && name.indexOf('\\') < 0;
    }

    // ---- Shared ----

    private JSONObject readBody(StaplerRequest2 req) throws Exception {
        try (BufferedReader reader = req.getReader()) {
            String text = reader.lines().collect(Collectors.joining("\n"));
            if (text.isEmpty()) {
                return new JSONObject();
            }
            return JSONObject.fromObject(text);
        }
    }

    private boolean isMitePrincipal() {
        for (GrantedAuthority auth : Jenkins.getAuthentication2().getAuthorities()) {
            if ("ROLE:varroa:system-mite".equals(auth.getAuthority())) {
                return true;
            }
        }
        return false;
    }
}
