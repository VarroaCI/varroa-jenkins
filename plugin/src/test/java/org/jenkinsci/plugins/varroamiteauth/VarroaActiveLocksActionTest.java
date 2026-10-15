package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import hudson.util.XStream2;

import java.util.Map;

import org.junit.Test;

/**
 * Proves the active-lock stack stays null-safe after the exact
 * restoration mechanism a build resumes through: {@link XStream2},
 * which never calls this class's constructor and never touches a
 * transient field (D11).
 */
public class VarroaActiveLocksActionTest {

    @Test
    public void theStackIsNullSafeAfterXStreamRestore() {
        VarroaActiveLocksAction action = new VarroaActiveLocksAction();
        action.push("alpha", "deploy-prod", 5L);

        XStream2 xstream = new XStream2();
        String xml = xstream.toXML(action);
        VarroaActiveLocksAction restored = (VarroaActiveLocksAction) xstream.fromXML(xml);

        // XStream never touches the transient stack, so the restored
        // action starts with no lock recorded, rather than a null stack
        // that fails the next call.
        assertNull(restored.currentTokenMap());

        restored.push("alpha", "deploy-prod", 9L);
        Map<String, Object> token = restored.currentTokenMap();
        assertEquals("deploy-prod", token.get("name"));
        assertEquals("alpha", token.get("tenant"));
        assertEquals(9L, token.get("token"));

        restored.pop("alpha", "deploy-prod");
        assertNull(restored.currentTokenMap());
    }

    @Test
    public void twoOwnersOfOneBareNameStayApart() {
        VarroaActiveLocksAction action = new VarroaActiveLocksAction();
        action.push("alpha", "deploy-prod", 1L);
        action.push("beta", "deploy-prod", 2L);

        // Popping alpha's entry leaves beta's, the innermost, in place.
        action.pop("alpha", "deploy-prod");
        Map<String, Object> token = action.currentTokenMap();
        assertEquals("beta", token.get("tenant"));
        assertEquals(2L, token.get("token"));

        action.pop("beta", "deploy-prod");
        assertNull(action.currentTokenMap());
    }
}
