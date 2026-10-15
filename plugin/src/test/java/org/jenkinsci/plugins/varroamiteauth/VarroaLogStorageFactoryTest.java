package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;

import org.junit.Test;

/**
 * Unit tests for {@link VarroaLogStorageFactory}'s activation rule (task
 * 6.5): the URL branch, the run-id branch (covered by {@link
 * VarroaLogStorageFactory#forBuild}, exercised through the {@code
 * JenkinsRule} tests), and the per-run cache.
 */
public class VarroaLogStorageFactoryTest {

    @Test
    public void anAbsoluteLoopbackUrlActivates() {
        assertEquals("http://127.0.0.1:9095/v1/log",
                VarroaLogStorageFactory.parseActivationUrl("http://127.0.0.1:9095/v1/log"));
    }

    @Test
    public void anEmptyUrlDoesNotActivate() {
        assertNull(VarroaLogStorageFactory.parseActivationUrl(""));
        assertNull(VarroaLogStorageFactory.parseActivationUrl(null));
    }

    @Test
    public void aNonLoopbackHostDoesNotActivate() {
        assertNull(VarroaLogStorageFactory.parseActivationUrl("http://example.com:9095/v1/log"));
    }

    @Test
    public void aNonHttpSchemeDoesNotActivate() {
        assertNull(VarroaLogStorageFactory.parseActivationUrl("https://127.0.0.1:9095/v1/log"));
    }

    @Test
    public void aRelativeValueDoesNotActivate() {
        assertNull(VarroaLogStorageFactory.parseActivationUrl("/v1/log"));
    }

    @Test
    public void aMalformedUrlDoesNotActivate() {
        assertNull(VarroaLogStorageFactory.parseActivationUrl("http://[::1"));
    }

    @Test
    public void finishOnAnUncachedRunReturnsNull() {
        VarroaLogStorageFactory factory = new VarroaLogStorageFactory();
        assertNull(factory.finish("never-captured"));
    }

    @Test
    public void setTailActiveOnAnUncachedRunReturnsFalse() {
        VarroaLogStorageFactory factory = new VarroaLogStorageFactory();
        assertFalse(factory.setTailActive("never-captured", true));
    }
}
