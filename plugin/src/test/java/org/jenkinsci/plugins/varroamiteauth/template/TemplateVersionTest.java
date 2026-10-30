package org.jenkinsci.plugins.varroamiteauth.template;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotEquals;

import org.junit.Test;

public class TemplateVersionTest {

    @Test
    public void equalVersionsAreEqualAndHashAlike() {
        TemplateVersion a = TemplateVersion.parse("1.2.3");
        TemplateVersion b = TemplateVersion.parse("1.2.3");
        assertEquals(a, b);
        assertEquals(a.hashCode(), b.hashCode());
        assertEquals(0, a.compareTo(b));
    }

    @Test
    public void differentVersionsAreNotEqual() {
        TemplateVersion a = TemplateVersion.parse("1.2.3");
        assertNotEquals(a, TemplateVersion.parse("1.2.4"));
        assertFalse(a.equals(null));
        assertFalse(a.equals("1.2.3"));
    }
}
