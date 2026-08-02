package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.security.ACL;
import hudson.security.ACLContext;
import jenkins.model.Jenkins;

import java.io.StringWriter;
import java.util.List;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

import static org.mockito.Mockito.*;

/**
 * Tests for {@link ActivityEndpoint}.
 */
public class ActivityEndpointTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    /** The authority the mite holds. */
    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    @Test
    public void authorizedMiteDrainsEvents() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();
        sink.offer(event("e1"));
        sink.offer(event("e2"));

        // Act as the mite: ACL.as2 with ROLE:varroa:system-mite authority
        // (Coordinator §9.2 pattern).
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");
            when(req.getParameter("max")).thenReturn(null);

            // Capture the writer output.
            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));

            endpoint.doEvents(req, rsp);

            // Verify 200.
            verify(rsp).setContentType("application/json");
            verify(rsp).setStatus(200);

            // Parse JSON response.
            String json = writer.toString();
            assertTrue(json.contains("\"events\""));
            assertTrue(json.contains("\"e1\""));
            assertTrue(json.contains("\"e2\""));
            assertTrue(json.contains("\"dropped\""));

            // Verify events were drained.
            assertEquals(0, sink.queueSize());
        }
    }

    @Test
    public void emptyBufferReturnsEmptyEventsAndZeroDropped() throws Exception {
        ActivitySink.get().resetForTesting();

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");
            when(req.getParameter("max")).thenReturn(null);

            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));

            endpoint.doEvents(req, rsp);

            String json = writer.toString();
            assertTrue(json.contains("\"events\":[]"));
            assertTrue(json.contains("\"dropped\":0"));
        }
    }

    @Test
    public void nonMiteCallerGets403() throws Exception {
        ActivitySink.get().resetForTesting();

        // Authenticated but WITHOUT the mite authority.
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");

            endpoint.doEvents(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void postMethodReturns405() throws Exception {
        ActivitySink.get().resetForTesting();

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("POST");

            endpoint.doEvents(req, rsp);

            verify(rsp).sendError(405, "Method Not Allowed: only GET is served");
        }
    }

    @Test
    public void maxClampingIsHonored() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Add 10 events.
        for (int i = 0; i < 10; i++) {
            sink.offer(event("e" + i));
        }

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");
            when(req.getParameter("max")).thenReturn("3"); // request max=3

            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));

            endpoint.doEvents(req, rsp);

            String json = writer.toString();
            // Should only have 3 events.
            assertTrue(json.contains("\"e0\""));
            assertTrue(json.contains("\"e2\""));
            assertFalse(json.contains("\"e3\""));

            // 7 remain in buffer.
            assertEquals(7, sink.queueSize());
        }
    }

    @Test
    public void droppedCounterInDrainResponse() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Overflow to produce drops: capacity is 1024 for the singleton,
        // so we need >1024 events.
        int cap = 1024;
        for (int i = 0; i < cap + 5; i++) {
            sink.offer(event("e" + i));
        }

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");
            when(req.getParameter("max")).thenReturn(String.valueOf(cap));

            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));

            endpoint.doEvents(req, rsp);

            String json = writer.toString();
            // dropped should be >= 5 (the overflow count).
            assertTrue("dropped should be >= 5, json: " + json,
                    json.contains("\"dropped\":5") || json.contains("\"dropped\":6")
                    || json.contains("\"dropped\":7") || json.contains("\"dropped\":8")
                    || json.contains("\"dropped\":9"));

            // Second drain: dropped should be 0.
            writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));
            endpoint.doEvents(req, rsp);
            json = writer.toString();
            assertTrue("second drain dropped should be 0, json: " + json,
                    json.contains("\"dropped\":0"));
        }
    }

    // ---- Idle gauge tests ----

    @Test
    public void drainResponseIncludesIdleObject() throws Exception {
        ActivitySink.get().resetForTesting();
        HttpActivityFilter.resetForTesting();
        ActivityEndpoint.resetTimerTriggerCache();

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");
            when(req.getParameter("max")).thenReturn(null);

            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new java.io.PrintWriter(writer));

            endpoint.doEvents(req, rsp);

            String json = writer.toString();
            // The idle object should be present with all five gauge fields.
            assertTrue("response should contain idle object: " + json,
                    json.contains("\"idle\":{"));
            assertTrue("response should contain last_http_activity_unix: " + json,
                    json.contains("\"last_http_activity_unix\""));
            assertTrue("response should contain last_event_unix: " + json,
                    json.contains("\"last_event_unix\""));
            assertTrue("response should contain running_builds: " + json,
                    json.contains("\"running_builds\""));
            assertTrue("response should contain queue_length: " + json,
                    json.contains("\"queue_length\""));
            assertTrue("response should contain timer_trigger_jobs: " + json,
                    json.contains("\"timer_trigger_jobs\""));
        }
    }

    @Test
    public void last_event_unixSurvivesDrains() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Offer an event — this sets last_event_unix to a non-zero value.
        sink.offer(event("e1"));
        long afterFirstOffer = sink.getLastEventUnix();
        assertTrue("last_event_unix should be > 0 after offer", afterFirstOffer > 0);

        // Drain the sink.
        ActivitySink.DrainResult r1 = sink.drain(100);
        assertEquals(1, r1.events.size());

        // After drain, last_event_unix should still be the same value (survives drain).
        assertEquals("last_event_unix should survive drain",
                afterFirstOffer, sink.getLastEventUnix());

        // Second drain with no new events — last_event_unix unchanged.
        ActivitySink.DrainResult r2 = sink.drain(100);
        assertEquals(0, r2.events.size());
        assertEquals("last_event_unix should survive second drain with no events",
                afterFirstOffer, sink.getLastEventUnix());
    }

    @Test
    public void idleGaugesReflectCurrentState() throws Exception {
        ActivitySink.get().resetForTesting();
        HttpActivityFilter.resetForTesting();
        ActivityEndpoint.resetTimerTriggerCache();

        // Offer an event to set last_event_unix.
        ActivitySink.get().offer(event("test-event"));

        // The idle object should show last_event_unix > 0.
        net.sf.json.JSONObject idle = ActivityEndpoint.buildIdleObject();

        assertTrue("last_event_unix should be > 0 after event offered",
                idle.getLong("last_event_unix") > 0);
        assertTrue("last_http_activity_unix should be >= 0",
                idle.getLong("last_http_activity_unix") >= 0);
        assertTrue("running_builds should be >= 0",
                idle.getInt("running_builds") >= 0);
        assertTrue("queue_length should be >= 0",
                idle.getInt("queue_length") >= 0);
        assertTrue("timer_trigger_jobs should be >= 0",
                idle.getInt("timer_trigger_jobs") >= 0);
    }

    private static ActivityEvent event(String msg) {
        return ActivityEvent.builder().message(msg).type("test").build();
    }
}
