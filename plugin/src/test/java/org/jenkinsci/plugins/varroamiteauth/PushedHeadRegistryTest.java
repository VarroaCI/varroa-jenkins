package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.io.IOException;
import jenkins.scm.api.SCMEvent;
import jenkins.scm.impl.mock.MockSCMController;
import jenkins.scm.impl.mock.MockSCMHead;
import jenkins.scm.impl.mock.MockSCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMRevision;
import jenkins.scm.impl.mock.MockSCMSource;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

public class PushedHeadRegistryTest {

    private MockSCMController controller;
    private MockSCMSource source;

    @Before
    public void setUp() throws IOException {
        controller = MockSCMController.create();
        controller.createRepository("repo");
        source = new MockSCMSource(controller, "repo");
    }

    @After
    public void tearDown() throws IOException {
        controller.close();
    }

    private MockSCMHeadEvent event(String head, String hash) {
        return new MockSCMHeadEvent(SCMEvent.Type.CREATED, controller, "repo", head, hash);
    }

    private static MockSCMRevision revision(String head, String hash) {
        return new MockSCMRevision(new MockSCMHead(head), hash);
    }

    @Test
    public void consumeMatchesRecordedHeadOnce() {
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> 0L);
        registry.record(event("feature", "h1"));
        MockSCMHead head = new MockSCMHead("feature");

        assertTrue(registry.consume(source, head, revision("feature", "h1"), "owner"));
        assertFalse(registry.consume(source, head, revision("feature", "h1"), "owner"));
    }

    @Test
    public void consumeRefusesOtherHeadName() {
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> 0L);
        registry.record(event("feature", "h1"));

        assertFalse(registry.consume(source, new MockSCMHead("other"), revision("other", "h1"), "owner"));
    }

    @Test
    public void consumeRefusesOtherRevision() {
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> 0L);
        registry.record(event("feature", "h1"));

        assertFalse(registry.consume(source, new MockSCMHead("feature"), revision("feature", "h2"), "owner"));
    }

    @Test
    public void entryExpiresAfterTtl() {
        long[] now = {0L};
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> now[0]);
        registry.record(event("feature", "h1"));
        now[0] = PushedHeadRegistry.TTL_MILLIS;

        assertFalse(registry.consume(source, new MockSCMHead("feature"), revision("feature", "h1"), "owner"));
    }

    @Test
    public void capacityEvictsOldest() {
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> 0L);
        for (int i = 0; i <= PushedHeadRegistry.CAPACITY; i++) {
            registry.record(event("b" + i, "h"));
        }

        assertTrue(registry.sizeForTesting() == PushedHeadRegistry.CAPACITY);
        assertFalse(registry.consume(source, new MockSCMHead("b0"), revision("b0", "h"), "owner"));
        assertTrue(registry.consume(source, new MockSCMHead("b" + PushedHeadRegistry.CAPACITY),
                revision("b" + PushedHeadRegistry.CAPACITY, "h"), "owner"));
    }

    @Test
    public void differentOwnersEachConsumeOnce() {
        PushedHeadRegistry registry = new PushedHeadRegistry(() -> 0L);
        registry.record(event("feature", "h1"));
        MockSCMHead head = new MockSCMHead("feature");
        MockSCMRevision rev = revision("feature", "h1");

        assertTrue(registry.consume(source, head, rev, "owner-a"));
        assertTrue(registry.consume(source, head, rev, "owner-b"));
        assertFalse(registry.consume(source, head, rev, "owner-a"));
    }
}
