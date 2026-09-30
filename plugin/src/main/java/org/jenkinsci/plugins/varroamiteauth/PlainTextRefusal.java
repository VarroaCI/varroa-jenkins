package org.jenkinsci.plugins.varroamiteauth;

import java.io.IOException;
import java.io.PrintWriter;

import org.kohsuke.stapler.StaplerResponse2;

/**
 * Writes the refusal of a Varroa plugin route as plain text.
 *
 * <p>Every refusal on {@code /varroa-run}, {@code /varroa-activity}, and {@code /varroa-log}
 * goes through {@link #refuse}. Do not call {@code sendError} on these routes: Jenkins then
 * renders its full HTML error page and keeps the message only in the page title.
 */
final class PlainTextRefusal {

    /** The content type of every refusal body. */
    static final String CONTENT_TYPE = "text/plain; charset=UTF-8";

    private PlainTextRefusal() {
    }

    /**
     * Answers {@code code} with {@code msg} as the whole body. An uncommitted response loses
     * its earlier status, headers, and buffered body first, so a late failure never appends
     * to a partial success body.
     */
    static void refuse(StaplerResponse2 rsp, int code, String msg) throws IOException {
        if (!rsp.isCommitted()) {
            rsp.reset();
        }
        rsp.setStatus(code);
        rsp.setContentType(CONTENT_TYPE);
        PrintWriter writer = rsp.getWriter();
        writer.write(msg);
        writer.flush();
    }
}
