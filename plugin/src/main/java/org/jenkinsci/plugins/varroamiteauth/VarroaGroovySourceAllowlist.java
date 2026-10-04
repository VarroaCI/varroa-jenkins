package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;

import java.net.URL;
import java.util.List;
import java.util.Objects;

import org.jenkinsci.plugins.workflow.cps.GroovySourceFileAllowlist;

/**
 * Lets the Pipeline class loader read the two CPS Groovy resources of the
 * {@code varroa} global variable. Without this entry the class loader hides
 * them, and a saved program that holds a {@code varroa.Varroa} instance
 * cannot load after a Jenkins restart.
 *
 * <p>The match is exact: only the URLs this plugin's own class loader
 * returns for the two resources pass. A suffix match would also pass a file
 * with the same path in another plugin.
 */
@Extension
public class VarroaGroovySourceAllowlist extends GroovySourceFileAllowlist {

    static final List<String> RESOURCES = List.of("varroa/Varroa.groovy", "varroa/VarroaRunHandle.groovy");

    @Override
    public boolean isAllowed(String groovyResourceUrl) {
        ClassLoader own = VarroaGroovySourceAllowlist.class.getClassLoader();
        for (String resource : RESOURCES) {
            URL url = own.getResource(resource);
            if (url != null && Objects.equals(url.toString(), groovyResourceUrl)) {
                return true;
            }
        }
        return false;
    }
}
