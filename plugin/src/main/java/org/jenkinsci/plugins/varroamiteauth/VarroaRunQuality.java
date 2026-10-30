package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Reads the coverage and warnings summaries of a completed run.
 *
 * <p>The plugin holds no compile dependency on the coverage or warnings-ng
 * plugins. An older installed plugin would stop this plugin from loading. The
 * reader matches each action by its exact class name and calls each getter
 * by reflection. A read failure skips one report at FINE. The run still
 * completes.
 *
 * <p>The reader keeps at most 20 reports of each kind, at most 16 extra
 * coverage metrics per report, and at most 128 code points of each name.
 * It counts what it does not keep. Each count travels with the event, so
 * the operator sees that a set was trimmed.
 */
final class VarroaRunQuality {

    private static final Logger LOGGER = Logger.getLogger(VarroaRunQuality.class.getName());

    static final String COVERAGE_ACTION = "io.jenkins.plugins.coverage.metrics.steps.CoverageBuildAction";
    static final String WARNINGS_ACTION = "io.jenkins.plugins.analysis.core.model.ResultAction";
    static final String BASELINE_ENUM = "io.jenkins.plugins.coverage.metrics.model.Baseline";

    /** The most reports kept for each kind. */
    static final int MAX_REPORTS = 20;

    /** The most extra coverage metrics kept for one report. */
    static final int MAX_METRICS = 16;

    /** The most code points kept from an id or a name. */
    static final int MAX_TEXT_CODE_POINTS = 128;

    private VarroaRunQuality() {
    }

    /**
     * The two lists a completed run reports, and the count of reports of
     * each kind the reader did not keep.
     */
    static final class Reports {
        final List<BuildCoverageReport> coverage;
        final List<BuildWarningsReport> warnings;
        final int coverageDropped;
        final int warningsDropped;

        Reports(List<BuildCoverageReport> coverage, List<BuildWarningsReport> warnings,
                int coverageDropped, int warningsDropped) {
            this.coverage = coverage;
            this.warnings = warnings;
            this.coverageDropped = coverageDropped;
            this.warningsDropped = warningsDropped;
        }
    }

    /** Reads both kinds of report from the actions of a run. */
    static Reports read(List<? extends Action> actions) {
        return collect(
                VarroaRunRecorder.findActionsByClassName(actions, COVERAGE_ACTION),
                VarroaRunRecorder.findActionsByClassName(actions, WARNINGS_ACTION));
    }

    /**
     * Reads the given actions. A failed action skips only its own report.
     * An action past the bound is not read. It counts as dropped, even when
     * it would have failed to read.
     */
    static Reports collect(List<?> coverageActions, List<?> warningsActions) {
        List<BuildCoverageReport> coverage = new ArrayList<>();
        int coverageDropped = 0;
        for (int i = 0; i < coverageActions.size(); i++) {
            if (coverage.size() >= MAX_REPORTS) {
                coverageDropped = coverageActions.size() - i;
                break;
            }
            Object action = coverageActions.get(i);
            try {
                BuildCoverageReport report = readCoverage(action);
                if (report != null) {
                    coverage.add(report);
                }
            } catch (RuntimeException | LinkageError e) {
                LOGGER.log(Level.FINE, "VarroaRunQuality: skipped a coverage report", e);
            }
        }
        List<BuildWarningsReport> warnings = new ArrayList<>();
        int warningsDropped = 0;
        for (int i = 0; i < warningsActions.size(); i++) {
            if (warnings.size() >= MAX_REPORTS) {
                warningsDropped = warningsActions.size() - i;
                break;
            }
            Object action = warningsActions.get(i);
            try {
                BuildWarningsReport report = readWarnings(action);
                if (report != null) {
                    warnings.add(report);
                }
            } catch (RuntimeException | LinkageError e) {
                LOGGER.log(Level.FINE, "VarroaRunQuality: skipped a warnings report", e);
            }
        }
        return new Reports(coverage, warnings, coverageDropped, warningsDropped);
    }

    /** Reads one coverage action. Returns null when the action has no URL name. */
    static BuildCoverageReport readCoverage(Object action) {
        Object id = VarroaRunRecorder.invokeNoArg(action, "getUrlName");
        if (!(id instanceof String) || ((String) id).isEmpty()) {
            LOGGER.log(Level.FINE, "VarroaRunQuality: a coverage action has no URL name");
            return null;
        }
        Object name = VarroaRunRecorder.invokeNoArg(action, "getDisplayName");
        Class<?> baseline;
        try {
            baseline = Class.forName(BASELINE_ENUM, false, action.getClass().getClassLoader());
        } catch (ClassNotFoundException e) {
            LOGGER.log(Level.FINE, "VarroaRunQuality: the coverage Baseline class is missing", e);
            return null;
        }
        Object project = enumConstant(baseline, "PROJECT");
        Object modified = enumConstant(baseline, "MODIFIED_LINES");

        Object values = VarroaRunRecorder.invokeOneArg(action, "getAllValues", baseline, project);
        if (!(values instanceof List)) {
            LOGGER.log(Level.FINE, "VarroaRunQuality: the coverage values are not a list");
            return null;
        }
        BuildCoverageCounter line = null;
        BuildCoverageCounter branch = null;
        List<BuildCoverageCounter> metrics = new ArrayList<>();
        int metricsDropped = 0;
        for (Object value : (List<?>) values) {
            BuildCoverageCounter counter = coverageCounter(value);
            if (counter == null) {
                continue;
            }
            if ("LINE".equals(counter.getMetric())) {
                line = counter;
            } else if ("BRANCH".equals(counter.getMetric())) {
                branch = counter;
            } else if (metrics.size() < MAX_METRICS) {
                metrics.add(counter);
            } else {
                metricsDropped++;
            }
        }

        BuildCoverageCounter modifiedLines = null;
        Object modifiedValues = VarroaRunRecorder.invokeOneArg(action, "getAllValues", baseline, modified);
        if (modifiedValues instanceof List) {
            for (Object value : (List<?>) modifiedValues) {
                BuildCoverageCounter counter = coverageCounter(value);
                if (counter != null && "LINE".equals(counter.getMetric())) {
                    modifiedLines = counter;
                    break;
                }
            }
        }

        return BuildCoverageReport.builder()
                .id((String) id)
                .name(name instanceof String ? (String) name : "")
                .line(line)
                .branch(branch)
                .modifiedLines(modifiedLines)
                .metrics(metrics)
                .metricsDropped(metricsDropped)
                .gateStatus(gateStatusOf(VarroaRunRecorder.invokeNoArg(action, "getQualityGateResult")))
                .build();
    }

    /** Reads one warnings action. Returns null when the action has no id or no result. */
    static BuildWarningsReport readWarnings(Object action) {
        Object id = VarroaRunRecorder.invokeNoArg(action, "getId");
        if (!(id instanceof String) || ((String) id).isEmpty()) {
            LOGGER.log(Level.FINE, "VarroaRunQuality: a warnings action has no id");
            return null;
        }
        Object name = VarroaRunRecorder.invokeNoArg(action, "getDisplayName");
        Object result = VarroaRunRecorder.invokeNoArg(action, "getResult");
        if (result == null) {
            LOGGER.log(Level.FINE, "VarroaRunQuality: a warnings action has no result");
            return null;
        }
        Object reference = VarroaRunRecorder.invokeNoArg(result, "getReferenceBuild");
        boolean hasReference = reference instanceof Optional && ((Optional<?>) reference).isPresent();
        int newTotal = 0;
        int fixedTotal = 0;
        if (hasReference) {
            newTotal = size(result, "getNewSize");
            fixedTotal = size(result, "getFixedSize");
        }
        return BuildWarningsReport.builder()
                .id((String) id)
                .name(name instanceof String ? (String) name : "")
                .total(size(result, "getTotalSize"))
                .error(size(result, "getTotalErrorsSize"))
                .high(size(result, "getTotalHighPrioritySize"))
                .normal(size(result, "getTotalNormalPrioritySize"))
                .low(size(result, "getTotalLowPrioritySize"))
                .hasReference(hasReference)
                .newTotal(newTotal)
                .fixedTotal(fixedTotal)
                .gateStatus(gateStatusOf(VarroaRunRecorder.invokeNoArg(result, "getQualityGateResult")))
                .build();
    }

    /**
     * Returns the status name of a quality gate result. Returns an empty
     * string for a null gate or an inactive gate.
     */
    static String gateStatusOf(Object gate) {
        if (gate == null) {
            return "";
        }
        Object status = VarroaRunRecorder.invokeNoArg(gate, "getOverallStatus");
        if (!(status instanceof Enum)) {
            return "";
        }
        String name = ((Enum<?>) status).name();
        return "INACTIVE".equals(name) ? "" : name;
    }

    /** Cuts a text value to 128 code points. A null value becomes an empty string. */
    static String truncateText(String s) {
        if (s == null) {
            return "";
        }
        if (s.codePointCount(0, s.length()) <= MAX_TEXT_CODE_POINTS) {
            return s;
        }
        return s.substring(0, s.offsetByCodePoints(0, MAX_TEXT_CODE_POINTS));
    }

    /** Returns one coverage counter, or null for a value that is not a coverage metric. */
    private static BuildCoverageCounter coverageCounter(Object value) {
        Object metric = VarroaRunRecorder.invokeNoArg(value, "getMetric");
        if (!(metric instanceof Enum)) {
            return null;
        }
        if (!Boolean.TRUE.equals(VarroaRunRecorder.invokeNoArg(metric, "isCoverage"))) {
            return null;
        }
        Object covered = VarroaRunRecorder.invokeNoArg(value, "getCovered");
        Object total = VarroaRunRecorder.invokeNoArg(value, "getTotal");
        if (!(covered instanceof Number) || !(total instanceof Number)) {
            return null;
        }
        return BuildCoverageCounter.builder()
                .metric(((Enum<?>) metric).name())
                .covered(((Number) covered).intValue())
                .total(((Number) total).intValue())
                .build();
    }

    /** Returns a size getter's value. A missing or non-numeric value fails the report. */
    private static int size(Object target, String methodName) {
        Object v = VarroaRunRecorder.invokeNoArg(target, methodName);
        if (!(v instanceof Number)) {
            throw new IllegalStateException("VarroaRunQuality: " + methodName + " is not a number");
        }
        return ((Number) v).intValue();
    }

    @SuppressWarnings({"unchecked", "rawtypes"})
    private static Object enumConstant(Class<?> enumType, String name) {
        return Enum.valueOf((Class) enumType, name);
    }
}
