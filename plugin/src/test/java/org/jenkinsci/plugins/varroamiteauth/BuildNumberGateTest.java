package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.BooleanSupplier;

import org.junit.Test;

/**
 * Tests for {@link BuildNumberGate}: one apply of a job at a time, the
 * order of the waiting applies, the release, the drop of a waiting apply,
 * and the timeout.
 */
public class BuildNumberGateTest {

    private static void await(BooleanSupplier condition, String message) throws Exception {
        long deadline = System.currentTimeMillis() + 5_000L;
        while (System.currentTimeMillis() < deadline) {
            if (condition.getAsBoolean()) {
                return;
            }
            Thread.sleep(10L);
        }
        fail("timed out waiting for: " + message);
    }

    @Test
    public void aSecondApplyWaitsUntilTheFirstRunStarts() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> applied.add("run-a"));
        gate.submit("job", "run-b", () -> applied.add("run-b"));
        await(() -> applied.size() == 1, "the first apply");
        Thread.sleep(100L);
        assertEquals(List.of("run-a"), applied);
        assertEquals("run-a", gate.holderForTesting("job"));

        gate.release("job", "run-a");
        await(() -> applied.size() == 2, "the second apply");
        assertEquals(List.of("run-a", "run-b"), applied);
        assertEquals("run-b", gate.holderForTesting("job"));
    }

    @Test
    public void anApplyThatKeepsNothingOpensTheGateAtOnce() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> {
            applied.add("run-a");
            return false;
        });
        gate.submit("job", "run-b", () -> applied.add("run-b"));
        await(() -> applied.size() == 2, "both applies");
        assertEquals(List.of("run-a", "run-b"), applied);
    }

    @Test
    public void anApplyThatKeepsNothingKeepsANewApplyOfTheSameRun() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> {
            applied.add("first");
            // A new claim of the same run submits again before the gate opens.
            gate.submit("job", "run-a", () -> applied.add("second"));
            return false;
        });
        await(() -> applied.size() == 2, "the second apply of the same run");
        assertEquals(List.of("first", "second"), applied);
    }

    @Test
    public void runWhileHoldingRunsOnlyForTheHolder() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> ran = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> true);
        await(() -> "run-a".equals(gate.holderForTesting("job")), "run-a to hold the gate");

        assertTrue(gate.runWhileHolding("job", "run-a", () -> ran.add("holder")));
        assertFalse(gate.runWhileHolding("job", "run-b", () -> ran.add("other")));
        gate.release("job", "run-a");
        assertFalse(gate.runWhileHolding("job", "run-a", () -> ran.add("expired")));
        assertEquals(List.of("holder"), ran);
    }

    @Test
    public void reopenKeepsANewApplyOfTheSameRun() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> applied.add("first"));
        await(() -> applied.size() == 1, "the first apply");
        // A retry of the same run waits behind its own holder.
        gate.submit("job", "run-a", () -> applied.add("second"));
        gate.reopen("job", "run-a");
        await(() -> applied.size() == 2, "the retry after the reopen");
        assertEquals(List.of("first", "second"), applied);
    }

    @Test
    public void twoJobsDoNotWaitForEachOther() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job-1", "run-a", () -> applied.add("run-a"));
        gate.submit("job-2", "run-b", () -> applied.add("run-b"));
        await(() -> applied.size() == 2, "both applies");
    }

    @Test
    public void aReleaseDropsAWaitingApply() throws Exception {
        BuildNumberGate gate = new BuildNumberGate(System::currentTimeMillis, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> applied.add("run-a"));
        gate.submit("job", "run-b", () -> applied.add("run-b"));
        await(() -> applied.size() == 1, "the first apply");
        gate.release("job", "run-b");
        gate.release("job", "run-a");
        Thread.sleep(100L);
        assertEquals(List.of("run-a"), applied);
        assertNull(gate.holderForTesting("job"));
    }

    @Test
    public void aHolderPastItsTimeOpensTheGate() throws Exception {
        AtomicLong now = new AtomicLong(1_000_000L);
        BuildNumberGate gate = new BuildNumberGate(now::get, false);
        List<String> applied = new CopyOnWriteArrayList<>();

        gate.submit("job", "run-a", () -> applied.add("run-a"));
        gate.submit("job", "run-b", () -> applied.add("run-b"));
        await(() -> applied.size() == 1, "the first apply");

        now.addAndGet(BuildNumberGate.HOLD_MILLIS - 1);
        gate.expire();
        assertEquals("run-a", gate.holderForTesting("job"));

        now.addAndGet(1);
        gate.expire();
        await(() -> applied.size() == 2, "the apply after the timeout");
        assertEquals("run-b", gate.holderForTesting("job"));
    }
}
