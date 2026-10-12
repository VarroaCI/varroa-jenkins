package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import net.sf.json.JSONObject;
import org.junit.Test;

/** Tests for the pure head diff and the scan record of {@link VarroaBranchScans}. */
public class VarroaBranchScansTest {

    private static Map<String, VarroaBranchScans.Head> heads(Object... pairs) {
        Map<String, VarroaBranchScans.Head> out = new LinkedHashMap<>();
        for (int i = 0; i < pairs.length; i += 3) {
            out.put((String) pairs[i],
                    new VarroaBranchScans.Head((String) pairs[i + 1], (Boolean) pairs[i + 2]));
        }
        return out;
    }

    @Test
    public void newHeadIsInAfterOnly() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("main", "a", true),
                heads("main", "a", true, "feature", "b", true));

        assertEquals(List.of("feature"), d.newHeads);
        assertTrue(d.changedHeads.isEmpty());
        assertTrue(d.orphanedHeads.isEmpty());
    }

    @Test
    public void headMissingFromAfterIsOrphaned() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("main", "a", true, "old", "b", true),
                heads("main", "a", true));

        assertEquals(List.of("old"), d.orphanedHeads);
        assertTrue(d.newHeads.isEmpty());
    }

    @Test
    public void buildableHeadThatTurnsUnbuildableIsOrphaned() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("main", "a", true),
                heads("main", "a", false));

        assertEquals(List.of("main"), d.orphanedHeads);
        assertTrue(d.changedHeads.isEmpty());
    }

    @Test
    public void unbuildableHeadStaysOutOfOrphanedWhenItStaysUnbuildable() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("gone", "a", false),
                heads("gone", "b", false));

        assertTrue(d.orphanedHeads.isEmpty());
        assertTrue(d.changedHeads.isEmpty());
    }

    @Test
    public void buildableHeadWithNewHashIsChanged() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("main", "a", true, "dev", "x", true),
                heads("main", "b", true, "dev", "x", true));

        assertEquals(List.of("main"), d.changedHeads);
        assertTrue(d.newHeads.isEmpty());
        assertTrue(d.orphanedHeads.isEmpty());
    }

    @Test
    public void eachListIsCappedAndSorted() {
        Map<String, VarroaBranchScans.Head> after = new LinkedHashMap<>();
        for (int i = VarroaBranchScans.LIST_CAP + 50; i >= 0; i--) {
            after.put(String.format("b%04d", i), new VarroaBranchScans.Head("h", true));
        }
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(new LinkedHashMap<>(), after);

        assertEquals(VarroaBranchScans.LIST_CAP, d.newHeads.size());
        assertEquals("b0000", d.newHeads.get(0));
        assertEquals(String.format("b%04d", VarroaBranchScans.LIST_CAP - 1),
                d.newHeads.get(VarroaBranchScans.LIST_CAP - 1));
    }

    @Test
    public void refusedScanAnswersItsStateAndMessage() {
        String fullName = "refused-record/mb";
        VarroaBranchScans.recordRefused(fullName, "scan-1", "ada", "the acting user may not scan this job");

        JSONObject row = (JSONObject) VarroaBranchScans.lastScanJson(fullName);
        assertEquals("scan-1", row.getString("scanId"));
        assertEquals("refused", row.getString("state"));
        assertEquals("ada", row.getString("actor"));
        assertEquals("the acting user may not scan this job", row.getString("message"));
        assertEquals("", row.getString("result"));
    }

    @Test
    public void aScanIdIsClaimedOnceAndAnsweredByItsOutcome() {
        String fullName = "claim-check/mb";
        assertEquals(VarroaBranchScans.CLAIM_NEW, VarroaBranchScans.claim(fullName, "scan-a"));
        assertEquals("an in-flight duplicate waits for its owner",
                VarroaBranchScans.CLAIM_IN_FLIGHT, VarroaBranchScans.claim(fullName, "scan-a"));
        assertEquals(VarroaBranchScans.CLAIM_NEW, VarroaBranchScans.claim(fullName, "scan-b"));
        VarroaBranchScans.markAccepted(fullName, "scan-a");
        assertEquals("an accepted duplicate answers accepted",
                VarroaBranchScans.CLAIM_ACCEPTED, VarroaBranchScans.claim(fullName, "scan-a"));
        VarroaBranchScans.recordRefused(fullName, "scan-b", "ada", "refused");
        assertEquals("a refused reservation is released for a retry",
                VarroaBranchScans.CLAIM_NEW, VarroaBranchScans.claim(fullName, "scan-b"));
    }

    @Test
    public void jobWithoutScanAnswersNull() {
        assertTrue(VarroaBranchScans.lastScanJson("never-scanned/mb") instanceof net.sf.json.JSONNull);
    }
}
