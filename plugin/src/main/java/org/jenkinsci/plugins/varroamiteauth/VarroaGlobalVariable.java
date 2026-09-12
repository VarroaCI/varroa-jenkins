package org.jenkinsci.plugins.varroamiteauth;

import groovy.lang.Binding;
import groovy.lang.GroovyClassLoader;

import hudson.Extension;

import java.io.InputStream;
import java.nio.charset.StandardCharsets;

import org.jenkinsci.plugins.workflow.cps.CpsScript;
import org.jenkinsci.plugins.workflow.cps.CpsThread;
import org.jenkinsci.plugins.workflow.cps.GlobalVariable;

/**
 * The {@code varroa} global variable (D1): a Jenkins step name holds no
 * dot, so {@code varroa.build(...)} cannot be a step name. This class
 * compiles the two CPS resources {@code varroa/VarroaRunHandle.groovy} and
 * {@code varroa/Varroa.groovy} into the pipeline's own {@link
 * GroovyClassLoader}, the pattern docker-workflow uses for {@code
 * docker.build}.
 *
 * <p>The shell's own {@link GroovyClassLoader#loadClass} cannot find these
 * resources on its source path: only this plugin's own classloader can, so
 * the source text is read here and handed to {@link
 * GroovyClassLoader#parseClass(String, String)} directly. {@code
 * VarroaRunHandle} is parsed first, so {@code Varroa}'s reference to it
 * resolves against an already-known class rather than triggering the same
 * failed lookup.
 */
@Extension
public class VarroaGlobalVariable extends GlobalVariable {

    @Override
    public String getName() {
        return "varroa";
    }

    @Override
    public Object getValue(CpsScript script) throws Exception {
        // Read the raw binding, never script.getProperty: an unset name
        // falls through the script's property lookup back to every
        // registered GlobalVariable, including this one, and getProperty
        // would recurse into this method forever.
        Binding binding = script.getBinding();
        if (binding.hasVariable(getName())) {
            return binding.getVariable(getName());
        }
        GroovyClassLoader gcl = (GroovyClassLoader) CpsThread.current().getExecution().getShell().getClassLoader();
        loadResourceClass(gcl, "varroa/VarroaRunHandle.groovy", "varroa.VarroaRunHandle");
        Class<?> clazz = loadResourceClass(gcl, "varroa/Varroa.groovy", "varroa.Varroa");
        // Varroa's untyped constructor parameter compiles as Object, not
        // CpsScript, so getConstructor(CpsScript.class) never matches: the
        // class declares exactly one constructor, so take it directly.
        Object varroa = clazz.getDeclaredConstructors()[0].newInstance(script);
        binding.setVariable(getName(), varroa);
        return varroa;
    }

    /**
     * Returns {@code className} from {@code gcl}, compiling {@code
     * resourcePath}'s source text (read from this plugin's own
     * classloader) into it on the first call.
     */
    private static Class<?> loadResourceClass(GroovyClassLoader gcl, String resourcePath, String className)
            throws Exception {
        try {
            return gcl.loadClass(className);
        } catch (ClassNotFoundException notYetParsed) {
            try (InputStream in = VarroaGlobalVariable.class.getClassLoader().getResourceAsStream(resourcePath)) {
                if (in == null) {
                    throw new ClassNotFoundException("plugin resource not found: " + resourcePath, notYetParsed);
                }
                String source = new String(in.readAllBytes(), StandardCharsets.UTF_8);
                return gcl.parseClass(source, resourcePath);
            }
        }
    }
}
