package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.FreeStyleProject;
import hudson.model.Queue;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaIntentAction} as a {@link Queue.QueueAction},
 * asserted against the real Jenkins queue.
 */
public class VarroaIntentActionQueueTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    /** A long quiet period keeps the item in the queue so the test can count it. */
    private static final int QUIET_PERIOD_SECONDS = 300;

    private static VarroaIntentAction intent(String runId) {
        return new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
    }

    @Test
    public void twoDifferentRunIdsProduceTwoQueueItems() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        Queue.getInstance().schedule2(project, QUIET_PERIOD_SECONDS, intent("run-a"));
        Queue.getInstance().schedule2(project, QUIET_PERIOD_SECONDS, intent("run-b"));

        assertEquals(2, countItemsFor(project));
    }

    @Test
    public void theSameRunIdRepeatedProducesOneQueueItem() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        Queue.getInstance().schedule2(project, QUIET_PERIOD_SECONDS, intent("run-c"));
        Queue.getInstance().schedule2(project, QUIET_PERIOD_SECONDS, intent("run-c"));

        assertEquals(1, countItemsFor(project));
    }

    private static int countItemsFor(FreeStyleProject project) {
        int count = 0;
        for (Queue.Item item : Queue.getInstance().getItems()) {
            if (item.task == project) {
                count++;
            }
        }
        return count;
    }
}
