package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import hudson.util.XStream2;

import org.junit.Test;

/**
 * Proves a restored intent action never holds a grant. XStream2 skips a
 * transient field on write, but it still reads that field when a stored
 * file names it. The restore must clear the grant anyway.
 */
public class VarroaIntentActionRestoreTest {

    @Test
    public void aStoredGrantedElementRestoresWithoutTheGrant() {
        VarroaIntentAction action = new VarroaIntentAction("run-7", "", null, System.currentTimeMillis());
        action.setIntentKey("key-7");
        action.setGranted(true);
        action.setGrantedNumber(7L);

        XStream2 xstream = new XStream2();
        String xml = xstream.toXML(action);
        assertFalse("a granted action writes no granted element", xml.contains("<granted>"));

        // Insert the element a queue file holds when the grant was saved.
        int rootEnd = xml.indexOf('>') + 1;
        String stored = xml.substring(0, rootEnd) + "<granted>true</granted>" + xml.substring(rootEnd);
        assertTrue("the stored file names the granted element", stored.contains("<granted>true</granted>"));

        VarroaIntentAction restored = (VarroaIntentAction) xstream.fromXML(stored);

        assertFalse("a restored action holds no grant", restored.isGranted());
        assertEquals("run-7", restored.getRunId());
        assertEquals("key-7", restored.getIntentKey());
        assertEquals(7L, restored.getGrantedNumber());
    }
}
