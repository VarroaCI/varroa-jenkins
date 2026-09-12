package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Run;

import java.util.Map;

import org.jenkinsci.plugins.scriptsecurity.sandbox.whitelists.Whitelisted;
import org.jenkinsci.plugins.workflow.cps.CpsScript;

/**
 * The public door this package opens to {@code varroa.Varroa} and {@code
 * varroa.VarroaRunHandle} (D1): the two CPS Groovy resources {@link
 * VarroaGlobalVariable} loads. They run in a different package, so normal
 * Java visibility, not CPS trust, decides what they may call directly.
 *
 * <p>Both methods carry {@link Whitelisted}: a marker plugin resource
 * loaded through the same {@code GroovyShell} as the calling pipeline can
 * still run under that pipeline's own sandbox setting, so this door stays
 * open whether or not it does.
 */
public final class VarroaCrossTenantBridge {

    private VarroaCrossTenantBridge() {
    }

    /**
     * Backs {@code VarroaRunHandle.refresh()}: re-reads the record by run
     * id when one is known, otherwise by intent key. Throws when the
     * recorder does not answer.
     */
    @Whitelisted
    public static Map<String, Object> refresh(String callerJobPath, String tenant, String job,
            String runId, String intentKey) {
        return VarroaCrossTenantOps.refresh(callerJobPath, tenant, job, runId, intentKey);
    }

    /**
     * Backs {@code varroa.lock.token}: the innermost active lock's name
     * and fence token, or {@code null} with no lock held, and with no
     * current run (D11). Takes the script itself, and resolves the run
     * from Java, so the Groovy caller never calls {@code CpsScript}'s own
     * run accessor directly.
     */
    @Whitelisted
    public static Map<String, Object> currentLockToken(CpsScript script) {
        Run<?, ?> run = script.$buildNoException();
        return run != null ? VarroaActiveLocksAction.currentTokenMap(run) : null;
    }
}
