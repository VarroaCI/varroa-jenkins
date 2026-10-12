package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotEquals;

import org.junit.Test;

/**
 * Tests for {@link VarroaRestartCause}: the short description names the
 * source run and the stage, and equality covers both values.
 */
public class VarroaRestartCauseTest {

    @Test
    public void shortDescriptionNamesTheSourceRunAndStage() {
        assertEquals("Restarted from run src-run-1 at stage deploy through Varroa",
                new VarroaRestartCause("src-run-1", "deploy").getShortDescription());
    }

    @Test
    public void shortDescriptionOfAnEmptySourceNamesNoRun() {
        assertEquals("Restarted through Varroa", new VarroaRestartCause("", "").getShortDescription());
        assertEquals("Restarted through Varroa", new VarroaRestartCause(null, null).getShortDescription());
    }

    @Test
    public void causesWithTheSameRunAndStageAreEqual() {
        assertEquals(new VarroaRestartCause("src-run-1", "deploy"), new VarroaRestartCause("src-run-1", "deploy"));
        assertEquals(new VarroaRestartCause("src-run-1", "deploy").hashCode(),
                new VarroaRestartCause("src-run-1", "deploy").hashCode());
    }

    @Test
    public void causesWithAnotherStageAreNotEqual() {
        assertNotEquals(new VarroaRestartCause("src-run-1", "deploy"), new VarroaRestartCause("src-run-1", "build"));
    }
}
