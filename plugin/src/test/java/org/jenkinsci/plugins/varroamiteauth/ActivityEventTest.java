package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.time.Instant;
import org.junit.Test;

/**
 * Tests for {@link ActivityEvent}.
 */
public class ActivityEventTest {

    @Test
    public void defaultFieldsAreEmpty() {
        ActivityEvent event = ActivityEvent.builder()
                .type("test.type")
                .message("hello")
                .build();

        assertEquals("test.type", event.getType());
        assertEquals("hello", event.getMessage());
        assertEquals("jenkins", event.getSource());
        assertEquals("", event.getActor());
        assertEquals("", event.getItemPath());
        assertEquals(0, event.getBuildNumber());
        assertEquals("", event.getResult());
        assertEquals("", event.getUrl());
        assertEquals("", event.getController());
        assertEquals("", event.getNamespace());
        assertEquals("", event.getPhase());
        assertEquals("", event.getReason());
        assertNotNull(event.getTimestamp());
    }

    @Test
    public void explicitFieldsArePreserved() {
        ActivityEvent event = ActivityEvent.builder()
                .type("build.completed")
                .actor("alice")
                .itemPath("my-folder/my-job")
                .buildNumber(42)
                .result("SUCCESS")
                .url("job/my-folder/my-job/42/")
                .message("Build #42 completed: SUCCESS")
                .timestamp("2025-01-15T14:30:00Z")
                .build();

        assertEquals("2025-01-15T14:30:00Z", event.getTimestamp());
        assertEquals("build.completed", event.getType());
        assertEquals("alice", event.getActor());
        assertEquals("my-folder/my-job", event.getItemPath());
        assertEquals(42, event.getBuildNumber());
        assertEquals("SUCCESS", event.getResult());
        assertEquals("job/my-folder/my-job/42/", event.getUrl());
        assertEquals("Build #42 completed: SUCCESS", event.getMessage());
    }

    @Test
    public void nowRfc3339ProducesValidInstant() {
        String rfc = ActivityEvent.nowRfc3339();
        assertNotNull(rfc);
        assertTrue("must end with Z: " + rfc, rfc.endsWith("Z"));

        // Must parse as a valid ISO instant.
        Instant parsed = Instant.parse(rfc);
        assertNotNull(parsed);
    }

    @Test
    public void sourceIsAlwaysJenkins() {
        ActivityEvent event = ActivityEvent.builder().type("test").message("x").build();
        assertEquals("jenkins", event.getSource());

        event = ActivityEvent.builder().type("build.started").message("y").build();
        assertEquals("jenkins", event.getSource());
    }
}
