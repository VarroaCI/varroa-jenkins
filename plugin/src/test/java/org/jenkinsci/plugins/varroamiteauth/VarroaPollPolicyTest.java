package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import java.util.Random;

import org.junit.Test;

/** Tests for {@link VarroaPollPolicy}'s cadence and jitter (D8, D11). */
public class VarroaPollPolicyTest {

    @Test
    public void baseIntervalIsTwoSecondsForTheFirstThirtySeconds() {
        assertEquals(2_000L, VarroaPollPolicy.baseIntervalMillis(0));
        assertEquals(2_000L, VarroaPollPolicy.baseIntervalMillis(29_999L));
    }

    @Test
    public void baseIntervalIsFiveSecondsUntilFiveMinutes() {
        assertEquals(5_000L, VarroaPollPolicy.baseIntervalMillis(30_000L));
        assertEquals(5_000L, VarroaPollPolicy.baseIntervalMillis(5L * 60 * 1000 - 1));
    }

    @Test
    public void baseIntervalIsFifteenSecondsAfterFiveMinutes() {
        assertEquals(15_000L, VarroaPollPolicy.baseIntervalMillis(5L * 60 * 1000));
        assertEquals(15_000L, VarroaPollPolicy.baseIntervalMillis(24L * 60 * 60 * 1000));
    }

    @Test
    public void jitterStaysWithinPlusOrMinusTwentyPercent() {
        Random random = new Random(42);
        for (int i = 0; i < 10_000; i++) {
            long jittered = VarroaPollPolicy.withJitter(1000L, random);
            assertTrue("jittered value " + jittered + " below 800", jittered >= 800L);
            assertTrue("jittered value " + jittered + " above 1200", jittered <= 1200L);
        }
    }

    @Test
    public void jitterVariesAcrossCalls() {
        Random random = new Random(7);
        long first = VarroaPollPolicy.withJitter(1000L, random);
        boolean sawDifferent = false;
        for (int i = 0; i < 20; i++) {
            if (VarroaPollPolicy.withJitter(1000L, random) != first) {
                sawDifferent = true;
                break;
            }
        }
        assertTrue("jitter must vary across calls", sawDifferent);
    }
}
