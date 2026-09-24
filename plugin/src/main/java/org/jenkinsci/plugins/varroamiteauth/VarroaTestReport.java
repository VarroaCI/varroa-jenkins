package org.jenkinsci.plugins.varroamiteauth;

import hudson.FilePath;
import hudson.Launcher;
import hudson.model.Action;
import hudson.model.BuildListener;
import hudson.model.Run;
import hudson.model.StreamBuildListener;
import hudson.model.TaskListener;

import java.io.IOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Stream;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

/**
 * Reads the run's junit test result and archives it as the report
 * artifact {@code .varroa/tests.json} (D1, D2, D3).
 *
 * <p>The plugin holds no compile dependency on {@code junit}, so every read
 * goes through {@link VarroaRunRecorder#invokeNoArg} and matches the action
 * by the simple name {@code AbstractTestResultAction} in its class
 * hierarchy. A read fails closed: a missing method, a null result, and a
 * thrown exception each answer an empty value, so no read throws into a
 * Jenkins listener.
 *
 * <p>Every method is a package-private static, and the class holds no
 * state, so a test drives each step directly.
 */
final class VarroaTestReport {

    private static final Logger LOGGER = Logger.getLogger(VarroaTestReport.class.getName());

    /** The report's path in the run's archive area and in the temporary workspace. */
    static final String REPORT_PATH = ".varroa/tests.json";

    /** The value of the report's {@code schema} key. */
    static final int SCHEMA = 1;

    /** The byte cap of each of {@code errorStackTrace}, {@code stdout}, and {@code stderr} in the file. */
    static final int MAX_FILE_TEXT_BYTES = 4 * 1024;

    static final String STATUS_SKIPPED = "SKIPPED";
    static final String STATUS_FAILED = "FAILED";
    static final String STATUS_REGRESSION = "REGRESSION";

    /** The simple class name that marks a test result action. */
    private static final String TEST_RESULT_ACTION = "AbstractTestResultAction";

    private VarroaTestReport() {
    }

    /**
     * One case of the walk, before any cap. The report file cuts the
     * three text values, and {@link BuildTestCase} cuts every string for
     * the event.
     */
    static final class Case {
        final String suite;
        final String className;
        final String name;
        final String status;
        final long durationMs;
        final String errorDetails;
        final String errorStackTrace;
        final String stdout;
        final String stderr;
        final boolean skipped;

        Case(String suite, String className, String name, String status, long durationMs,
                String errorDetails, String errorStackTrace, String stdout, String stderr, boolean skipped) {
            this.suite = orEmpty(suite);
            this.className = orEmpty(className);
            this.name = orEmpty(name);
            this.status = orEmpty(status);
            this.durationMs = durationMs;
            this.errorDetails = orEmpty(errorDetails);
            this.errorStackTrace = orEmpty(errorStackTrace);
            this.stdout = orEmpty(stdout);
            this.stderr = orEmpty(stderr);
            this.skipped = skipped;
        }

        boolean isFailed() {
            return STATUS_FAILED.equals(status) || STATUS_REGRESSION.equals(status);
        }

        boolean isSkipped() {
            return skipped || STATUS_SKIPPED.equals(status);
        }
    }

    // ---- The read (D1) ----

    /**
     * Returns the result object of the run's test result action, or
     * {@code null} for a run with no such action. The action matches when
     * its class hierarchy holds the simple name {@code
     * AbstractTestResultAction}, the rule the test counters also use.
     */
    static Object findResult(Run<?, ?> run) {
        if (run == null) {
            return null;
        }
        try {
            for (Action action : run.getAllActions()) {
                if (action != null
                        && VarroaRunRecorder.hasSimpleNameInHierarchy(action.getClass(), TEST_RESULT_ACTION)) {
                    return VarroaRunRecorder.invokeNoArg(action, "getResult");
                }
            }
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaTestReport: could not read the test result action", e);
        }
        return null;
    }

    /**
     * Reports whether {@code result} answers a suite collection. A result
     * that declares no {@code getSuites()} produces no report and no case.
     */
    static boolean hasSuites(Object result) {
        return result != null && VarroaRunRecorder.invokeNoArg(result, "getSuites") instanceof Iterable;
    }

    /**
     * Walks every suite and every case of {@code result} once, in the
     * order the reporter answers them. Answers an empty list for a null
     * result, for a result with no {@code getSuites()}, and for a walk
     * that throws.
     *
     * <p>A skipped case carries the status {@code SKIPPED}. Every other
     * case carries the reporter's own status name, so a reader that
     * filters on the status name gets the same classification as
     * {@link #boundedCases}.
     */
    static List<Case> readCases(Object result) {
        List<Case> cases = new ArrayList<>();
        if (result == null) {
            return cases;
        }
        Object suites = VarroaRunRecorder.invokeNoArg(result, "getSuites");
        if (!(suites instanceof Iterable)) {
            return cases;
        }
        try {
            for (Object suite : (Iterable<?>) suites) {
                if (suite == null) {
                    continue;
                }
                String suiteName = stringOf(VarroaRunRecorder.invokeNoArg(suite, "getName"));
                Object suiteCases = VarroaRunRecorder.invokeNoArg(suite, "getCases");
                if (!(suiteCases instanceof Iterable)) {
                    continue;
                }
                for (Object c : (Iterable<?>) suiteCases) {
                    if (c != null) {
                        cases.add(readCase(suiteName, c));
                    }
                }
            }
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaTestReport: could not walk the test suites", e);
            return new ArrayList<>();
        }
        return cases;
    }

    private static Case readCase(String suiteName, Object c) {
        boolean skipped = Boolean.TRUE.equals(VarroaRunRecorder.invokeNoArg(c, "isSkipped"));
        String rawStatus = statusName(VarroaRunRecorder.invokeNoArg(c, "getStatus"));
        if (STATUS_SKIPPED.equals(rawStatus)) {
            skipped = true;
        }
        return new Case(
                suiteName,
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getClassName")),
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getName")),
                skipped ? STATUS_SKIPPED : rawStatus,
                durationMs(VarroaRunRecorder.invokeNoArg(c, "getDuration")),
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getErrorDetails")),
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getErrorStackTrace")),
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getStdout")),
                stringOf(VarroaRunRecorder.invokeNoArg(c, "getStderr")),
                skipped);
    }

    /** Answers the constant name of an enum status, and the empty string for a null status. */
    static String statusName(Object status) {
        if (status == null) {
            return "";
        }
        if (status instanceof Enum) {
            return ((Enum<?>) status).name();
        }
        return String.valueOf(status);
    }

    /**
     * Converts a duration in seconds to milliseconds. A negative, a
     * non-finite, and a non-numeric value each answer zero.
     */
    static long durationMs(Object seconds) {
        if (!(seconds instanceof Number)) {
            return 0L;
        }
        float d = ((Number) seconds).floatValue();
        if (!Float.isFinite(d) || d < 0f) {
            return 0L;
        }
        return Math.round(d * 1000f);
    }

    // ---- The report file (D2) ----

    /**
     * Writes {@code <dir>/.varroa/tests.json} and returns its path. The
     * file holds every case, a passed case included. Its keys are camel
     * case, because its reader is a person.
     */
    static Path writeReportFile(Path dir, Run<?, ?> run, List<Case> cases) throws IOException {
        Path varroaDir = dir.resolve(".varroa");
        Files.createDirectories(varroaDir);
        JSONObject o = new JSONObject();
        o.put("schema", SCHEMA);
        o.put("runId", runIdOf(run));
        o.put("jobPath", jobPathOf(run));
        o.put("buildNumber", run != null ? (long) run.getNumber() : 0L);
        o.put("caseCount", cases.size());
        JSONArray arr = new JSONArray();
        for (Case c : cases) {
            JSONObject e = new JSONObject();
            e.put("suite", c.suite);
            e.put("className", c.className);
            e.put("name", c.name);
            e.put("status", c.status);
            e.put("durationMs", c.durationMs);
            // The file is an artifact and not an event, so the error
            // details stay uncut.
            e.put("errorDetails", c.errorDetails);
            e.put("errorStackTrace", VarroaRunRecorder.truncateUtf8Bytes(c.errorStackTrace, MAX_FILE_TEXT_BYTES));
            e.put("stdout", VarroaRunRecorder.truncateUtf8Bytes(c.stdout, MAX_FILE_TEXT_BYTES));
            e.put("stderr", VarroaRunRecorder.truncateUtf8Bytes(c.stderr, MAX_FILE_TEXT_BYTES));
            arr.add(e);
        }
        o.put("cases", arr);
        Path file = varroaDir.resolve("tests.json");
        Files.write(file, o.toString().getBytes(StandardCharsets.UTF_8));
        return file;
    }

    /**
     * Writes the report into a new temporary directory and archives it
     * through the run's own artifact manager, with that directory as the
     * workspace. Answers the file's length in bytes, or {@code -1} when
     * any step failed. This method owns the temporary directory: it
     * creates it and deletes it in every case.
     */
    static long archiveReport(Run<?, ?> run, TaskListener listener, List<Case> cases) {
        Path dir = null;
        try {
            dir = Files.createTempDirectory("varroa-tests");
            Path file = writeReportFile(dir, run, cases);
            long size = Files.size(file);
            BuildListener bl = listener instanceof BuildListener
                    ? (BuildListener) listener
                    : new StreamBuildListener(OutputStream.nullOutputStream(), StandardCharsets.UTF_8);
            run.pickArtifactManager().archive(new FilePath(dir.toFile()), new Launcher.LocalLauncher(bl), bl,
                    Map.of(REPORT_PATH, REPORT_PATH));
            return size;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            LOGGER.log(Level.WARNING, "VarroaTestReport: the report archive was interrupted", e);
            return -1L;
        } catch (Exception | LinkageError e) {
            LOGGER.log(Level.WARNING, "VarroaTestReport: could not archive " + REPORT_PATH, e);
            return -1L;
        } finally {
            deleteQuietly(dir);
        }
    }

    private static void deleteQuietly(Path dir) {
        if (dir == null) {
            return;
        }
        try (Stream<Path> walk = Files.walk(dir)) {
            walk.sorted(Comparator.reverseOrder()).forEach(p -> {
                try {
                    Files.deleteIfExists(p);
                } catch (IOException e) {
                    LOGGER.log(Level.FINE, "VarroaTestReport: could not delete " + p, e);
                }
            });
        } catch (IOException | RuntimeException e) {
            LOGGER.log(Level.FINE, "VarroaTestReport: could not delete " + dir, e);
        }
    }

    // ---- The event's set and pointer (D2, D3) ----

    /**
     * Returns the failed cases in walk order, then the skipped cases in
     * walk order, at most {@link VarroaRunRecorder#MAX_TEST_CASES}. A
     * passed case is never on the list. The event builder cuts every
     * string at 512 bytes.
     */
    static List<BuildTestCase> boundedCases(List<Case> cases) {
        List<BuildTestCase> bounded = new ArrayList<>();
        if (cases == null) {
            return bounded;
        }
        for (Case c : cases) {
            if (bounded.size() == VarroaRunRecorder.MAX_TEST_CASES) {
                return bounded;
            }
            if (c.isFailed() && !c.isSkipped()) {
                bounded.add(eventCase(c));
            }
        }
        for (Case c : cases) {
            if (bounded.size() == VarroaRunRecorder.MAX_TEST_CASES) {
                return bounded;
            }
            if (c.isSkipped()) {
                bounded.add(eventCase(c));
            }
        }
        return bounded;
    }

    private static BuildTestCase eventCase(Case c) {
        return BuildTestCase.builder()
                .suite(c.suite)
                .className(c.className)
                .name(c.name)
                .status(c.status)
                .durationMs(c.durationMs)
                .errorDetails(c.errorDetails)
                .build();
    }

    /**
     * Builds the report pointer from the collected artifact list. The blob
     * key comes from the entry whose path is {@link #REPORT_PATH}, so one
     * key layout keeps one implementation. A list with no such entry
     * answers an empty blob key and keeps the size and the case count.
     */
    static BuildTestReport reportPointer(List<BuildArtifact> artifacts, long sizeBytes, int caseCount) {
        String blobKey = "";
        boolean found = false;
        if (artifacts != null) {
            for (BuildArtifact a : artifacts) {
                if (a != null && REPORT_PATH.equals(a.getPath())) {
                    blobKey = a.getBlobKey();
                    found = true;
                    break;
                }
            }
        }
        if (!found) {
            LOGGER.log(Level.FINE, "VarroaTestReport: the collected artifact list holds no " + REPORT_PATH);
        }
        return BuildTestReport.builder()
                .blobKey(blobKey)
                .sizeBytes(sizeBytes)
                .caseCount(caseCount)
                .build();
    }

    // ---- Helpers ----

    private static String runIdOf(Run<?, ?> run) {
        try {
            VarroaIntentAction action = run != null ? run.getAction(VarroaIntentAction.class) : null;
            return action != null ? VarroaRunRecorder.resolveRunId(action) : "";
        } catch (RuntimeException e) {
            return "";
        }
    }

    private static String jobPathOf(Run<?, ?> run) {
        try {
            return run != null && run.getParent() != null ? orEmpty(run.getParent().getFullName()) : "";
        } catch (RuntimeException e) {
            return "";
        }
    }

    private static String stringOf(Object v) {
        return v instanceof String ? (String) v : v != null ? String.valueOf(v) : "";
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }
}
