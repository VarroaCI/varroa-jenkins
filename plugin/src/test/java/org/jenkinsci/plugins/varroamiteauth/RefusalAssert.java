package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import java.io.PrintWriter;
import java.io.StringWriter;

import org.kohsuke.stapler.StaplerResponse2;

/**
 * Test support for {@link PlainTextRefusal}: a mocked response that keeps its body, and the
 * assertions that a route refused with a plain-text body and never through {@code sendError}.
 */
final class RefusalAssert {

    private RefusalAssert() {
    }

    /** A writer that gives back the text written to it. */
    static final class CapturingWriter extends PrintWriter {
        private final StringWriter sink;

        CapturingWriter(StringWriter sink) {
            super(sink, true);
            this.sink = sink;
        }

        String body() {
            flush();
            return sink.toString();
        }
    }

    /** A mocked response whose writer writes into {@code sink}. */
    static StaplerResponse2 capturing(StringWriter sink) throws Exception {
        StaplerResponse2 rsp = mock(StaplerResponse2.class);
        when(rsp.getWriter()).thenReturn(new CapturingWriter(sink));
        return rsp;
    }

    /** A mocked response whose writer writes into a new sink. */
    static StaplerResponse2 capturing() throws Exception {
        return capturing(new StringWriter());
    }

    /** Asserts a plain-text refusal with {@code code} and a body equal to {@code message}. */
    static void assertRefused(StaplerResponse2 rsp, int code, String message) throws Exception {
        assertEquals(message, plainTextBody(rsp, code));
    }

    /** Asserts a plain-text refusal with {@code code} and a body that holds {@code fragment}. */
    static void assertRefusedContaining(StaplerResponse2 rsp, int code, String fragment) throws Exception {
        String body = plainTextBody(rsp, code);
        assertTrue("refusal body " + body + " holds no " + fragment, body.contains(fragment));
    }

    private static String plainTextBody(StaplerResponse2 rsp, int code) throws Exception {
        verify(rsp).setStatus(code);
        verify(rsp).setContentType(PlainTextRefusal.CONTENT_TYPE);
        verify(rsp, never()).sendError(anyInt());
        verify(rsp, never()).sendError(anyInt(), anyString());
        return ((CapturingWriter) rsp.getWriter()).body();
    }
}
