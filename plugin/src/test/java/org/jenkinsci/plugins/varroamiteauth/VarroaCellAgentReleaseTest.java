package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import hudson.model.TaskListener;
import hudson.slaves.AbstractCloudComputer;
import hudson.slaves.AbstractCloudSlave;
import hudson.slaves.JNLPLauncher;

import java.io.IOException;
import java.time.Duration;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

public class VarroaCellAgentReleaseTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final Set<String> TERMINATED = ConcurrentHashMap.newKeySet();

    /** A cloud agent that records its termination instead of touching a cloud. */
    static final class TestCloudAgent extends AbstractCloudSlave {

        private static final long serialVersionUID = 1L;

        TestCloudAgent(String name) throws Exception {
            super(name, "/tmp/" + name, new JNLPLauncher());
        }

        @Override
        public AbstractCloudComputer<TestCloudAgent> createComputer() {
            return new AbstractCloudComputer<>(this);
        }

        @Override
        protected void _terminate(TaskListener listener) throws IOException, InterruptedException {
            if (getNodeName().startsWith("boom")) {
                throw new IOException("boom");
            }
            TERMINATED.add(getNodeName());
        }
    }

    @Before
    public void cell() {
        VarroaCellAgentRelease.setCellForTest(true);
    }

    @After
    public void resetCell() {
        VarroaCellAgentRelease.resetCellForTest();
        TERMINATED.clear();
    }

    @Test
    public void releasesIdleCloudAgentOnCell() throws Exception {
        j.jenkins.addNode(new TestCloudAgent("a1"));

        int n = VarroaCellAgentRelease.release(j.jenkins, c -> true, Duration.ofSeconds(5));

        assertEquals(1, n);
        assertTrue(TERMINATED.contains("a1"));
        assertNull(j.jenkins.getNode("a1"));
    }

    @Test
    public void keepsBusyCloudAgent() throws Exception {
        j.jenkins.addNode(new TestCloudAgent("a1"));
        j.jenkins.addNode(new TestCloudAgent("a2"));

        VarroaCellAgentRelease.release(j.jenkins, c -> !"a2".equals(c.getName()), Duration.ofSeconds(5));

        assertTrue(TERMINATED.contains("a1"));
        assertFalse(TERMINATED.contains("a2"));
        assertNotNull(j.jenkins.getNode("a2"));
    }

    @Test
    public void neverTouchesPermanentAgent() throws Exception {
        j.createSlave("perm", null, null);

        int n = VarroaCellAgentRelease.release(j.jenkins, c -> true, Duration.ofSeconds(5));

        assertEquals(0, n);
        assertNotNull(j.jenkins.getNode("perm"));
    }

    @Test
    public void terminatorDoesNothingOffCell() throws Exception {
        VarroaCellAgentRelease.setCellForTest(false);
        j.jenkins.addNode(new TestCloudAgent("a1"));

        VarroaCellAgentRelease.releaseIdleAgentsAtStop();

        assertFalse(TERMINATED.contains("a1"));
        assertNotNull(j.jenkins.getNode("a1"));
    }

    @Test
    public void terminatorReleasesOnCell() throws Exception {
        j.jenkins.addNode(new TestCloudAgent("a1"));

        VarroaCellAgentRelease.releaseIdleAgentsAtStop();

        assertTrue(TERMINATED.contains("a1"));
    }

    @Test
    public void oneFailureDoesNotStopTheOthers() throws Exception {
        j.jenkins.addNode(new TestCloudAgent("boom1"));
        j.jenkins.addNode(new TestCloudAgent("a1"));

        int n = VarroaCellAgentRelease.release(j.jenkins, c -> true, Duration.ofSeconds(5));

        assertTrue(TERMINATED.contains("a1"));
        assertEquals(1, n);
    }
}
