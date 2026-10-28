package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import io.jenkins.plugins.coverage.metrics.model.Baseline;
import io.jenkins.plugins.util.QualityGateResult;
import io.jenkins.plugins.util.QualityGateStatus;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Optional;
import org.junit.Test;

/**
 * Tests for {@link VarroaRunQuality} with fakes. The fakes stand in for the
 * coverage and warnings-ng actions, and they call the reader by reflection.
 * The reader matches the real class names, so the fakes reach the reader
 * through {@code collect}, and the class-name test checks the match rule.
 */
public class VarroaRunQualityReadTest {

    /** The coverage metrics the fakes use. {@code NONCOV} is not a coverage metric. */
    public enum FakeMetric {
        LINE, BRANCH, NONCOV,
        M0, M1, M2, M3, M4, M5, M6, M7, M8, M9,
        M10, M11, M12, M13, M14, M15, M16, M17, M18, M19;

        public boolean isCoverage() {
            return this != NONCOV;
        }
    }

    /** One coverage value, read by the reader through its getters. */
    public static class FakeValue {
        private final FakeMetric metric;
        private final Object covered;
        private final Object total;

        public FakeValue(FakeMetric metric, Object covered, Object total) {
            this.metric = metric;
            this.covered = covered;
            this.total = total;
        }

        public FakeMetric getMetric() {
            return metric;
        }

        public Object getCovered() {
            return covered;
        }

        public Object getTotal() {
            return total;
        }
    }

    /** A coverage action that answers the values of each baseline. */
    public static class FakeCoverageAction {
        private final String urlName;
        private final String displayName;
        private final List<Object> projectValues;
        private final List<Object> modifiedValues;
        private final Object gate;
        private final boolean throwOnValues;

        public FakeCoverageAction(String urlName, String displayName, List<Object> projectValues,
                List<Object> modifiedValues, Object gate) {
            this(urlName, displayName, projectValues, modifiedValues, gate, false);
        }

        public FakeCoverageAction(String urlName, String displayName, List<Object> projectValues,
                List<Object> modifiedValues, Object gate, boolean throwOnValues) {
            this.urlName = urlName;
            this.displayName = displayName;
            this.projectValues = projectValues;
            this.modifiedValues = modifiedValues;
            this.gate = gate;
            this.throwOnValues = throwOnValues;
        }

        public String getUrlName() {
            return urlName;
        }

        public String getDisplayName() {
            return displayName;
        }

        public List<Object> getAllValues(Baseline baseline) {
            if (throwOnValues) {
                throw new IllegalStateException("the values failed");
            }
            return baseline == Baseline.MODIFIED_LINES ? modifiedValues : projectValues;
        }

        public Object getQualityGateResult() {
            return gate;
        }
    }

    /** The size getters of a warnings result. Each one answers a fixed value. */
    public static class FakeResult {
        private final boolean withSizes;
        private final boolean withReference;
        private final Object gate;

        public FakeResult(boolean withSizes, boolean withReference, Object gate) {
            this.withSizes = withSizes;
            this.withReference = withReference;
            this.gate = gate;
        }

        public int getTotalSize() {
            return 3;
        }

        public int getTotalErrorsSize() {
            return 1;
        }

        public int getTotalHighPrioritySize() {
            return 0;
        }

        public int getTotalNormalPrioritySize() {
            return 1;
        }

        public int getTotalLowPrioritySize() {
            return 1;
        }

        public int getNewSize() {
            return withSizes ? 2 : 0;
        }

        public int getFixedSize() {
            return withSizes ? 1 : 0;
        }

        public Optional<Object> getReferenceBuild() {
            return withReference ? Optional.of("previous") : Optional.empty();
        }

        public Object getQualityGateResult() {
            return gate;
        }
    }

    /** A result that has no size getters. The reader skips the report. */
    public static class SizelessResult {
        public Optional<Object> getReferenceBuild() {
            return Optional.empty();
        }

        public Object getQualityGateResult() {
            return null;
        }
    }

    /** A warnings action. */
    public static class FakeWarningsAction {
        private final String id;
        private final String displayName;
        private final Object result;

        public FakeWarningsAction(String id, String displayName, Object result) {
            this.id = id;
            this.displayName = displayName;
            this.result = result;
        }

        public String getId() {
            return id;
        }

        public String getDisplayName() {
            return displayName;
        }

        public Object getResult() {
            return result;
        }
    }

    private static FakeValue value(FakeMetric m, int covered, int total) {
        return new FakeValue(m, covered, total);
    }

    private static FakeCoverageAction coverage(String id, List<Object> values) {
        return new FakeCoverageAction(id, "Coverage", values, Collections.emptyList(), null);
    }

    private static FakeWarningsAction warnings(String id, Object result) {
        return new FakeWarningsAction(id, "Checks", result);
    }

    private static BuildCoverageReport readCoverage(FakeCoverageAction action) {
        return VarroaRunQuality.readCoverage(action);
    }

    @Test
    public void coverageReadsLineBranchAndOtherCoverageMetrics() {
        List<Object> values = new ArrayList<>();
        values.add(value(FakeMetric.M0, 1, 1));
        values.add(value(FakeMetric.LINE, 4, 5));
        values.add(value(FakeMetric.BRANCH, 3, 4));
        values.add(value(FakeMetric.M1, 8, 10));
        FakeCoverageAction action = new FakeCoverageAction("jacoco", "Coverage", values,
                Collections.emptyList(), new QualityGateResult(QualityGateStatus.PASSED));

        BuildCoverageReport report = readCoverage(action);

        assertEquals("jacoco", report.getId());
        assertEquals("Coverage", report.getName());
        assertEquals(4, report.getLine().getCovered());
        assertEquals(5, report.getLine().getTotal());
        assertEquals("BRANCH", report.getBranch().getMetric());
        assertEquals(3, report.getBranch().getCovered());
        assertEquals(2, report.getMetrics().size());
        assertEquals("M0", report.getMetrics().get(0).getMetric());
        assertEquals("M1", report.getMetrics().get(1).getMetric());
        assertEquals("PASSED", report.getGateStatus());
        assertNull(report.getModifiedLines());
    }

    @Test
    public void aNonCoverageMetricIsDropped() {
        List<Object> values = new ArrayList<>();
        values.add(value(FakeMetric.NONCOV, 1, 2));
        values.add(value(FakeMetric.LINE, 1, 2));

        BuildCoverageReport report = readCoverage(coverage("jacoco", values));

        assertTrue(report.getMetrics().isEmpty());
        assertEquals(2, report.getLine().getTotal());
    }

    @Test
    public void modifiedLinesValuesSetTheCounter() {
        List<Object> modified = new ArrayList<>();
        modified.add(value(FakeMetric.LINE, 3, 9));
        FakeCoverageAction action = new FakeCoverageAction("jacoco", "Coverage",
                Collections.emptyList(), modified, null);

        BuildCoverageReport report = readCoverage(action);

        assertEquals(3, report.getModifiedLines().getCovered());
        assertEquals(9, report.getModifiedLines().getTotal());
    }

    @Test
    public void noModifiedLinesValuesLeaveTheCounterAbsent() {
        BuildCoverageReport report = readCoverage(coverage("jacoco", List.of(value(FakeMetric.LINE, 1, 2))));

        assertNull(report.getModifiedLines());
        assertFalse(report.toJson().has("modified_lines"));
    }

    @Test
    public void moreThanSixteenMetricsKeepTheFirstSixteen() {
        List<Object> values = new ArrayList<>();
        FakeMetric[] all = FakeMetric.values();
        for (FakeMetric m : all) {
            if (m.ordinal() >= FakeMetric.M0.ordinal()) {
                values.add(value(m, 1, 2));
            }
        }
        BuildCoverageReport report = readCoverage(coverage("jacoco", values));

        assertEquals(16, report.getMetrics().size());
        assertEquals("M0", report.getMetrics().get(0).getMetric());
        assertEquals("M15", report.getMetrics().get(15).getMetric());
    }

    @Test
    public void moreThanTwentyCoverageReportsKeepTheFirstTwenty() {
        List<Object> actions = new ArrayList<>();
        for (int i = 0; i < 25; i++) {
            actions.add(coverage("c" + i, List.of(value(FakeMetric.LINE, 1, 2))));
        }
        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(actions, Collections.emptyList());

        assertEquals(20, reports.coverage.size());
        assertEquals("c0", reports.coverage.get(0).getId());
        assertEquals("c19", reports.coverage.get(19).getId());
    }

    @Test
    public void moreThanTwentyWarningsReportsKeepTheFirstTwenty() {
        List<Object> actions = new ArrayList<>();
        for (int i = 0; i < 25; i++) {
            actions.add(warnings("w" + i, new FakeResult(false, false, null)));
        }
        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(Collections.emptyList(), actions);

        assertEquals(20, reports.warnings.size());
        assertEquals("w0", reports.warnings.get(0).getId());
        assertEquals("w19", reports.warnings.get(19).getId());
    }

    @Test
    public void idAndNameAreCutAt128CodePoints() {
        String exact = "a".repeat(127) + "😀";
        assertEquals(exact, VarroaRunQuality.truncateText(exact));
        String longText = "b".repeat(200);
        assertEquals(128, VarroaRunQuality.truncateText(longText).length());

        BuildCoverageReport report = readCoverage(new FakeCoverageAction(exact, longText,
                List.of(value(FakeMetric.LINE, 1, 2)), Collections.emptyList(), null));

        assertEquals(exact, report.getId());
        assertEquals(128, report.getName().length());
    }

    @Test
    public void aThrowingGetterSkipsOnlyThatReport() {
        List<Object> actions = new ArrayList<>();
        actions.add(new FakeCoverageAction("broken", "Broken", null, null, null, true));
        actions.add(coverage("good", List.of(value(FakeMetric.LINE, 1, 2))));

        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(actions, Collections.emptyList());

        assertEquals(1, reports.coverage.size());
        assertEquals("good", reports.coverage.get(0).getId());
    }

    @Test
    public void aMissingIdSkipsTheReport() {
        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(
                List.of(coverage(null, List.of(value(FakeMetric.LINE, 1, 2)))), Collections.emptyList());

        assertTrue(reports.coverage.isEmpty());
    }

    @Test
    public void aSkippedReportDoesNotCountAgainstTheBound() {
        List<Object> actions = new ArrayList<>();
        actions.add(coverage(null, List.of(value(FakeMetric.LINE, 1, 2))));
        for (int i = 0; i < 20; i++) {
            actions.add(coverage("c" + i, List.of(value(FakeMetric.LINE, 1, 2))));
        }
        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(actions, Collections.emptyList());

        assertEquals(20, reports.coverage.size());
        assertEquals("c19", reports.coverage.get(19).getId());
    }

    @Test
    public void anInactiveOrNullGateWritesNoGateStatus() {
        BuildCoverageReport nullGate = readCoverage(coverage("a", List.of(value(FakeMetric.LINE, 1, 2))));
        BuildCoverageReport inactive = readCoverage(new FakeCoverageAction("b", "B",
                List.of(value(FakeMetric.LINE, 1, 2)), Collections.emptyList(),
                new QualityGateResult(QualityGateStatus.INACTIVE)));

        assertEquals("", nullGate.getGateStatus());
        assertEquals("", inactive.getGateStatus());
        assertFalse(inactive.toJson().has("gate_status"));
    }

    @Test
    public void warningsWithAReferenceCarryNewAndFixed() {
        BuildWarningsReport report = VarroaRunQuality.readWarnings(
                warnings("checkstyle", new FakeResult(true, true, new QualityGateResult(QualityGateStatus.NOTE))));

        assertEquals("checkstyle", report.getId());
        assertEquals(3, report.getTotal());
        assertEquals(1, report.getError());
        assertEquals(0, report.getHigh());
        assertEquals(1, report.getNormal());
        assertEquals(1, report.getLow());
        assertTrue(report.hasReference());
        assertEquals(2, report.getNewTotal());
        assertEquals(1, report.getFixedTotal());
        assertEquals("NOTE", report.getGateStatus());
        assertTrue(report.toJson().getBoolean("has_reference"));
        assertEquals(2, report.toJson().getInt("new_total"));
    }

    @Test
    public void warningsWithoutAReferenceWriteNoNewOrFixedKey() {
        BuildWarningsReport report = VarroaRunQuality.readWarnings(
                warnings("checkstyle", new FakeResult(true, false, null)));

        assertFalse(report.hasReference());
        assertFalse(report.toJson().has("new_total"));
        assertFalse(report.toJson().has("fixed_total"));
        assertFalse(report.toJson().has("gate_status"));
    }

    @Test
    public void aResultMissingASizeGetterSkipsTheReport() {
        VarroaRunQuality.Reports reports = VarroaRunQuality.collect(Collections.emptyList(),
                List.of(warnings("checkstyle", new SizelessResult())));

        assertTrue(reports.warnings.isEmpty());
    }

    @Test
    public void readMatchesExactClassNamesOnly() {
        hudson.model.Action other = new hudson.model.InvisibleAction() {
        };
        hudson.model.Action second = new hudson.model.InvisibleAction() {
        };
        List<hudson.model.Action> actions = List.of(other, second);

        assertTrue(VarroaRunRecorder.findActionsByClassName(actions, VarroaRunQuality.COVERAGE_ACTION).isEmpty());
        assertTrue(VarroaRunRecorder.findActionsByClassName(actions, VarroaRunQuality.WARNINGS_ACTION).isEmpty());
        List<hudson.model.Action> found =
                VarroaRunRecorder.findActionsByClassName(actions, other.getClass().getName());
        assertEquals(1, found.size());
        assertTrue(found.get(0) == other);
    }
}
