package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import java.net.URL;

import org.junit.Test;

/** Tests the exact-URL match of {@link VarroaGroovySourceAllowlist}. */
public class VarroaGroovySourceAllowlistTest {

    private final VarroaGroovySourceAllowlist allowlist = new VarroaGroovySourceAllowlist();

    @Test
    public void allowsBothPluginResources() {
        for (String resource : VarroaGroovySourceAllowlist.RESOURCES) {
            URL url = VarroaGroovySourceAllowlist.class.getClassLoader().getResource(resource);
            assertNotNull(resource, url);
            assertTrue(resource, allowlist.isAllowed(url.toString()));
        }
    }

    @Test
    public void refusesTheSameSuffixFromAnotherJar() {
        assertFalse(allowlist.isAllowed("jar:file:/var/jenkins_home/plugins/other/WEB-INF/lib/other.jar!/varroa/Varroa.groovy"));
        assertFalse(allowlist.isAllowed("file:/tmp/elsewhere/varroa/VarroaRunHandle.groovy"));
    }

    @Test
    public void refusesAnotherGroovyResource() {
        URL varroa = VarroaGroovySourceAllowlist.class.getClassLoader().getResource("varroa/Varroa.groovy");
        assertNotNull(varroa);
        assertFalse(allowlist.isAllowed(varroa.toString().replace("Varroa.groovy", "Other.groovy")));
        assertFalse(allowlist.isAllowed(null));
    }
}
