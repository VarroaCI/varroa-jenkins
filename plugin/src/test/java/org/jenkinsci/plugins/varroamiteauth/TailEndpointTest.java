package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.PrintWriter;
import java.io.StringReader;
import java.io.StringWriter;
import java.util.List;
import java.util.concurrent.AbstractExecutorService;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;

import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

import net.sf.json.JSONObject;

/**
 * Tests for {@link TailEndpoint} (task 6.9): the authorised call, the
 * unauthorised call, and the lease expiry that restores the slow interval.
 * Also covers {@code doCounters} (D2, D11): the authorised counter
 * snapshot, the unauthorised refusal, and the stuck-queue observer's
 * increment reaching the same response.
 */
public class TailEndpointTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    @Before
    public void resetCounters() {
        PluginCounters.resetForTesting();
        VarroaRunRecorder.resetOversizedDroppedForTesting();
        LogChunkBuffer.resetStuckQueueWarningsForTesting();
    }

    private static StaplerRequest2 requestWithMethod(String method) {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn(method);
        return req;
    }

    private static StaplerRequest2 requestWithBody(String method, String json) throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn(method);
        when(req.getReader()).thenReturn(new BufferedReader(new StringReader(json)));
        return req;
    }

    private static StaplerResponse2 responseCapturing(StringWriter writer) throws Exception {
        StaplerResponse2 rsp = mock(StaplerResponse2.class);
        when(rsp.getWriter()).thenReturn(new PrintWriter(writer));
        return rsp;
    }

    @Test
    public void authorizedStartRecordsALease() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StaplerRequest2 req = requestWithBody("POST", "{\"runId\":\"run-1\",\"action\":\"start\",\"leaseSeconds\":90}");
            StringWriter writer = new StringWriter();
            StaplerResponse2 rsp = responseCapturing(writer);

            endpoint.doTail(req, rsp);

            verify(rsp).setContentType("application/json");
            verify(rsp).setStatus(200);
            assertTrue(writer.toString().contains("\"ok\""));
            assertTrue(endpoint.hasLeaseForTesting("run-1"));
        }
    }

    @Test
    public void aStopClearsTheLease() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            endpoint.doTail(requestWithBody("POST", "{\"runId\":\"run-2\",\"action\":\"start\",\"leaseSeconds\":90}"),
                    responseCapturing(new StringWriter()));
            assertTrue(endpoint.hasLeaseForTesting("run-2"));

            endpoint.doTail(requestWithBody("POST", "{\"runId\":\"run-2\",\"action\":\"stop\"}"),
                    responseCapturing(new StringWriter()));
            assertFalse(endpoint.hasLeaseForTesting("run-2"));
        }
    }

    @Test
    public void unauthorizedCallerGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StaplerRequest2 req = requestWithBody("POST", "{\"runId\":\"run-3\",\"action\":\"start\",\"leaseSeconds\":90}");
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            endpoint.doTail(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
            assertFalse(endpoint.hasLeaseForTesting("run-3"));
        }
    }

    @Test
    public void aNonPostMethodGets405() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            when(req.getMethod()).thenReturn("GET");
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            endpoint.doTail(req, rsp);

            verify(rsp).sendError(405, "Method Not Allowed: only POST is served");
        }
    }

    @Test
    public void aMissingRunIdGets400() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StaplerRequest2 req = requestWithBody("POST", "{\"action\":\"start\"}");
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            endpoint.doTail(req, rsp);

            verify(rsp).sendError(400, "Bad Request: runId and a start/stop action are required");
        }
    }

    @Test
    public void leaseExpiryRestoresTheSlowInterval() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            endpoint.doTail(requestWithBody("POST", "{\"runId\":\"run-4\",\"action\":\"start\",\"leaseSeconds\":1}"),
                    responseCapturing(new StringWriter()));
            assertTrue(endpoint.hasLeaseForTesting("run-4"));

            endpoint.tickForTesting(System.currentTimeMillis() + 2000);

            assertFalse("an expired lease must be cleared", endpoint.hasLeaseForTesting("run-4"));
        }
    }

    @Test
    public void authorizedCountersCallReturnsEveryCounter() throws Exception {
        PluginCounters.increment(PluginCounters.SPOOL_DROPPED);
        PluginCounters.increment(PluginCounters.SPOOL_DROPPED);
        PluginCounters.increment(PluginCounters.EVENTS_DROPPED);

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StringWriter writer = new StringWriter();
            StaplerResponse2 rsp = responseCapturing(writer);

            endpoint.doCounters(requestWithMethod("GET"), rsp);

            verify(rsp).setContentType("application/json");
            verify(rsp).setStatus(200);
            JSONObject body = JSONObject.fromObject(writer.toString());
            assertEquals(2, body.getLong(PluginCounters.SPOOL_DROPPED));
            assertEquals(1, body.getLong(PluginCounters.EVENTS_DROPPED));
            assertEquals(0, body.getLong(PluginCounters.EVENTS_OVERSIZED));
            assertEquals(0, body.getLong(PluginCounters.LOG_CHUNKS_DROPPED));
            assertEquals(0, body.getLong(PluginCounters.LOG_QUEUES_STUCK));
            assertEquals(0, body.getLong(PluginCounters.CLAIMS_NO_REVISION));
        }
    }

    @Test
    public void unauthorizedCountersCallGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            endpoint.doCounters(requestWithMethod("GET"), rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void theStuckQueueObserversIncrementIsVisibleThroughTheEndpoint() throws Exception {
        AtomicLong clockMillis = new AtomicLong(1_000_000L);
        LogChunkSender.Transport neverAsked = (url, chunk, token, timeout) -> {
            throw new AssertionError("a chunk the pool never runs must never reach the transport");
        };
        LogChunkSender stuckSender = new LogChunkSender("http://127.0.0.1:9095/v1/log",
                new LoopbackTokenFile(), neverAsked, ms -> { }, neverRunsExecutor());
        LogChunkBuffer buffer = new LogChunkBuffer("run-stuck", "folder/job", 1, stuckSender,
                chunk -> { }, 1_000_000, 1_000_000, () -> 1_000, clockMillis::get);
        buffer.append("", "", "hello".getBytes(java.nio.charset.StandardCharsets.UTF_8), 0, 5);
        buffer.finish();
        clockMillis.addAndGet(LogChunkBuffer.STUCK_QUEUE_WARN_MILLIS);
        buffer.observeStuckQueueForTesting(clockMillis.get());

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {
            TailEndpoint endpoint = new TailEndpoint();
            StringWriter writer = new StringWriter();
            StaplerResponse2 rsp = responseCapturing(writer);

            endpoint.doCounters(requestWithMethod("GET"), rsp);

            JSONObject body = JSONObject.fromObject(writer.toString());
            assertEquals(1, body.getLong(PluginCounters.LOG_QUEUES_STUCK));
        }
    }

    /** A pool that accepts a submission but never runs it: the mite that never accepts. */
    private static ExecutorService neverRunsExecutor() {
        return new AbstractExecutorService() {
            @Override
            public void execute(Runnable command) {
                // Deliberately never runs the pump: simulates a mite that
                // never accepts, so the chunk stays queued forever.
            }

            @Override
            public void shutdown() {
            }

            @Override
            public List<Runnable> shutdownNow() {
                return List.of();
            }

            @Override
            public boolean isShutdown() {
                return false;
            }

            @Override
            public boolean isTerminated() {
                return false;
            }

            @Override
            public boolean awaitTermination(long timeout, TimeUnit unit) {
                return true;
            }
        };
    }
}
