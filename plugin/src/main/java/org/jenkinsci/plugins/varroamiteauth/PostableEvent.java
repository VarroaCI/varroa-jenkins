package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Common shape for anything {@link BuildEventDispatcher} can post to the
 * mite loopback: a live {@link BuildEvent} drained from the sink, or a
 * {@link RunSpool.Entry} replayed from disk. The dispatcher's splitting,
 * retry, and acknowledgement logic works against this interface so a
 * replayed spool file needs no round trip through a rebuilt {@link
 * BuildEvent}.
 */
interface PostableEvent {

    String getRunId();

    long getSeq();

    boolean isHeartbeat();

    /** {@code true} when the event's phase is {@code COMPLETED}. */
    boolean isCompletedPhase();

    JSONObject toJson();
}
