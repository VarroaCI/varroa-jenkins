package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.function.LongSupplier;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMHeadEvent;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;

/**
 * Bounded, time-limited record of the head events that create or update a
 * branch or a pull request. {@link VarroaPushedHeadListener} writes an entry
 * for each event. {@link VarroaPushedBranchBuildStrategy} consumes an entry
 * once for each branch job that a push creates.
 *
 * <p>Holds at most 256 entries, each alive for ten minutes. The oldest entry
 * is evicted first when the bound is reached. An entry records which owners
 * already started a build from it, so a push builds at most once per branch
 * job.
 */
public final class PushedHeadRegistry {

    static final int CAPACITY = 256;
    static final long TTL_MILLIS = 10L * 60 * 1000;

    private static final PushedHeadRegistry INSTANCE = new PushedHeadRegistry();

    private final List<Entry> entries = new ArrayList<>();
    private final LongSupplier clock;

    /** Returns the process-wide singleton. */
    public static PushedHeadRegistry get() {
        return INSTANCE;
    }

    PushedHeadRegistry() {
        this(System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the clock. */
    PushedHeadRegistry(LongSupplier clock) {
        this.clock = clock;
    }

    /** Records {@code event}'s arrival at the current time. */
    public synchronized void record(SCMHeadEvent<?> event) {
        if (event == null) {
            return;
        }
        if (entries.size() >= CAPACITY) {
            entries.remove(0);
        }
        entries.add(new Entry(event, clock.getAsLong()));
    }

    /**
     * Answers whether a live entry names {@code head} at {@code curr} for
     * {@code source}, and {@code ownerKey} has not consumed that entry yet.
     * The first matching call marks the entry consumed for {@code ownerKey}
     * and answers true. Later calls for the same owner answer false.
     */
    public synchronized boolean consume(SCMSource source, SCMHead head, SCMRevision curr, String ownerKey) {
        long now = clock.getAsLong();
        for (int i = entries.size() - 1; i >= 0; i--) {
            Entry entry = entries.get(i);
            if (now - entry.arrival >= TTL_MILLIS || entry.consumedOwners.contains(ownerKey)) {
                continue;
            }
            if (matches(entry.event, source, head, curr)) {
                entry.consumedOwners.add(ownerKey);
                return true;
            }
        }
        return false;
    }

    private static boolean matches(SCMHeadEvent<?> event, SCMSource source, SCMHead head, SCMRevision curr) {
        try {
            if (!event.isMatch(source)) {
                return false;
            }
            Map<SCMHead, SCMRevision> heads = event.heads(source);
            if (heads == null) {
                return false;
            }
            for (Map.Entry<SCMHead, SCMRevision> named : heads.entrySet()) {
                if (!named.getKey().getName().equals(head.getName())) {
                    continue;
                }
                SCMRevision revision = named.getValue();
                if (revision == null || revision.equals(curr)) {
                    return true;
                }
            }
            return false;
        } catch (RuntimeException e) {
            return false;
        }
    }

    /** Number of entries currently held. Package-private for testing. */
    synchronized int sizeForTesting() {
        return entries.size();
    }

    /** Clears every entry. Package-private for testing. */
    synchronized void resetForTesting() {
        entries.clear();
    }

    private static final class Entry {
        final SCMHeadEvent<?> event;
        final long arrival;
        final Set<String> consumedOwners = new HashSet<>();

        Entry(SCMHeadEvent<?> event, long arrival) {
            this.event = event;
            this.arrival = arrival;
        }
    }
}
