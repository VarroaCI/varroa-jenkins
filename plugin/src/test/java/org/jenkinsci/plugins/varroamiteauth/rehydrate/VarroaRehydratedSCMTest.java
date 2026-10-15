package org.jenkinsci.plugins.varroamiteauth.rehydrate;

import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import org.junit.Test;

import hudson.scm.ChangeLogParser;

/**
 * Tests for {@link VarroaRehydratedSCM}.
 */
public class VarroaRehydratedSCMTest {

    @Test
    public void createChangeLogParserAnswersTheVarroaParser() {
        ChangeLogParser parser = new VarroaRehydratedSCM().createChangeLogParser();
        assertTrue(parser instanceof VarroaChangeLogParser);
    }

    @Test
    public void checkoutThrows() {
        try {
            new VarroaRehydratedSCM().checkout(null, null, null, null, null, null);
            fail("checkout must throw: a rehydrated run is already terminal");
        } catch (Exception expected) {
            // expected: a terminal run never checks out
        }
    }
}
