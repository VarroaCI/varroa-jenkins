package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.Action;

import java.util.List;
import org.junit.Test;

/**
 * Tests for {@link VarroaRunRecorder}'s reflective test-counter read (D3)
 * — plain JUnit, no JenkinsRule. {@code
 * hudson.tasks.test.AbstractTestResultAction} ships in the {@code junit}
 * plugin, which this plugin does not compile against, so the read matches
 * by simple class name instead.
 */
public class TestCounterTest {

    /** Test double matched by simple name, not by type. */
    public static class AbstractTestResultAction implements Action {
        private final int total;
        private final int failed;
        private final int skipped;

        AbstractTestResultAction(int total, int failed, int skipped) {
            this.total = total;
            this.failed = failed;
            this.skipped = skipped;
        }

        public int getTotalCount() {
            return total;
        }

        public int getFailCount() {
            return failed;
        }

        public int getSkipCount() {
            return skipped;
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

    @Test
    public void zeroCountersWithNoTestResultAction() {
        int[] counts = VarroaRunRecorder.readTestCounters(List.of());
        assertArrayEquals(new int[] {0, 0, 0, 0}, counts);
    }

    @Test
    public void countersFromAStubAction() {
        Action stub = new AbstractTestResultAction(10, 2, 1);
        int[] counts = VarroaRunRecorder.readTestCounters(List.of(stub));
        // total, passed, failed, skipped
        assertArrayEquals(new int[] {10, 7, 2, 1}, counts);
    }
}
