package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.FreeStyleProject;
import hudson.model.Item;
import hudson.model.TopLevelItem;
import hudson.security.ACL;
import jenkins.model.Jenkins;

import java.util.List;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link ActivityItemListener}.
 */
public class ActivityItemListenerTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void createJobEmitsItemCreated() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Creating a job via JenkinsRule triggers onCreated.
        FreeStyleProject project = j.createFreeStyleProject("build-job");

        ActivitySink.DrainResult result = sink.drain(100);
        assertTrue("should have at least 1 event", result.events.size() >= 1);

        // Find the item.created event (there may be config.changed or other events too).
        ActivityEvent created = findEvent(result.events, "item.created");
        assertNotNull("item.created event should exist", created);
        assertEquals("item.created", created.getType());
        assertEquals("build-job", created.getItemPath());
    }

    @Test
    public void updateJobEmitsItemUpdated() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("update-job");
        sink.resetForTesting(); // clear creation events

        // Updating the config triggers onUpdated.
        project.setDescription("updated description");

        // Force a save to ensure onUpdated fires.
        project.save();

        ActivitySink.DrainResult result = sink.drain(100);
        ActivityEvent updated = findEvent(result.events, "item.updated");
        assertNotNull("item.updated event should exist", updated);
        assertEquals("item.updated", updated.getType());
        assertEquals("update-job", updated.getItemPath());
    }

    @Test
    public void renameJobEmitsItemMoved() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("old-name");
        sink.resetForTesting();

        // Rename the job.
        project.renameTo("new-name");

        ActivitySink.DrainResult result = sink.drain(100);
        ActivityEvent moved = findEvent(result.events, "item.moved");
        assertNotNull("item.moved event should exist", moved);
        assertEquals("item.moved", moved.getType());
        assertEquals("new-name", moved.getItemPath());
        assertTrue(moved.getMessage().contains("old-name"));
        assertTrue(moved.getMessage().contains("new-name"));

        // Assert no duplicate item.moved (only one).
        long movedCount = result.events.stream()
                .filter(e -> "item.moved".equals(e.getType()))
                .count();
        assertEquals("should be exactly one item.moved", 1, movedCount);
    }

    @Test
    public void deleteJobEmitsItemDeleted() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("delete-me");
        sink.resetForTesting();

        project.delete();

        ActivitySink.DrainResult result = sink.drain(100);
        ActivityEvent deleted = findEvent(result.events, "item.deleted");
        assertNotNull("item.deleted event should exist", deleted);
        assertEquals("item.deleted", deleted.getType());
        assertEquals("delete-me", deleted.getItemPath());
    }

    @Test
    public void systemOrAnonymousActorIsEmpty() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Fire an item event while impersonating the SYSTEM user.
        ACL.impersonate2(ACL.SYSTEM2);
        try {
            FreeStyleProject project = j.createFreeStyleProject("system-test");
            sink.resetForTesting();

            // Modify to trigger onUpdated under SYSTEM auth.
            project.setDescription("system change");
            project.save();
        } finally {
            // Restore the context (not strictly needed, the rule resets it).
        }

        ActivitySink.DrainResult result = sink.drain(100);
        for (ActivityEvent e : result.events) {
            assertEquals("actor should be empty for SYSTEM principal", "", e.getActor());
        }
    }

    // --- Helpers ---

    private static ActivityEvent findEvent(List<ActivityEvent> events, String type) {
        for (ActivityEvent e : events) {
            if (type.equals(e.getType())) {
                return e;
            }
        }
        return null;
    }
}
