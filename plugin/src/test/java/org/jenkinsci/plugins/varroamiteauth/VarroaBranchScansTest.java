package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;
import org.junit.Test;

/** Tests for the pure head diff and the scan record of {@link VarroaBranchScans}. */
public class VarroaBranchScansTest {

    /** Builds heads keyed by job path. Each group is path, branch name, hash, buildable. */
    private static Map<String, VarroaBranchScans.Head> heads(Object... quads) {
        Map<String, VarroaBranchScans.Head> out = new LinkedHashMap<>();
        for (int i = 0; i < quads.length; i += 4) {
            out.put((String) quads[i],
                    new VarroaBranchScans.Head((String) quads[i + 1], (String) quads[i + 2], (Boolean) quads[i + 3]));
        }
        return out;
    }

    /** Answers each ref as "name@jobPath", in list order. */
    private static List<String> refs(List<VarroaBranchScans.Ref> refs) {
        List<String> out = new ArrayList<>();
        for (VarroaBranchScans.Ref ref : refs) {
            out.add(ref.name + "@" + ref.jobPath);
        }
        return out;
    }

    @Test
    public void newHeadIsInAfterOnly() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/main", "main", "a", true),
                heads("mb/main", "main", "a", true, "mb/feature", "feature", "b", true));

        assertEquals(List.of("feature@mb/feature"), refs(d.newHeads));
        assertTrue(d.changedHeads.isEmpty());
        assertTrue(d.orphanedHeads.isEmpty());
    }

    @Test
    public void headMissingFromAfterIsOrphanedWithItsJobPath() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/main", "main", "a", true, "mb/old", "old", "b", true),
                heads("mb/main", "main", "a", true));

        assertEquals(List.of("old@mb/old"), refs(d.orphanedHeads));
        assertTrue(d.newHeads.isEmpty());
    }

    @Test
    public void buildableHeadThatTurnsUnbuildableIsOrphaned() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/main", "main", "a", true),
                heads("mb/main", "main", "a", false));

        assertEquals(List.of("main@mb/main"), refs(d.orphanedHeads));
        assertTrue(d.changedHeads.isEmpty());
    }

    @Test
    public void unbuildableHeadStaysOutOfOrphanedWhenItStaysUnbuildable() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/gone", "gone", "a", false),
                heads("mb/gone", "gone", "b", false));

        assertTrue(d.orphanedHeads.isEmpty());
        assertTrue(d.changedHeads.isEmpty());
    }

    @Test
    public void buildableHeadWithNewHashIsChanged() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/main", "main", "a", true, "mb/dev", "dev", "x", true),
                heads("mb/main", "main", "b", true, "mb/dev", "dev", "x", true));

        assertEquals(List.of("main@mb/main"), refs(d.changedHeads));
        assertTrue(d.newHeads.isEmpty());
        assertTrue(d.orphanedHeads.isEmpty());
    }

    @Test
    public void twoHeadsWithOneNameStaySeparate() {
        // A branch and a tag that share the name "release" live in two jobs.
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("mb/release", "release", "a", true, "mb/tags%2Frelease", "release", "t", true),
                heads("mb/tags%2Frelease", "release", "t", true));

        assertEquals(List.of("release@mb/release"), refs(d.orphanedHeads));
        assertTrue(d.newHeads.isEmpty());
    }

    @Test
    public void headMissingFromAfterCarriesItsJobPath() {
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("payments/mb/gone", "gone", "a", true),
                heads());

        assertEquals("the job path survives the head's removal",
                List.of("gone@payments/mb/gone"), refs(d.orphanedHeads));
    }

    @Test
    public void eachListIsCappedAndSortedByName() {
        Map<String, VarroaBranchScans.Head> after = new LinkedHashMap<>();
        for (int i = VarroaBranchScans.LIST_CAP + 50; i >= 0; i--) {
            after.put(String.format("mb/p%04d", i), new VarroaBranchScans.Head(String.format("b%04d", i), "h", true));
        }
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(new LinkedHashMap<>(), after);

        assertEquals(VarroaBranchScans.LIST_CAP, d.newHeads.size());
        assertEquals("b0000", d.newHeads.get(0).name);
        assertEquals(String.format("b%04d", VarroaBranchScans.LIST_CAP - 1),
                d.newHeads.get(VarroaBranchScans.LIST_CAP - 1).name);
    }

    @Test
    public void lastScanJsonWritesHeadRefs() {
        String fullName = "head-refs/mb";
        VarroaBranchScans.Diff d = new VarroaBranchScans.Diff(
                List.of(new VarroaBranchScans.Ref("feature", "payments/mb/feature")),
                List.of(),
                List.of(new VarroaBranchScans.Ref("gone", "payments/mb/gone")));
        VarroaBranchScans.recordCompletedForTest(fullName, "scan-refs", d);

        JSONObject row = (JSONObject) VarroaBranchScans.lastScanJson(fullName);
        JSONArray orphaned = row.getJSONArray("orphanedHeadRefs");
        assertEquals(1, orphaned.size());
        assertEquals("gone", orphaned.getJSONObject(0).getString("name"));
        assertEquals("payments/mb/gone", orphaned.getJSONObject(0).getString("jobPath"));
        assertEquals("feature", row.getJSONArray("newHeadRefs").getJSONObject(0).getString("name"));
        assertTrue(row.getJSONArray("changedHeadRefs").isEmpty());
        assertTrue("the names-only keys are gone", !row.containsKey("orphanedHeads"));
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

    @Test
    public void repositoryHeadsOfAnOrganizationFolderDiffByJobPath() {
        // An organization folder records one head per repository, with an empty hash.
        VarroaBranchScans.Diff d = VarroaBranchScans.diff(
                heads("org/repo-a", "repo-a", "", true, "org/repo-gone", "repo-gone", "", true),
                heads("org/repo-a", "repo-a", "", true, "org/repo-new", "repo-new", "", true));

        assertEquals(List.of("repo-new@org/repo-new"), refs(d.newHeads));
        assertEquals(List.of("repo-gone@org/repo-gone"), refs(d.orphanedHeads));
        assertTrue(d.changedHeads.isEmpty());
    }
}
