package org.jenkinsci.plugins.varroamiteauth;

import groovy.lang.Binding;

import hudson.Extension;

import org.jenkinsci.plugins.workflow.cps.CpsScript;
import org.jenkinsci.plugins.workflow.cps.CpsThread;
import org.jenkinsci.plugins.workflow.cps.GlobalVariable;

/**
 * The {@code varroa} global variable (D1): a Jenkins step name holds no
 * dot, so {@code varroa.build(...)} cannot be a step name. The value is an
 * instance of the CPS resource {@code varroa/Varroa.groovy}, loaded by
 * name through the Pipeline class loader, the pattern docker-workflow uses
 * for {@code docker.build}.
 *
 * <p>The class must load by name, never through a direct parse of its
 * source: a saved program holds the instance, and a restarted Jenkins
 * resolves its class by name before this method runs again. {@link
 * VarroaGroovySourceAllowlist} lets the class loader read both resources.
 */
@Extension
public class VarroaGlobalVariable extends GlobalVariable {

    static final String CLASS_NAME = "varroa.Varroa";

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
        Class<?> clazz = CpsThread.current().getExecution().getShell().getClassLoader().loadClass(CLASS_NAME);
        // Varroa's untyped constructor parameter compiles as Object, not
        // CpsScript, so getConstructor(CpsScript.class) never matches: the
        // class declares exactly one constructor, so take it directly.
        Object varroa = clazz.getDeclaredConstructors()[0].newInstance(script);
        binding.setVariable(getName(), varroa);
        return varroa;
    }
}
