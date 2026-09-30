package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.inOrder;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import java.io.StringWriter;

import org.htmlunit.WebResponse;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.kohsuke.stapler.StaplerResponse2;
import org.mockito.InOrder;

/**
 * Tests for {@link PlainTextRefusal}: the helper answers a plain-text body, and every Varroa
 * plugin route that refuses a request answers that body over the real servlet stack.
 */
public class PlainTextRefusalTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void refuseResetsAnUncommittedResponse() throws Exception {
        StringWriter sink = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(sink);
        sink.write("{\"runId\":\"partial");

        PlainTextRefusal.refuse(rsp, 500, "Internal server error");

        InOrder order = inOrder(rsp);
        order.verify(rsp).reset();
        order.verify(rsp).setStatus(500);
        order.verify(rsp).setContentType(PlainTextRefusal.CONTENT_TYPE);
        // The mock cannot discard what the sink already holds, so the test reads only the new text.
        assertTrue(sink.toString().endsWith("Internal server error"));
    }

    @Test
    public void refuseKeepsACommittedResponse() throws Exception {
        StaplerResponse2 rsp = RefusalAssert.capturing();
        when(rsp.isCommitted()).thenReturn(true);

        PlainTextRefusal.refuse(rsp, 409, "Conflict: x");

        verify(rsp, never()).reset();
        RefusalAssert.assertRefused(rsp, 409, "Conflict: x");
    }

    @Test
    public void refusalAnswersPlainTextOverHttp() throws Exception {
        assertPlainTextRefusal("varroa-run/trigger", 405, "Method Not Allowed: only POST is served");
        assertPlainTextRefusal("varroa-log/tail", 405, "Method Not Allowed: only POST is served");
        assertPlainTextRefusal("varroa-activity/events", 403,
                "Forbidden: requires ROLE:varroa:system-mite authority");
        assertPlainTextRefusal("varroa-activity/schedules", 403,
                "Forbidden: requires ROLE:varroa:system-mite authority");
    }

    private void assertPlainTextRefusal(String path, int code, String message) throws Exception {
        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        WebResponse response = wc.goTo(path, null).getWebResponse();

        assertEquals(path, code, response.getStatusCode());
        assertEquals(path, "text/plain", response.getContentType());
        assertEquals(path, "UTF-8", response.getContentCharset().name());
        assertEquals(path, message, response.getContentAsString());
    }
}
