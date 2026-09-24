package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.doReturn;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.FilePath;
import hudson.Launcher;
import hudson.model.Action;
import hudson.model.BuildListener;
import hudson.model.Job;
import hudson.model.Result;
import hudson.model.Run;

import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

import jenkins.model.ArtifactManager;
import jenkins.model.ArtifactManagerConfiguration;
import jenkins.model.ArtifactManagerFactory;
import jenkins.model.ArtifactManagerFactoryDescriptor;
import jenkins.model.StandardArtifactManager;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.TestExtension;

/**
 * Tests for {@link VarroaTestReport}: the reflective junit walk, the report
 * file, the archive call, the event's bounded set, and the caps. The fake
 * classes match by simple name and method name, the way the plugin reads
 * the real junit classes. The {@code JenkinsRule} tests run a real {@code
 * junit} step.
 */
public class VarroaTestReportTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    // ---- The fakes ----

    /** The junit status constants, matched by constant name. */
    public enum FakeStatus {
        PASSED, SKIPPED, FAILED, FIXED, REGRESSION
    }

    /** A test result action matched by simple name, with the three counters. */
    public static class AbstractTestResultAction implements Action {
        private final Object result;

        public AbstractTestResultAction(Object result) {
            this.result = result;
        }

        public Object getResult() {
            return result;
        }

        public int getTotalCount() {
            return 5;
        }

        public int getFailCount() {
            return 2;
        }

        public int getSkipCount() {
            return 1;
        }

        @Override
        public String getIconFileName() {
            return null;
        }

        @Override
        public String getDisplayName() {
            return "Test Result";
        }

        @Override
        public String getUrlName() {
            return null;
        }
    }

    /** A reporter's action whose hierarchy holds the simple name through its superclass. */
    public static class ReporterAction extends AbstractTestResultAction {
        public ReporterAction(Object result) {
            super(result);
        }
    }

    /** A reporter whose {@code getResult} throws. */
    public static class ThrowingAction extends AbstractTestResultAction {
        public ThrowingAction() {
            super(null);
        }

        @Override
        public Object getResult() {
            throw new IllegalStateException("the reporter failed");
        }
    }

    /** A result with suites. */
    public static class FakeResult {
        private final List<FakeSuite> suites;

        public FakeResult(List<FakeSuite> suites) {
            this.suites = suites;
        }

        public Collection<FakeSuite> getSuites() {
            return suites;
        }
    }

    /** A result of another reporter, which declares no suites. */
    public static class NoSuitesResult {
        public int getTotalCount() {
            return 5;
        }
    }

    public static class FakeSuite {
        private final String name;
        private final List<FakeCase> cases;

        public FakeSuite(String name, List<FakeCase> cases) {
            this.name = name;
            this.cases = cases;
        }

        public String getName() {
            return name;
        }

        public List<FakeCase> getCases() {
            return cases;
        }
    }

    public static class FakeCase {
        private final String className;
        private final String name;
        private final FakeStatus status;
        private final float duration;
        private final boolean skipped;
        private String errorDetails;
        private String errorStackTrace;
        private String stdout;
        private String stderr;

        public FakeCase(String className, String name, FakeStatus status, float duration, boolean skipped) {
            this.className = className;
            this.name = name;
            this.status = status;
            this.duration = duration;
            this.skipped = skipped;
        }

        FakeCase error(String details, String stack) {
            this.errorDetails = details;
            this.errorStackTrace = stack;
            return this;
        }

        FakeCase output(String out, String err) {
            this.stdout = out;
            this.stderr = err;
            return this;
        }

        public String getClassName() {
            return className;
        }

        public String getName() {
            return name;
        }

        public FakeStatus getStatus() {
            return status;
        }

        public float getDuration() {
            return duration;
        }

        public String getErrorDetails() {
            return errorDetails;
        }

        public String getErrorStackTrace() {
            return errorStackTrace;
        }

        public String getStdout() {
            return stdout;
        }

        public String getStderr() {
            return stderr;
        }

        public boolean isSkipped() {
            return skipped;
        }
    }

    private static FakeCase fcase(String cls, String name, FakeStatus status) {
        return new FakeCase(cls, name, status, 0.5f, status == FakeStatus.SKIPPED);
    }

    private static Run<?, ?> runWith(List<Action> actions) {
        Run<?, ?> run = mock(Run.class);
        doReturn(actions).when(run).getAllActions();
        return run;
    }

    private static String repeat(String s, int times) {
        StringBuilder sb = new StringBuilder(s.length() * times);
        for (int i = 0; i < times; i++) {
            sb.append(s);
        }
        return sb.toString();
    }

    private static int utf8(String s) {
        return s.getBytes(StandardCharsets.UTF_8).length;
    }

    // ---- The walk and the mapping ----

    @Test
    public void theWalkReadsEverySuiteAndMapsStatusAndDuration() {
        FakeSuite a = new FakeSuite("suite-a", List.of(
                new FakeCase("com.example.A", "passes", FakeStatus.PASSED, 1.25f, false),
                new FakeCase("com.example.A", "fails", FakeStatus.FAILED, 0.001f, false).error("boom", "at A"),
                new FakeCase("com.example.A", "regresses", FakeStatus.REGRESSION, -3f, false)));
        FakeSuite b = new FakeSuite("suite-b", List.of(
                new FakeCase("com.example.B", "fixed", FakeStatus.FIXED, Float.NaN, false),
                new FakeCase("com.example.B", "skipsUnderPassed", FakeStatus.PASSED, 0f, true),
                new FakeCase("com.example.B", "noStatus", null, Float.POSITIVE_INFINITY, false)));
        Run<?, ?> run = runWith(List.of(new ReporterAction(new FakeResult(List.of(a, b)))));

        Object result = VarroaTestReport.findResult(run);
        assertTrue("the walk finds the action through its superclass", result instanceof FakeResult);
        assertTrue(VarroaTestReport.hasSuites(result));
        List<VarroaTestReport.Case> cases = VarroaTestReport.readCases(result);

        assertEquals("the walk reads every case of both suites", 6, cases.size());
        assertEquals("suite-a", cases.get(0).suite);
        assertEquals("com.example.A", cases.get(0).className);
        assertEquals("passes", cases.get(0).name);
        assertEquals("PASSED", cases.get(0).status);
        assertEquals("1.25 seconds is 1250 ms", 1250L, cases.get(0).durationMs);
        assertEquals("FAILED", cases.get(1).status);
        assertEquals(1L, cases.get(1).durationMs);
        assertEquals("boom", cases.get(1).errorDetails);
        assertEquals("at A", cases.get(1).errorStackTrace);
        assertEquals("REGRESSION", cases.get(2).status);
        assertEquals("a negative duration is zero", 0L, cases.get(2).durationMs);
        assertEquals("suite-b", cases.get(3).suite);
        assertEquals("a new status name reaches the value unchanged", "FIXED", cases.get(3).status);
        assertEquals("a NaN duration is zero", 0L, cases.get(3).durationMs);
        assertEquals("a case that reports itself skipped carries SKIPPED", "SKIPPED", cases.get(4).status);
        assertEquals("a null status is empty", "", cases.get(5).status);
        assertEquals("an infinite duration is zero", 0L, cases.get(5).durationMs);
        assertEquals("a null text value is empty", "", cases.get(0).stdout);
    }

    @Test
    public void theEventCarriesTheFailedCasesFirstThenTheSkippedCases() {
        FakeSuite s = new FakeSuite("s", List.of(
                fcase("C", "skip-1", FakeStatus.SKIPPED),
                fcase("C", "fail-1", FakeStatus.FAILED),
                fcase("C", "pass-1", FakeStatus.PASSED),
                new FakeCase("C", "skip-2", FakeStatus.PASSED, 0f, true),
                fcase("C", "regress-1", FakeStatus.REGRESSION),
                fcase("C", "fixed-1", FakeStatus.FIXED),
                fcase("C", "skip-3", FakeStatus.SKIPPED)));
        List<VarroaTestReport.Case> cases = VarroaTestReport.readCases(new FakeResult(List.of(s)));

        List<BuildTestCase> bounded = VarroaTestReport.boundedCases(cases);

        List<String> names = new ArrayList<>();
        for (BuildTestCase c : bounded) {
            names.add(c.getName() + ":" + c.getStatus());
        }
        assertEquals("failed cases first in walk order, then skipped cases in walk order, no passed case",
                List.of("fail-1:FAILED", "regress-1:REGRESSION", "skip-1:SKIPPED", "skip-2:SKIPPED", "skip-3:SKIPPED"),
                names);
        assertEquals(500L, bounded.get(0).getDurationMs());
    }

    @Test
    public void aRunWithAllPassedCasesCarriesAnEmptySet() {
        FakeSuite s = new FakeSuite("s", List.of(fcase("C", "a", FakeStatus.PASSED), fcase("C", "b", FakeStatus.FIXED)));
        List<BuildTestCase> bounded = VarroaTestReport.boundedCases(
                VarroaTestReport.readCases(new FakeResult(List.of(s))));
        assertTrue("a passed case never rides the event", bounded.isEmpty());

        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).testCases(bounded).build();
        assertFalse("an empty set writes no test_cases key", event.toJson().containsKey("test_cases"));
        assertFalse("a null report writes no test_report key", event.toJson().containsKey("test_report"));
    }

    // ---- A reporter with no suites, no action, a throwing reporter ----

    @Test
    public void aResultWithNoSuitesProducesNoReportAndNoCaseAndKeepsTheCounters() {
        AbstractTestResultAction action = new AbstractTestResultAction(new NoSuitesResult());
        Run<?, ?> run = runWith(List.of(action));

        Object result = VarroaTestReport.findResult(run);

        assertTrue(result instanceof NoSuitesResult);
        assertFalse("a result with no getSuites produces no report", VarroaTestReport.hasSuites(result));
        assertTrue("a result with no getSuites carries no case", VarroaTestReport.readCases(result).isEmpty());
        assertTrue(VarroaTestReport.boundedCases(VarroaTestReport.readCases(result)).isEmpty());
        assertEquals("the aggregate counters stay", List.of(5, 2, 2, 1),
                toList(VarroaRunRecorder.readTestCounters(List.of(action))));
    }

    @Test
    public void aRunWithNoTestActionAnswersNoResult() {
        assertNull(VarroaTestReport.findResult(runWith(List.of())));
        assertNull(VarroaTestReport.findResult(null));
        assertFalse(VarroaTestReport.hasSuites(null));
        assertTrue(VarroaTestReport.readCases(null).isEmpty());
    }

    @Test
    public void aThrowingReporterAnswersNoResultAndThrowsNothing() {
        Run<?, ?> run = runWith(List.of(new ThrowingAction()));
        assertNull(VarroaTestReport.findResult(run));
    }

    // ---- The report file ----

    @Test
    public void theReportFileHoldsEveryCaseIncludingAPassedCase() throws Exception {
        Run<?, ?> run = runWith(List.of());
        when(run.getNumber()).thenReturn(42);
        Job<?, ?> job = mock(Job.class);
        when(job.getFullName()).thenReturn("team-a/job-x");
        doReturn(job).when(run).getParent();
        List<VarroaTestReport.Case> cases = VarroaTestReport.readCases(new FakeResult(List.of(
                new FakeSuite("s", List.of(
                        fcase("com.example.A", "[1] value", FakeStatus.PASSED),
                        fcase("com.example.A", "fails", FakeStatus.FAILED))))));
        Path dir = Files.createTempDirectory("varroa-tests-test");
        try {
            Path file = VarroaTestReport.writeReportFile(dir, run, cases);

            assertEquals(dir.resolve(".varroa").resolve("tests.json"), file);
            JSONObject o = JSONObject.fromObject(Files.readString(file, StandardCharsets.UTF_8));
            assertEquals(1, o.getInt("schema"));
            assertEquals("", o.getString("runId"));
            assertEquals("team-a/job-x", o.getString("jobPath"));
            assertEquals(42L, o.getLong("buildNumber"));
            assertEquals(2, o.getInt("caseCount"));
            JSONArray arr = o.getJSONArray("cases");
            assertEquals(2, arr.size());
            JSONObject passed = arr.getJSONObject(0);
            assertEquals("the file holds a passed case", "PASSED", passed.getString("status"));
            assertEquals("a bracketed name stays a string", "[1] value", passed.getString("name"));
            assertEquals("com.example.A", passed.getString("className"));
            assertEquals("s", passed.getString("suite"));
            assertEquals(500L, passed.getLong("durationMs"));
            for (String key : List.of("errorDetails", "errorStackTrace", "stdout", "stderr")) {
                assertTrue("every entry holds " + key, passed.containsKey(key));
            }
        } finally {
            deleteTree(dir);
        }
    }

    @Test
    public void theFileCutsTheThreeTextValuesAt4KiBAndKeepsTheErrorDetails() throws Exception {
        String stack = repeat("x", 100 * 1024);
        String out = repeat("é", 10 * 1024);
        String err = repeat("e", 10 * 1024);
        String details = repeat("d", 10 * 1024);
        List<VarroaTestReport.Case> cases = VarroaTestReport.readCases(new FakeResult(List.of(
                new FakeSuite("s", List.of(
                        new FakeCase("C", "fails", FakeStatus.FAILED, 1f, false)
                                .error(details, stack).output(out, err))))));
        Path dir = Files.createTempDirectory("varroa-tests-test");
        try {
            Path file = VarroaTestReport.writeReportFile(dir, runWith(List.of()), cases);
            JSONObject c = JSONObject.fromObject(Files.readString(file, StandardCharsets.UTF_8))
                    .getJSONArray("cases").getJSONObject(0);

            assertEquals("the stack trace is cut at 4 KiB", 4096, utf8(c.getString("errorStackTrace")));
            assertEquals("stdout is cut on a whole character at or under 4 KiB", 4096, utf8(c.getString("stdout")));
            assertEquals("stderr is cut at 4 KiB", 4096, utf8(c.getString("stderr")));
            assertEquals("the error details ride the file uncut", details, c.getString("errorDetails"));
        } finally {
            deleteTree(dir);
        }
    }

    // ---- The event caps ----

    @Test
    public void theCountCapKeepsTheFirst500Cases() {
        List<FakeCase> list = new ArrayList<>();
        for (int i = 0; i < 600; i++) {
            list.add(fcase("C", "fail-" + i, FakeStatus.FAILED));
        }
        for (int i = 0; i < 10; i++) {
            list.add(fcase("C", "skip-" + i, FakeStatus.SKIPPED));
        }
        List<BuildTestCase> bounded = VarroaTestReport.boundedCases(
                VarroaTestReport.readCases(new FakeResult(List.of(new FakeSuite("s", list)))));

        assertEquals(500, bounded.size());
        assertEquals("fail-0", bounded.get(0).getName());
        assertEquals("fail-499", bounded.get(499).getName());
        for (BuildTestCase c : bounded) {
            assertEquals("no skipped case survives the cap", "FAILED", c.getStatus());
        }
    }

    @Test
    public void theErrorDetailsAreCutAt512BytesOnACharacterBoundary() {
        // Each euro sign is three bytes, so 512 is not a character boundary.
        String details = repeat("€", 400);
        List<BuildTestCase> bounded = VarroaTestReport.boundedCases(VarroaTestReport.readCases(new FakeResult(List.of(
                new FakeSuite("s", List.of(new FakeCase("C", "fails", FakeStatus.FAILED, 0f, false)
                        .error(details, "")))))));

        String cut = bounded.get(0).getErrorDetails();
        assertEquals("the largest whole character boundary at or under 512 bytes", 510, utf8(cut));
        assertEquals(repeat("€", 170), cut);
    }

    @Test
    public void everyOtherCaseStringIsCutAt512Bytes() {
        String longValue = repeat("n", 2000);
        FakeCase c = new FakeCase(longValue, longValue, FakeStatus.FAILED, 0f, false);
        List<BuildTestCase> bounded = VarroaTestReport.boundedCases(VarroaTestReport.readCases(
                new FakeResult(List.of(new FakeSuite(longValue, List.of(c))))));

        BuildTestCase out = bounded.get(0);
        assertEquals(512, utf8(out.getSuite()));
        assertEquals(512, utf8(out.getClassName()));
        assertEquals(512, utf8(out.getName()));
        assertEquals(512, utf8(BuildTestCase.builder().status(longValue).build().getStatus()));
        JSONObject json = out.toJson();
        for (String key : List.of("suite", "class_name", "name", "status", "duration_ms", "error_details")) {
            assertTrue("the case JSON holds the proto name " + key, json.containsKey(key));
        }
    }

    // ---- The report pointer ----

    @Test
    public void aScanThatFindsNoEntryAnswersAnEmptyBlobKeyWithTheSizeAndTheCount() {
        List<BuildArtifact> artifacts = List.of(
                BuildArtifact.builder().path("out.txt").blobKey("prefix/job/1/artifacts/out.txt").build());

        BuildTestReport report = VarroaTestReport.reportPointer(artifacts, 1234L, 7);

        assertEquals("", report.getBlobKey());
        assertEquals(1234L, report.getSizeBytes());
        assertEquals(7, report.getCaseCount());
        JSONObject json = report.toJson();
        assertEquals("", json.getString("blob_key"));
        assertEquals(1234L, json.getLong("size_bytes"));
        assertEquals(7, json.getInt("case_count"));
    }

    @Test
    public void theScanTakesTheBlobKeyOfTheCollectedEntry() {
        String key = "prefix/job/1/artifacts/.varroa/tests.json";
        List<BuildArtifact> artifacts = List.of(
                BuildArtifact.builder().path("out.txt").blobKey("other").build(),
                BuildArtifact.builder().path(VarroaTestReport.REPORT_PATH).blobKey(key).build());

        BuildTestReport report = VarroaTestReport.reportPointer(artifacts, 99L, 3);

        assertEquals(key, report.getBlobKey());
        assertEquals(99L, report.getSizeBytes());
        assertEquals(3, report.getCaseCount());
    }

    // ---- JenkinsRule: a real junit step ----

    private static final String REPORT_XML =
            "<testsuite name=\"suite-a\" tests=\"3\" failures=\"1\" skipped=\"1\">"
                    + "<testcase classname=\"com.example.ATest\" name=\"passes\" time=\"0.5\"/>"
                    + "<testcase classname=\"com.example.ATest\" name=\"fails\" time=\"1.25\">"
                    + "<failure message=\"expected 1\">stack line</failure></testcase>"
                    + "<testcase classname=\"com.example.ATest\" name=\"skips\" time=\"0\"><skipped/></testcase>"
                    + "</testsuite>";

    private WorkflowRun runJunitPipeline(String name) throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, name + "-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  writeFile file: 'report.xml', text: '" + REPORT_XML + "'\n"
                        + "  junit 'report.xml'\n"
                        + "}", true));
        String runId = Uuid7.next();
        WorkflowRun run = p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.UNSTABLE, run);
        return run;
    }

    @Test
    public void aPipelineWithAFailureAndASkipArchivesTheReport() throws Exception {
        WorkflowRun run = runJunitPipeline("junit-report");

        List<String> paths = new ArrayList<>();
        for (var a : run.getArtifacts()) {
            paths.add(a.relativePath);
        }
        assertTrue("the report is archived, artifacts=" + paths, paths.contains(VarroaTestReport.REPORT_PATH));
        boolean collected = false;
        for (BuildArtifact a : ArtifactCollector.collect(run)) {
            if (VarroaTestReport.REPORT_PATH.equals(a.getPath())) {
                collected = true;
                assertEquals("local mode carries no blob key", "", a.getBlobKey());
            }
        }
        assertTrue("the collector reports the report", collected);

        String text;
        try (InputStream in = run.getArtifactManager().root().child(VarroaTestReport.REPORT_PATH).open()) {
            text = new String(in.readAllBytes(), StandardCharsets.UTF_8);
        }
        JSONObject report = JSONObject.fromObject(text);
        assertEquals("the report holds every case, text=" + text, 3, report.getInt("caseCount"));
        assertEquals(3, report.getJSONArray("cases").size());
        assertEquals(run.getParent().getFullName(), report.getString("jobPath"));
        assertEquals(run.getNumber(), report.getInt("buildNumber"));

        BuildEvent completed = findCompleted(run.getAction(VarroaIntentAction.class).getRunId());
        assertNotNull("a COMPLETED event must be published for the run", completed);
        BuildTestReport pointer = completed.getTestReport();
        assertNotNull("the COMPLETED event carries the report pointer", pointer);
        assertEquals("", pointer.getBlobKey());
        assertEquals(text.getBytes(StandardCharsets.UTF_8).length, pointer.getSizeBytes());
        assertEquals(3, pointer.getCaseCount());
        List<BuildTestCase> cases = completed.getTestCases();
        assertEquals("one failed case and one skipped case, cases=" + cases.size(), 2, cases.size());
        assertEquals("fails", cases.get(0).getName());
        assertEquals("FAILED", cases.get(0).getStatus());
        assertEquals(1250L, cases.get(0).getDurationMs());
        assertEquals("skips", cases.get(1).getName());
        assertEquals("SKIPPED", cases.get(1).getStatus());
        assertEquals(3, completed.getTestTotal());
    }

    @Test
    public void aRunWithNoTestResultArchivesNothing() throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "no-junit-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition("echo 'no tests'\n", true));
        String runId = Uuid7.next();
        WorkflowRun run = p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(run);

        assertTrue("no artifact is archived", run.getArtifacts().isEmpty());
        BuildEvent completed = findCompleted(runId);
        assertNotNull(completed);
        assertNull("no report pointer", completed.getTestReport());
        assertTrue("no case", completed.getTestCases().isEmpty());
    }

    /** An artifact manager whose archive call always throws. */
    public static class ThrowingArtifactManager extends StandardArtifactManager {
        public ThrowingArtifactManager(Run<?, ?> build) {
            super(build);
        }

        @Override
        public void archive(FilePath workspace, Launcher launcher, BuildListener listener,
                Map<String, String> artifacts) throws IOException {
            throw new IOException("the artifact store is not available");
        }
    }

    public static class ThrowingArtifactManagerFactory extends ArtifactManagerFactory {
        @Override
        public ArtifactManager managerFor(Run<?, ?> build) {
            return new ThrowingArtifactManager(build);
        }

        @TestExtension("aThrowingArtifactManagerLeavesTheCompletionEventIntact")
        public static class DescriptorImpl extends ArtifactManagerFactoryDescriptor {
        }
    }

    @Test
    public void aThrowingArtifactManagerLeavesTheCompletionEventIntact() throws Exception {
        ArtifactManagerConfiguration.get().getArtifactManagerFactories().add(new ThrowingArtifactManagerFactory());

        WorkflowRun run = runJunitPipeline("junit-throwing");

        BuildEvent completed = findCompleted(run.getAction(VarroaIntentAction.class).getRunId());
        assertNotNull("the COMPLETED event is still published", completed);
        assertEquals("UNSTABLE", completed.getResult());
        assertNull("a report that was not archived has no pointer", completed.getTestReport());
        assertEquals("the cases the read produced still ride the event", 2, completed.getTestCases().size());
        assertEquals(3, completed.getTestTotal());
    }

    // ---- Helpers ----

    private BuildEvent findCompleted(String runId) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 10_000;
        while (System.currentTimeMillis() < deadline) {
            BuildEventSink.DrainResult result = BuildEventSink.get().drain(256);
            for (BuildEvent e : result.events) {
                if (runId.equals(e.getRunId()) && e.isCompletedPhase()) {
                    return e;
                }
            }
            Thread.sleep(50);
        }
        return null;
    }

    private static List<Integer> toList(int[] values) {
        List<Integer> list = new ArrayList<>();
        for (int v : values) {
            list.add(v);
        }
        return list;
    }

    private static void deleteTree(Path dir) throws IOException {
        try (var walk = Files.walk(dir)) {
            for (Path p : walk.sorted(java.util.Comparator.reverseOrder()).toList()) {
                Files.deleteIfExists(p);
            }
        }
    }
}
