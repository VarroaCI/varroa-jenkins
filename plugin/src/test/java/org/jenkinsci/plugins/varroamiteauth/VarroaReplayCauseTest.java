package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import org.junit.Test;

/**
 * Tests for {@link VarroaReplayCause}: the short description names the
 * source run when the cause holds one, and names no run when it is empty.
 */
public class VarroaReplayCauseTest {

    @Test
    public void shortDescriptionNamesTheSourceRun() {
        assertEquals("Replayed from run src-run-1 through Varroa",
                new VarroaReplayCause("src-run-1").getShortDescription());
    }

    @Test
    public void shortDescriptionOfAnEmptySourceNamesNoRun() {
        assertEquals("Replayed through Varroa", new VarroaReplayCause("").getShortDescription());
        assertEquals("Replayed through Varroa", new VarroaReplayCause(null).getShortDescription());
    }
}
