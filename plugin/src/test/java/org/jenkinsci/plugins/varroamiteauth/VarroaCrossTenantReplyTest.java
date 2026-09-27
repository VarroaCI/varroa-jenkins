package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * Tests for {@link VarroaCrossTenantReply}: the failure code, the text a
 * step shows for it, and the veto fields.
 */
public class VarroaCrossTenantReplyTest {

    private static VarroaCrossTenantReply parse(String body) {
        return VarroaCrossTenantReply.parse(new MiteLoopbackClient.Response(200, body, null));
    }

    @Test
    public void aStructuredFailureShowsItsCodeAndItsError() {
        VarroaCrossTenantReply reply = parse("{\"code\":\"NOT_FOUND\",\"error\":\"no artifact at path \\\"x\\\"\"}");
        assertTrue(reply.isFailure());
        assertEquals("NOT_FOUND", reply.code);
        assertEquals("NOT_FOUND: no artifact at path \"x\"", reply.describe());
    }

    @Test
    public void aFailureWithNoErrorShowsItsCodeAlone() {
        VarroaCrossTenantReply reply = parse("{\"code\":\"UNAVAILABLE\"}");
        assertEquals("UNAVAILABLE", reply.describe());
    }

    @Test
    public void aTransportFailureCarriesUnavailableAndTheSocketText() {
        VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                new MiteLoopbackClient.Response(0, null, "Connection refused"));
        assertEquals(VarroaCrossTenantReply.CODE_UNAVAILABLE, reply.code);
        assertEquals("Connection refused", reply.message);
        assertEquals("UNAVAILABLE: Connection refused", reply.describe());
    }

    @Test
    public void aBodyThatDoesNotDecodeNamesTheDecodeFailure() {
        VarroaCrossTenantReply reply = parse("not json");
        assertEquals("UNAVAILABLE: the reply did not decode", reply.describe());
    }

    @Test
    public void aVetoIsNoFailureAndShowsItsReasonAndMessage() {
        VarroaCrossTenantReply reply = parse(
                "{\"reason_code\":\"CROSS_TENANT\",\"message\":\"Tenant beta does not grant job x to alpha.\"}");
        assertFalse(reply.isFailure());
        assertEquals("CROSS_TENANT", reply.reasonCode());
        assertEquals("CROSS_TENANT: Tenant beta does not grant job x to alpha.", reply.describeVeto());
    }

    @Test
    public void aReplyWithNoReasonCodeIsNoVeto() {
        VarroaCrossTenantReply reply = parse("{\"run_id\":\"run-1\"}");
        assertEquals("", reply.reasonCode());
        assertFalse(reply.isPermanentVeto());
    }

    @Test
    public void notReadyAndThrottledAreTheRetryableVetoes() {
        assertFalse(parse("{\"reason_code\":\"NOT_READY\"}").isPermanentVeto());
        assertFalse(parse("{\"reason_code\":\"THROTTLED\"}").isPermanentVeto());
        assertTrue(parse("{\"reason_code\":\"CROSS_TENANT\"}").isPermanentVeto());
        assertTrue(parse("{\"reason_code\":\"FENCED\"}").isPermanentVeto());
    }
}
