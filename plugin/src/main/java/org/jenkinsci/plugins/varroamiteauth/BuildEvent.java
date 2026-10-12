package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Immutable value object mirroring the {@code BuildEvent} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly, so
 * the mite loopback accepts the body with no translation.
 *
 * <p>The plugin never sets field 42, {@code delivery_id}, through this
 * builder for a claim; {@link VarroaClaimQueue} carries it separately.
 * Field 36, {@code artifacts}, and field 37, {@code log_summary}, are set
 * only on the {@code COMPLETED} event. Fields 38 to 41 — {@code
 * template_name}, {@code template_version}, {@code marker_path}, and
 * {@code marker_revision} — are empty for a run with no template, and a
 * {@code HEARTBEAT} event never sets them.
 *
 * <p>Fields 43 to 45 — {@code libraries}, {@code definition_path}, and
 * {@code pod_templates} — are empty for a run whose source the plugin did
 * not read. {@link #toJson()} then writes no key for them.
 *
 * <p>Field 46, {@code summary}, is empty for a run whose pipeline never
 * called {@code varroa.summary}. Only a {@code HEARTBEAT} and a {@code
 * COMPLETED} event carry it. {@link #toJson()} writes no key for it when
 * it is empty.
 *
 * <p>Field 47, {@code inputs}, holds the run's pending {@code input}
 * steps. Only a {@code run.input} event and a {@code HEARTBEAT} event
 * carry it, and only a {@code HEARTBEAT} event carries an empty list.
 * {@link #toJson()} writes no key for an empty list, and the gateway
 * writes the key for every set.
 *
 * <p>Field 48, {@code test_report}, and field 49, {@code test_cases}, are
 * set only on the {@code COMPLETED} event. {@link #toJson()} writes no
 * {@code test_report} key for a null report and no {@code test_cases} key
 * for an empty list.
 *
 * <p>Field 50, {@code replay_of}, names the run whose script a replayed
 * build copied. Only the {@code STARTED} event carries it. {@link
 * #toJson()} writes no key for it when it is empty.
 *
 * Field 51, {@code granted_build_number}, is the grant the run applied.
 * Only the {@code STARTED} event carries it. {@link #toJson()} writes no
 * key for zero.
 *
 * <p>Field 52, {@code restart_of}, names the run a restarted build reran
 * from, and field 53, {@code restart_stage}, names the stage the restart
 * began at. Only the {@code STARTED} event carries them.
 *
 * <p>Field 54, {@code restart_stages}, lists the stages a completed
 * declarative run can restart from. Field 55, {@code restart_refusal},
 * names why a completed run cannot restart. Only the {@code COMPLETED}
 * event carries them. {@link #toJson()} writes no key for an empty value.
 * <p>Field 57, {@code rejected_signature}, is the script signature the
 * sandbox refused in a failed build. Only the {@code COMPLETED} event
 * carries it. {@link #toJson()} writes no key for it when it is empty.
 *
 * <p>Field 56, {@code change_request}, names the change request a
 * multibranch pull-request run builds. Only the {@code STARTED} event
 * carries it. {@link #toJson()} writes no key for a null value or an
 * empty id.
 */
public final class BuildEvent implements PostableEvent {

    /** Phase values, a closed set: no proto enum backs {@code phase}. */
    public static final String PHASE_STARTED = "STARTED";
    public static final String PHASE_STAGE_STARTED = "STAGE_STARTED";
    public static final String PHASE_STAGE = "STAGE";
    public static final String PHASE_HEARTBEAT = "HEARTBEAT";
    public static final String PHASE_COMPLETED = "COMPLETED";
    /** Phase value of the two run-source emissions. The registry allocates the lower-case literal. */
    public static final String PHASE_RUN_SOURCE = "run.source";
    /** Phase value of a pending-input report. The registry allocates the lower-case literal. */
    public static final String PHASE_RUN_INPUT = "run.input";

    private final String phase;
    private final long seq;
    private final long emittedAtMillis;
    private final String runId;
    private final String intentKey;
    private final String jobPath;
    private final String jobDisplayName;
    private final long buildNumber;
    private final String buildDisplayName;
    private final String url;
    private final String result;
    private final String actor;
    private final List<String> causeTypes;
    private final String causeDescription;
    private final String upstreamJobPath;
    private final long upstreamBuildNumber;
    private final long queuedAtMillis;
    private final long startedAtMillis;
    private final long completedAtMillis;
    private final long durationMillis;
    private final long queueWaitMillis;
    private final long estimatedDurationMillis;
    private final Map<String, String> parameters;
    private final List<String> maskedParameterNames;
    private final String scmUrl;
    private final String scmHead;
    private final String scmRevision;
    private final List<BuildChange> changes;
    private final int changeCount;
    private final int testTotal;
    private final int testPassed;
    private final int testFailed;
    private final int testSkipped;
    private final List<BuildStage> stages;
    private final String branch;
    private final String deliveryId;
    private final List<BuildArtifact> artifacts;
    private final LogSummary logSummary;
    private final String templateName;
    private final String templateVersion;
    private final String markerPath;
    private final String markerRevision;
    private final List<BuildLibrary> libraries;
    private final String definitionPath;
    private final List<BuildPodTemplate> podTemplates;
    private final String summary;
    private final List<BuildInput> inputs;
    private final BuildTestReport testReport;
    private final List<BuildTestCase> testCases;
    private final String replayOf;
    private final long grantedBuildNumber;
    private final String restartOf;
    private final String restartStage;
    private final List<String> restartStages;
    private final String restartRefusal;
    private final ChangeRequestInfo changeRequest;
    private final String rejectedSignature;

    private BuildEvent(Builder b) {
        this.phase = orEmpty(b.phase);
        this.seq = b.seq;
        this.emittedAtMillis = b.emittedAtMillis;
        this.runId = orEmpty(b.runId);
        this.intentKey = orEmpty(b.intentKey);
        this.jobPath = orEmpty(b.jobPath);
        this.jobDisplayName = orEmpty(b.jobDisplayName);
        this.buildNumber = b.buildNumber;
        this.buildDisplayName = orEmpty(b.buildDisplayName);
        this.url = orEmpty(b.url);
        this.result = orEmpty(b.result);
        this.actor = orEmpty(b.actor);
        this.causeTypes = List.copyOf(b.causeTypes);
        this.causeDescription = orEmpty(b.causeDescription);
        this.upstreamJobPath = orEmpty(b.upstreamJobPath);
        this.upstreamBuildNumber = b.upstreamBuildNumber;
        this.queuedAtMillis = b.queuedAtMillis;
        this.startedAtMillis = b.startedAtMillis;
        this.completedAtMillis = b.completedAtMillis;
        this.durationMillis = b.durationMillis;
        this.queueWaitMillis = b.queueWaitMillis;
        this.estimatedDurationMillis = b.estimatedDurationMillis;
        this.parameters = new LinkedHashMap<>(b.parameters);
        this.maskedParameterNames = List.copyOf(b.maskedParameterNames);
        this.scmUrl = orEmpty(b.scmUrl);
        this.scmHead = orEmpty(b.scmHead);
        this.scmRevision = orEmpty(b.scmRevision);
        this.changes = List.copyOf(b.changes);
        this.changeCount = b.changeCount;
        this.testTotal = b.testTotal;
        this.testPassed = b.testPassed;
        this.testFailed = b.testFailed;
        this.testSkipped = b.testSkipped;
        this.stages = List.copyOf(b.stages);
        this.branch = orEmpty(b.branch);
        this.deliveryId = orEmpty(b.deliveryId);
        this.artifacts = List.copyOf(b.artifacts);
        this.logSummary = b.logSummary;
        this.templateName = orEmpty(b.templateName);
        this.templateVersion = orEmpty(b.templateVersion);
        this.markerPath = orEmpty(b.markerPath);
        this.markerRevision = orEmpty(b.markerRevision);
        this.libraries = List.copyOf(b.libraries);
        this.definitionPath = orEmpty(b.definitionPath);
        this.podTemplates = List.copyOf(b.podTemplates);
        this.summary = orEmpty(b.summary);
        this.inputs = List.copyOf(b.inputs);
        this.testReport = b.testReport;
        this.testCases = List.copyOf(b.testCases);
        this.replayOf = orEmpty(b.replayOf);
        this.grantedBuildNumber = b.grantedBuildNumber;
        this.restartOf = orEmpty(b.restartOf);
        this.restartStage = orEmpty(b.restartStage);
        this.restartStages = List.copyOf(b.restartStages);
        this.restartRefusal = orEmpty(b.restartRefusal);
        this.changeRequest = b.changeRequest;
        this.rejectedSignature = orEmpty(b.rejectedSignature);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    /** Returns {@code true} when the phase is {@code HEARTBEAT}. */
    @Override
    public boolean isHeartbeat() {
        return PHASE_HEARTBEAT.equals(phase);
    }

    @Override
    public boolean isCompletedPhase() {
        return PHASE_COMPLETED.equals(phase);
    }

    public String getPhase() {
        return phase;
    }

    public long getSeq() {
        return seq;
    }

    public String getRunId() {
        return runId;
    }

    public String getJobPath() {
        return jobPath;
    }

    public long getBuildNumber() {
        return buildNumber;
    }

    public String getIntentKey() {
        return intentKey;
    }

    public String getResult() {
        return result;
    }

    public long getEmittedAtMillis() {
        return emittedAtMillis;
    }

    public String getJobDisplayName() {
        return jobDisplayName;
    }

    public String getBuildDisplayName() {
        return buildDisplayName;
    }

    public String getUrl() {
        return url;
    }

    public String getActor() {
        return actor;
    }

    public List<String> getCauseTypes() {
        return causeTypes;
    }

    public String getCauseDescription() {
        return causeDescription;
    }

    public String getUpstreamJobPath() {
        return upstreamJobPath;
    }

    public long getUpstreamBuildNumber() {
        return upstreamBuildNumber;
    }

    public long getQueuedAtMillis() {
        return queuedAtMillis;
    }

    public long getStartedAtMillis() {
        return startedAtMillis;
    }

    public long getCompletedAtMillis() {
        return completedAtMillis;
    }

    public long getDurationMillis() {
        return durationMillis;
    }

    public long getQueueWaitMillis() {
        return queueWaitMillis;
    }

    public long getEstimatedDurationMillis() {
        return estimatedDurationMillis;
    }

    public Map<String, String> getParameters() {
        return parameters;
    }

    public List<String> getMaskedParameterNames() {
        return maskedParameterNames;
    }

    public String getScmUrl() {
        return scmUrl;
    }

    public String getScmHead() {
        return scmHead;
    }

    public String getScmRevision() {
        return scmRevision;
    }

    public List<BuildChange> getChanges() {
        return changes;
    }

    public int getChangeCount() {
        return changeCount;
    }

    public int getTestTotal() {
        return testTotal;
    }

    public int getTestPassed() {
        return testPassed;
    }

    public int getTestFailed() {
        return testFailed;
    }

    public int getTestSkipped() {
        return testSkipped;
    }

    public List<BuildStage> getStages() {
        return stages;
    }

    public String getBranch() {
        return branch;
    }

    /** Field 56: the change request a multibranch run builds, or {@code null} for a branch run. */
    public ChangeRequestInfo getChangeRequest() {
        return changeRequest;
    }

    public String getDeliveryId() {
        return deliveryId;
    }

    /** Field 36: one entry per artifact the run archived. Empty until {@code COMPLETED}. */
    public List<BuildArtifact> getArtifacts() {
        return artifacts;
    }

    /** Field 37: the run's log summary, or {@code null} before {@code COMPLETED}. */
    public LogSummary getLogSummary() {
        return logSummary;
    }

    /** Field 38: the resolved template's name. Empty for a run with no template. */
    public String getTemplateName() {
        return templateName;
    }

    /** Field 39: the resolved template's version. Empty for a run with no template. */
    public String getTemplateVersion() {
        return templateVersion;
    }

    /** Field 40: the marker file's path. Empty for a run with no template. */
    public String getMarkerPath() {
        return markerPath;
    }

    /** Field 41: the marker's trusted revision, never the branch tip. Empty for a run with no template. */
    public String getMarkerRevision() {
        return markerRevision;
    }

    /** Field 43: one entry per global library the definition loaded. Empty at a run with no library. */
    public List<BuildLibrary> getLibraries() {
        return libraries;
    }

    /** Field 44: the script path the run's definition declares. Empty for no path. */
    public String getDefinitionPath() {
        return definitionPath;
    }

    /** Field 45: one entry per pod template the run allocated. Empty before the probe resolves. */
    public List<BuildPodTemplate> getPodTemplates() {
        return podTemplates;
    }

    /** Field 46: the run's summary text. Empty for a run whose pipeline set none. */
    public String getSummary() {
        return summary;
    }

    /** Field 47: one entry per pending {@code input} step. Empty for a run that waits on none. */
    public List<BuildInput> getInputs() {
        return inputs;
    }

    /** Field 48: the pointer to the archived test report. Null for a run that archived none. */
    public BuildTestReport getTestReport() {
        return testReport;
    }

    /** Field 49: the failed cases, then the skipped cases. Empty for a run that reported none. */
    public List<BuildTestCase> getTestCases() {
        return testCases;
    }

    /** Field 50: the run a replayed build copied its script from. Empty for a build with no source. */
    public String getReplayOf() {
        return replayOf;
    }

    /** Field 57: the script signature the sandbox refused. Empty when none. */
    public String getRejectedSignature() {
        return rejectedSignature;
    }

    /** Field 51: the grant the run applied. Zero for a run with no grant. */
    public long getGrantedBuildNumber() {
        return grantedBuildNumber;
    }

    /** Field 52: the run a restarted build reran from. Empty for a build that is not a restart. */
    public String getRestartOf() {
        return restartOf;
    }

    /** Field 53: the stage a restarted build began at. Empty for a build that is not a restart. */
    public String getRestartStage() {
        return restartStage;
    }

    /** Field 54: the stages a completed declarative run can restart from. */
    public List<String> getRestartStages() {
        return restartStages;
    }

    /** Field 55: why a completed run cannot restart. Empty when it can. */
    public String getRestartRefusal() {
        return restartRefusal;
    }

    /**
     * Serializes the event to a {@link JSONObject} using proto JSON field
     * names, ready to post to {@code /v1/events}.
     */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("phase", phase);
        o.put("seq", seq);
        o.put("emitted_at_millis", emittedAtMillis);
        o.put("run_id", runId);
        o.put("intent_key", intentKey);
        o.put("job_path", jobPath);
        o.put("job_display_name", jobDisplayName);
        o.put("build_number", buildNumber);
        o.put("build_display_name", buildDisplayName);
        o.put("url", url);
        o.put("result", result);
        o.put("actor", actor);
        o.put("cause_types", stringArray(causeTypes));
        o.put("cause_description", causeDescription);
        o.put("upstream_job_path", upstreamJobPath);
        o.put("upstream_build_number", upstreamBuildNumber);
        o.put("queued_at_millis", queuedAtMillis);
        o.put("started_at_millis", startedAtMillis);
        o.put("completed_at_millis", completedAtMillis);
        o.put("duration_millis", durationMillis);
        o.put("queue_wait_millis", queueWaitMillis);
        o.put("estimated_duration_millis", estimatedDurationMillis);
        JSONObject params = new JSONObject();
        for (Map.Entry<String, String> e : parameters.entrySet()) {
            params.put(e.getKey(), e.getValue());
        }
        o.put("parameters", params);
        o.put("masked_parameter_names", stringArray(maskedParameterNames));
        o.put("scm_url", scmUrl);
        o.put("scm_head", scmHead);
        o.put("scm_revision", scmRevision);
        JSONArray changesArr = new JSONArray();
        for (BuildChange c : changes) {
            changesArr.add(c.toJson());
        }
        o.put("changes", changesArr);
        o.put("change_count", changeCount);
        o.put("test_total", testTotal);
        o.put("test_passed", testPassed);
        o.put("test_failed", testFailed);
        o.put("test_skipped", testSkipped);
        JSONArray stagesArr = new JSONArray();
        for (BuildStage s : stages) {
            stagesArr.add(s.toJson());
        }
        o.put("stages", stagesArr);
        o.put("branch", branch);
        o.put("delivery_id", deliveryId);
        JSONArray artifactsArr = new JSONArray();
        for (BuildArtifact a : artifacts) {
            artifactsArr.add(a.toJson());
        }
        o.put("artifacts", artifactsArr);
        if (logSummary != null) {
            o.put("log_summary", logSummary.toJson());
        }
        o.put("template_name", templateName);
        o.put("template_version", templateVersion);
        o.put("marker_path", markerPath);
        o.put("marker_revision", markerRevision);
        if (!libraries.isEmpty()) {
            JSONArray librariesArr = new JSONArray();
            for (BuildLibrary lib : libraries) {
                librariesArr.add(lib.toJson());
            }
            o.put("libraries", librariesArr);
        }
        if (!definitionPath.isEmpty()) {
            o.put("definition_path", definitionPath);
        }
        if (!podTemplates.isEmpty()) {
            JSONArray podTemplatesArr = new JSONArray();
            for (BuildPodTemplate t : podTemplates) {
                podTemplatesArr.add(t.toJson());
            }
            o.put("pod_templates", podTemplatesArr);
        }
        if (!summary.isEmpty()) {
            o.put("summary", summary);
        }
        if (!inputs.isEmpty()) {
            JSONArray inputsArr = new JSONArray();
            for (BuildInput input : inputs) {
                inputsArr.add(input.toJson());
            }
            o.put("inputs", inputsArr);
        }
        if (testReport != null) {
            o.put("test_report", testReport.toJson());
        }
        if (!testCases.isEmpty()) {
            JSONArray testCasesArr = new JSONArray();
            for (BuildTestCase c : testCases) {
                testCasesArr.add(c.toJson());
            }
            o.put("test_cases", testCasesArr);
        }
        if (!replayOf.isEmpty()) {
            o.put("replay_of", replayOf);
        }
        if (grantedBuildNumber != 0) {
            o.put("granted_build_number", grantedBuildNumber);
        }
        if (!restartOf.isEmpty()) {
            o.put("restart_of", restartOf);
        }
        if (!restartStage.isEmpty()) {
            o.put("restart_stage", restartStage);
        }
        if (!restartStages.isEmpty()) {
            o.put("restart_stages", stringArray(restartStages));
        }
        if (!restartRefusal.isEmpty()) {
            o.put("restart_refusal", restartRefusal);
        }
        if (changeRequest != null && !changeRequest.getId().isEmpty()) {
            o.put("change_request", changeRequest.toJson());
        }
        if (!rejectedSignature.isEmpty()) {
            o.put("rejected_signature", rejectedSignature);
        }
        return o;
    }

    private static JSONArray stringArray(List<String> values) {
        JSONArray arr = new JSONArray();
        for (String v : values) {
            arr.add(v);
        }
        return arr;
    }

    public static Builder builder() {
        return new Builder();
    }

    /** Returns a builder pre-populated with every field of {@code this}. */
    public Builder toBuilder() {
        return new Builder()
                .phase(phase)
                .seq(seq)
                .emittedAtMillis(emittedAtMillis)
                .runId(runId)
                .intentKey(intentKey)
                .jobPath(jobPath)
                .jobDisplayName(jobDisplayName)
                .buildNumber(buildNumber)
                .buildDisplayName(buildDisplayName)
                .url(url)
                .result(result)
                .actor(actor)
                .causeTypes(causeTypes)
                .causeDescription(causeDescription)
                .upstreamJobPath(upstreamJobPath)
                .upstreamBuildNumber(upstreamBuildNumber)
                .queuedAtMillis(queuedAtMillis)
                .startedAtMillis(startedAtMillis)
                .completedAtMillis(completedAtMillis)
                .durationMillis(durationMillis)
                .queueWaitMillis(queueWaitMillis)
                .estimatedDurationMillis(estimatedDurationMillis)
                .parameters(parameters)
                .maskedParameterNames(maskedParameterNames)
                .scmUrl(scmUrl)
                .scmHead(scmHead)
                .scmRevision(scmRevision)
                .changes(changes)
                .changeCount(changeCount)
                .testTotal(testTotal)
                .testPassed(testPassed)
                .testFailed(testFailed)
                .testSkipped(testSkipped)
                .stages(stages)
                .branch(branch)
                .deliveryId(deliveryId)
                .artifacts(artifacts)
                .logSummary(logSummary)
                .templateName(templateName)
                .templateVersion(templateVersion)
                .markerPath(markerPath)
                .markerRevision(markerRevision)
                .libraries(libraries)
                .definitionPath(definitionPath)
                .podTemplates(podTemplates)
                .summary(summary)
                .inputs(inputs)
                .testReport(testReport)
                .testCases(testCases)
                .replayOf(replayOf)
                .grantedBuildNumber(grantedBuildNumber)
                .restartOf(restartOf)
                .restartStage(restartStage)
                .restartStages(restartStages)
                .restartRefusal(restartRefusal)
                .changeRequest(changeRequest)
                .rejectedSignature(rejectedSignature);
    }

    /**
     * Field 56: the change request of a multibranch run. Every value is
     * a plain string, and an empty value means the fact is unknown.
     */
    public static final class ChangeRequestInfo {
        private final String id;
        private final String title;
        private final String sourceBranch;
        private final String targetBranch;
        private final String url;

        public ChangeRequestInfo(String id, String title, String sourceBranch, String targetBranch, String url) {
            this.id = orEmpty(id);
            this.title = orEmpty(title);
            this.sourceBranch = orEmpty(sourceBranch);
            this.targetBranch = orEmpty(targetBranch);
            this.url = orEmpty(url);
        }

        public String getId() {
            return id;
        }

        public String getTitle() {
            return title;
        }

        public String getSourceBranch() {
            return sourceBranch;
        }

        public String getTargetBranch() {
            return targetBranch;
        }

        public String getUrl() {
            return url;
        }

        /** Serializes the value with the proto JSON names. Empty keys are omitted except {@code id}. */
        JSONObject toJson() {
            JSONObject o = new JSONObject();
            o.put("id", id);
            if (!title.isEmpty()) {
                o.put("title", title);
            }
            if (!sourceBranch.isEmpty()) {
                o.put("source_branch", sourceBranch);
            }
            if (!targetBranch.isEmpty()) {
                o.put("target_branch", targetBranch);
            }
            if (!url.isEmpty()) {
                o.put("url", url);
            }
            return o;
        }
    }

    public static final class Builder {
        private String phase;
        private long seq;
        private long emittedAtMillis;
        private String runId;
        private String intentKey;
        private String jobPath;
        private String jobDisplayName;
        private long buildNumber;
        private String buildDisplayName;
        private String url;
        private String result;
        private String actor;
        private List<String> causeTypes = new ArrayList<>();
        private String causeDescription;
        private String upstreamJobPath;
        private long upstreamBuildNumber;
        private long queuedAtMillis;
        private long startedAtMillis;
        private long completedAtMillis;
        private long durationMillis;
        private long queueWaitMillis;
        private long estimatedDurationMillis;
        private Map<String, String> parameters = new LinkedHashMap<>();
        private List<String> maskedParameterNames = new ArrayList<>();
        private String scmUrl;
        private String scmHead;
        private String scmRevision;
        private List<BuildChange> changes = new ArrayList<>();
        private int changeCount;
        private int testTotal;
        private int testPassed;
        private int testFailed;
        private int testSkipped;
        private List<BuildStage> stages = new ArrayList<>();
        private String branch;
        private String deliveryId;
        private List<BuildArtifact> artifacts = new ArrayList<>();
        private LogSummary logSummary;
        private String templateName;
        private String templateVersion;
        private String markerPath;
        private String markerRevision;
        private List<BuildLibrary> libraries = new ArrayList<>();
        private String definitionPath;
        private List<BuildPodTemplate> podTemplates = new ArrayList<>();
        private String summary;
        private List<BuildInput> inputs = new ArrayList<>();
        private BuildTestReport testReport;
        private List<BuildTestCase> testCases = new ArrayList<>();
        private String replayOf;
        private long grantedBuildNumber;
        private String restartOf;
        private String restartStage;
        private List<String> restartStages = new ArrayList<>();
        private String restartRefusal;
        private ChangeRequestInfo changeRequest;
        private String rejectedSignature;

        private Builder() {
        }

        public Builder phase(String v) {
            this.phase = v;
            return this;
        }

        public Builder seq(long v) {
            this.seq = v;
            return this;
        }

        public Builder emittedAtMillis(long v) {
            this.emittedAtMillis = v;
            return this;
        }

        public Builder runId(String v) {
            this.runId = v;
            return this;
        }

        public Builder intentKey(String v) {
            this.intentKey = v;
            return this;
        }

        public Builder jobPath(String v) {
            this.jobPath = v;
            return this;
        }

        public Builder jobDisplayName(String v) {
            this.jobDisplayName = v;
            return this;
        }

        public Builder buildNumber(long v) {
            this.buildNumber = v;
            return this;
        }

        public Builder buildDisplayName(String v) {
            this.buildDisplayName = v;
            return this;
        }

        public Builder url(String v) {
            this.url = v;
            return this;
        }

        public Builder result(String v) {
            this.result = v;
            return this;
        }

        public Builder actor(String v) {
            this.actor = v;
            return this;
        }

        public Builder causeTypes(List<String> v) {
            this.causeTypes = new ArrayList<>(v);
            return this;
        }

        public Builder causeDescription(String v) {
            this.causeDescription = v;
            return this;
        }

        public Builder upstreamJobPath(String v) {
            this.upstreamJobPath = v;
            return this;
        }

        public Builder upstreamBuildNumber(long v) {
            this.upstreamBuildNumber = v;
            return this;
        }

        public Builder queuedAtMillis(long v) {
            this.queuedAtMillis = v;
            return this;
        }

        public Builder startedAtMillis(long v) {
            this.startedAtMillis = v;
            return this;
        }

        public Builder completedAtMillis(long v) {
            this.completedAtMillis = v;
            return this;
        }

        public Builder durationMillis(long v) {
            this.durationMillis = v;
            return this;
        }

        public Builder queueWaitMillis(long v) {
            this.queueWaitMillis = v;
            return this;
        }

        public Builder estimatedDurationMillis(long v) {
            this.estimatedDurationMillis = v;
            return this;
        }

        public Builder parameters(Map<String, String> v) {
            this.parameters = new LinkedHashMap<>(v);
            return this;
        }

        public Builder maskedParameterNames(List<String> v) {
            this.maskedParameterNames = new ArrayList<>(v);
            return this;
        }

        public Builder scmUrl(String v) {
            this.scmUrl = v;
            return this;
        }

        public Builder scmHead(String v) {
            this.scmHead = v;
            return this;
        }

        public Builder scmRevision(String v) {
            this.scmRevision = v;
            return this;
        }

        public Builder changes(List<BuildChange> v) {
            this.changes = new ArrayList<>(v);
            return this;
        }

        public Builder changeCount(int v) {
            this.changeCount = v;
            return this;
        }

        public Builder testTotal(int v) {
            this.testTotal = v;
            return this;
        }

        public Builder testPassed(int v) {
            this.testPassed = v;
            return this;
        }

        public Builder testFailed(int v) {
            this.testFailed = v;
            return this;
        }

        public Builder testSkipped(int v) {
            this.testSkipped = v;
            return this;
        }

        public Builder stages(List<BuildStage> v) {
            this.stages = new ArrayList<>(v);
            return this;
        }

        public Builder changeRequest(ChangeRequestInfo v) {
            this.changeRequest = v;
            return this;
        }

        public Builder branch(String v) {
            this.branch = v;
            return this;
        }

        public Builder deliveryId(String v) {
            this.deliveryId = v;
            return this;
        }

        /** Field 36: one entry per artifact the run archived. */
        public Builder artifacts(List<BuildArtifact> v) {
            this.artifacts = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        /** Field 37: the run's log summary. */
        public Builder logSummary(LogSummary v) {
            this.logSummary = v;
            return this;
        }

        /** Field 38: the resolved template's name. */
        public Builder templateName(String v) {
            this.templateName = v;
            return this;
        }

        /** Field 39: the resolved template's version. */
        public Builder templateVersion(String v) {
            this.templateVersion = v;
            return this;
        }

        /** Field 40: the marker file's path. */
        public Builder markerPath(String v) {
            this.markerPath = v;
            return this;
        }

        /** Field 41: the marker's trusted revision. */
        public Builder markerRevision(String v) {
            this.markerRevision = v;
            return this;
        }

        /** Field 43: one entry per global library the definition loaded. */
        public Builder libraries(List<BuildLibrary> v) {
            this.libraries = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        /** Field 44: the script path the run's definition declares. */
        public Builder definitionPath(String v) {
            this.definitionPath = v;
            return this;
        }

        /** Field 45: one entry per pod template the run allocated. */
        public Builder podTemplates(List<BuildPodTemplate> v) {
            this.podTemplates = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        /** Field 46: the run's summary text. */
        public Builder summary(String v) {
            this.summary = v;
            return this;
        }

        /**
         * Field 47: one entry per pending {@code input} step. An empty
         * list reports a run that waits on none, which a {@code
         * HEARTBEAT} event states on every tick.
         */
        public Builder inputs(List<BuildInput> v) {
            this.inputs = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        /** Field 48: the pointer to the archived test report. Null for no report. */
        public Builder testReport(BuildTestReport v) {
            this.testReport = v;
            return this;
        }

        /** Field 49: the failed cases, then the skipped cases, in walk order. */
        public Builder testCases(List<BuildTestCase> v) {
            this.testCases = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        /** Field 50: the run a replayed build copied its script from. */
        public Builder replayOf(String v) {
            this.replayOf = v;
            return this;
        }

        /** Field 57: the script signature the sandbox refused. */
        public Builder rejectedSignature(String v) {
            this.rejectedSignature = v;
            return this;
        }

        /** Field 51: the grant the run applied. */
        public Builder grantedBuildNumber(long v) {
            this.grantedBuildNumber = v;
            return this;
        }

        /** Field 52: the run a restarted build reran from. */
        public Builder restartOf(String v) {
            this.restartOf = v;
            return this;
        }

        /** Field 53: the stage a restarted build began at. */
        public Builder restartStage(String v) {
            this.restartStage = v;
            return this;
        }

        /** Field 54: the stages a completed declarative run can restart from. */
        public Builder restartStages(List<String> v) {
            this.restartStages = new ArrayList<>(v);
            return this;
        }

        /** Field 55: why a completed run cannot restart. */
        public Builder restartRefusal(String v) {
            this.restartRefusal = v;
            return this;
        }

        public BuildEvent build() {
            return new BuildEvent(this);
        }
    }
}
