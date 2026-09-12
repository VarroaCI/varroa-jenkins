package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.Item;
import hudson.model.Job;
import hudson.model.ParameterDefinition;
import hudson.model.ParameterValue;
import hudson.model.ParametersAction;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.Queue;
import hudson.model.RootAction;
import hudson.model.Run;

import java.io.BufferedReader;
import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Collectors;

import jenkins.model.Jenkins;
import jenkins.model.ParameterizedJobMixIn;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;

import org.springframework.security.core.GrantedAuthority;

/**
 * Protected {@link RootAction} at {@code /varroa-run}, the mite's local call for a
 * manual build trigger and a manual build cancel (D3, D4, run-trigger-endpoint).
 *
 * <p>Requires the authority {@code ROLE:varroa:system-mite}, the same check {@link
 * ActivityEndpoint} performs. Only the mite calls these routes, over the Jenkins
 * bearer token the operator signs.
 *
 * <p>{@code POST /varroa-run/trigger} stamps a caller-supplied run identifier and
 * intent key onto the queue item before it schedules, so the claim gate observes the
 * caller's key instead of minting its own. {@code POST /varroa-run/cancel} resolves
 * its target by run identifier, never by job path alone, because one job can hold
 * several queued items and several running builds.
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
     * intentKey}, {@code jobPath}, {@code parameters}, and {@code actor}, narrows
     * the named item to a parameterized job, builds a {@link ParametersAction} from
     * its declared definitions, stamps a {@link VarroaIntentAction} carrying the
     * caller's identity, and schedules through {@link ParameterizedJobMixIn}.
     */
    public void doTrigger(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                rsp.sendError(405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                rsp.sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                rsp.sendError(400, "Bad Request: body does not decode");
                return;
            }

            String runId = body.optString("runId", "");
            String intentKey = body.optString("intentKey", "");
            String jobPath = body.optString("jobPath", "");
            String actor = body.optString("actor", "");
            JSONObject parameters = body.optJSONObject("parameters");

            // Look the item up untyped first: Jenkins.getItemByFullName(path, Job.class)
            // answers null both for a missing item and for one that exists but is not
            // a Job (e.g. a folder), and those two cases carry different response codes.
            Item rawItem = Jenkins.get().getItemByFullName(jobPath);
            if (rawItem == null) {
                rsp.sendError(404, "Not Found: no item named " + jobPath);
                return;
            }
            if (!(rawItem instanceof Job) || !(rawItem instanceof ParameterizedJobMixIn.ParameterizedJob)) {
                rsp.sendError(409, "Conflict: " + jobPath + " is a " + rawItem.getClass().getName()
                        + ", not a parameterized job");
                return;
            }
            Job<?, ?> job = (Job<?, ?>) rawItem;

            List<String> droppedParameters = new ArrayList<>();
            List<Action> actions = new ArrayList<>();
            actions.add(buildParametersAction(job, req, parameters, droppedParameters));
            actions.add(stampedIntentAction(runId, intentKey, actor));
            if (!actor.isEmpty()) {
                actions.add(new CauseAction(new Cause.UserIdCause(actor)));
            }

            Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(job, 0, actions.toArray(new Action[0]));
            if (item == null) {
                rsp.sendError(409, "Conflict: the queue refused to schedule " + jobPath);
                return;
            }

            JSONObject out = new JSONObject();
            out.put("runId", runId);
            JSONArray dropped = new JSONArray();
            dropped.addAll(droppedParameters);
            out.put("droppedParameters", dropped);

            rsp.setContentType("application/json");
            rsp.setStatus(202);
            rsp.getWriter().write(out.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doTrigger caught exception", e);
            try {
                rsp.sendError(500, "Internal server error");
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
                rsp.sendError(405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                rsp.sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            JSONObject body;
            try {
                body = readBody(req);
            } catch (Exception e) {
                rsp.sendError(400, "Bad Request: body does not decode");
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
                rsp.sendError(404, "Not Found: no build or queue item carries run id " + runId);
                return;
            }
            rsp.setStatus(202);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunTriggerAction.doCancel caught exception", e);
            try {
                rsp.sendError(500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    // ---- Trigger helpers ----

    /**
     * Stamps a fresh {@link VarroaIntentAction} carrying the caller's run
     * identifier, intent key, and actor. The action's origin is {@code
     * VARROA_INTENT}: the claim gate reads a non-empty intent key straight
     * off the action rather than deriving one (see {@link IntentKey#derive}).
     * The actor is the value the caller supplied, unchanged, even when empty.
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
