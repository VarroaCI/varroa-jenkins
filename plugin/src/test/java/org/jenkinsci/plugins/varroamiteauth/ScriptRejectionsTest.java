package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import java.util.List;
import org.jenkinsci.plugins.scriptsecurity.sandbox.RejectedAccessException;
import org.junit.Test;

/** Plain JUnit tests for the bounded refusal map in {@link ScriptRejections}. */
public class ScriptRejectionsTest {

    private static ScriptRejections.Rejection rej(String sig) {
        return new ScriptRejections.Rejection(sig, false);
    }

    private static ScriptRejections.Entry entryFor(String sig) {
        for (ScriptRejections.Entry e : ScriptRejections.entries()) {
            if (e.getSignature().equals(sig)) {
                return e;
            }
        }
        return null;
    }

    @Test
    public void requestersAreNewestFirstAndDeduplicatedByJob() {
        ScriptRejections.record(rej("method a b"), "team-a/one", 1, "r1");
        ScriptRejections.record(rej("method a b"), "team-a/two", 2, "r2");
        ScriptRejections.record(rej("method a b"), "team-a/one", 3, "r3");

        List<ScriptRejections.Requester> got = entryFor("method a b").getRequesters();
        assertEquals(2, got.size());
        assertEquals("team-a/one", got.get(0).getJobPath());
        assertEquals(3, got.get(0).getBuildNumber());
        assertEquals("team-a/two", got.get(1).getJobPath());
    }

    @Test
    public void requestersAreCappedAtThree() {
        for (int i = 0; i < 5; i++) {
            ScriptRejections.record(rej("method c d"), "team-a/job-" + i, i, "r" + i);
        }
        List<ScriptRejections.Requester> got = entryFor("method c d").getRequesters();
        assertEquals(ScriptRejections.MAX_REQUESTERS, got.size());
        assertEquals("team-a/job-4", got.get(0).getJobPath());
    }

    @Test
    public void entriesAreNewestSignatureFirst() {
        ScriptRejections.record(rej("sig-first-x"), "team-a/a", 1, "r");
        ScriptRejections.record(rej("sig-second-x"), "team-a/b", 1, "r");
        List<ScriptRejections.Entry> all = ScriptRejections.entries();
        int first = -1;
        int second = -1;
        for (int i = 0; i < all.size(); i++) {
            if (all.get(i).getSignature().equals("sig-first-x")) {
                first = i;
            }
            if (all.get(i).getSignature().equals("sig-second-x")) {
                second = i;
            }
        }
        assertTrue(second < first);
    }

    @Test
    public void oldestSignatureLeavesFirstWhenFull() {
        for (int i = 0; i <= ScriptRejections.MAX_SIGNATURES; i++) {
            ScriptRejections.record(rej("bounded-" + i), "team-a/job", i, "r");
        }
        assertNull(entryFor("bounded-0"));
        assertTrue(entryFor("bounded-" + ScriptRejections.MAX_SIGNATURES) != null);
    }

    @Test
    public void dangerousFlagIsKept() {
        ScriptRejections.record(new ScriptRejections.Rejection("danger-x", true), "team-a/a", 1, "r");
        assertTrue(entryFor("danger-x").isDangerous());
    }

    @Test
    public void nullOrEmptySignatureIsIgnored() {
        ScriptRejections.record(null, "team-a/a", 1, "r");
        ScriptRejections.record(rej(""), "team-a/a", 1, "r");
        assertNull(entryFor(""));
    }

    @Test
    public void rejectionIsFoundThroughTheCauseChain() {
        Throwable wrapped = new IllegalStateException("outer",
                new RuntimeException("middle", new RejectedAccessException("denied", "method x y")));
        ScriptRejections.Rejection found = ScriptRejections.rejectionIn(wrapped);
        assertTrue(found.getSignature().endsWith("method x y"));
    }

    @Test
    public void rejectionWalkStopsOnACycle() {
        RuntimeException a = new RuntimeException("a");
        RuntimeException b = new RuntimeException("b", a);
        a.initCause(b);
        assertNull(ScriptRejections.rejectionIn(a));
    }

    @Test
    public void nonRejectionFailureHasNoSignature() {
        assertNull(ScriptRejections.rejectionIn(new RuntimeException("boom")));
        assertNull(ScriptRejections.rejectionIn(null));
    }
}
