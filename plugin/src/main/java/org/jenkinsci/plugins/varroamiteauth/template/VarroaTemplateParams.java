package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.Extension;
import hudson.model.Run;

import java.util.Collections;
import java.util.Map;

import org.jenkinsci.plugins.workflow.cps.CpsScript;
import org.jenkinsci.plugins.workflow.cps.GlobalVariable;

/**
 * Exposes a marker's or an items-declared template's validated values as
 * {@code varroaParams} in a pipeline body (D9). Values never enter Groovy
 * source text: {@code getValue} returns an unmodifiable map read from
 * {@link VarroaTemplateValuesAction}, so a value holding a quote, a {@code
 * ${}}, or a closure stays inert data under the sandbox's generic map
 * access rules.
 */
@Extension
public class VarroaTemplateParams extends GlobalVariable {

    @Override
    public String getName() {
        return "varroaParams";
    }

    @Override
    public Object getValue(CpsScript script) {
        Run<?, ?> run = script.$buildNoException();
        if (run == null) {
            return Collections.emptyMap();
        }
        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        if (action == null) {
            return Collections.emptyMap();
        }
        Map<String, Object> values = action.getValues();
        return values != null ? values : Collections.emptyMap();
    }
}
