package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;
import org.junit.Test;

/** Plain JUnit tests for the script-approvals envelope, without a Jenkins instance. */
public class ActivityEndpointScriptApprovalsTest {

    private static ActivityEndpoint.PendingRow row(String signature, ScriptRejections.Requester... requesters) {
        List<ScriptRejections.Requester> list = new ArrayList<>();
        Collections.addAll(list, requesters);
        return new ActivityEndpoint.PendingRow(signature, false, list);
    }

    @Test
    public void envelopeListsEveryRowWithItsSignature() {
        List<ActivityEndpoint.PendingRow> rows = new ArrayList<>();
        rows.add(row("method open"));
        rows.add(row("method other"));

        JSONObject env = ActivityEndpoint.scriptApprovalEnvelope(rows);
        JSONArray pending = env.getJSONArray("pending");
        assertEquals(2, pending.size());
        assertEquals("method open", pending.getJSONObject(0).getString("signature"));
        assertEquals(2, env.getInt("total"));
        assertFalse(env.getBoolean("truncated"));
    }

    @Test
    public void listIsCappedAndTotalCountsEveryRow() {
        List<ActivityEndpoint.PendingRow> rows = new ArrayList<>();
        for (int i = 0; i < 30; i++) {
            rows.add(row("method sig" + i));
        }
        JSONObject env = ActivityEndpoint.scriptApprovalEnvelope(rows);
        assertEquals(ActivityEndpoint.MAX_PENDING_LISTED, env.getJSONArray("pending").size());
        assertEquals(30, env.getInt("total"));
        assertTrue(env.getBoolean("truncated"));
    }

    @Test
    public void requesterFieldsUseCamelCaseKeys() {
        List<ActivityEndpoint.PendingRow> rows = new ArrayList<>();
        rows.add(row("method req", new ScriptRejections.Requester("team-a/build", 9, "run-9")));
        JSONObject env = ActivityEndpoint.scriptApprovalEnvelope(rows);

        JSONObject item = env.getJSONArray("pending").getJSONObject(0);
        JSONObject requester = item.getJSONArray("requesters").getJSONObject(0);
        assertEquals("team-a/build", requester.getString("jobPath"));
        assertEquals(9, requester.getInt("buildNumber"));
        assertEquals("run-9", requester.getString("runId"));
        assertTrue(item.has("dangerous"));
    }

    @Test
    public void tenantIsEmptyWhenTheCellHostsNoTenant() {
        assertEquals("", ActivityEndpoint.tenantOfJobPath("team-a/build"));
        assertEquals("", ActivityEndpoint.tenantOfJobPath(""));
    }
}
