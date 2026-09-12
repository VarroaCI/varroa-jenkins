package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.util.XStream2;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaCallSeqAction} (task 9.7): the {@code s<n>}
 * sequence and its survival across a reload.
 */
public class VarroaCallSeqActionTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void twoCallsInOneRunSendS1AndS2() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        FreeStyleBuild build = j.buildAndAssertSuccess(project);

        assertEquals(1, VarroaCallSeqAction.nextSeq(build));
        assertEquals(2, VarroaCallSeqAction.nextSeq(build));
    }

    @Test
    public void counterSurvivesAReload() {
        VarroaCallSeqAction action = new VarroaCallSeqAction();
        action.increment();
        action.increment();

        XStream2 xstream = new XStream2();
        String xml = xstream.toXML(action);
        VarroaCallSeqAction reloaded = (VarroaCallSeqAction) xstream.fromXML(xml);

        assertEquals(2, reloaded.lastSeqForTesting());
        assertEquals(3, reloaded.increment());
    }
}
