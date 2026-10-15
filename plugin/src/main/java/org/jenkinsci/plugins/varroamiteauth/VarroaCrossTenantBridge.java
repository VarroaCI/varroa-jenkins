package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Queue;
import hudson.model.Run;

import java.io.IOException;
import java.util.Map;

import org.jenkinsci.plugins.workflow.cps.CpsThread;

/**
 * The public door this package opens to {@code varroa.Varroa} and {@code
 * varroa.VarroaRunHandle} (D1): the two CPS Groovy resources {@link
 * VarroaGlobalVariable} loads. They run in a different package, so normal
 * Java visibility, not CPS trust, decides what they may call directly.
 *
 * <p>Neither method is on the sandbox allowlist. The two resources compile
 * in the trusted Pipeline shell, so they need no allowlist entry, and a
 * sandboxed script must not call this class directly.
 *
 * <p>Both methods resolve the calling run from the current CPS thread, and
 * never from an argument. The sandbox permits every constructor of the two
 * resources, so a script can build a handle or a {@code varroa.Varroa}
 * from values of its own choice.
 */
public final class VarroaCrossTenantBridge {

    private VarroaCrossTenantBridge() {
    }

    /**
     * Backs {@code VarroaRunHandle.refresh()}: re-reads the record by run
     * id when one is known, otherwise by intent key, as the running
     * pipeline's own job. Throws when no pipeline runs on this thread, and
     * when the recorder does not answer.
     */
    public static Map<String, Object> refresh(String tenant, String job, String runId, String intentKey) {
        Run<?, ?> run = currentRun();
        if (run == null) {
            throw new IllegalStateException("varroa.refresh: no running pipeline on this thread");
        }
        return VarroaCrossTenantOps.refresh(run.getParent().getFullName(), tenant, job, runId, intentKey);
    }

    /**
     * Backs {@code varroa.lock.token}: the innermost active lock's name
     * and fence token, or {@code null} with no lock held, and with no
     * current run (D11).
     */
    public static Map<String, Object> currentLockToken() {
        Run<?, ?> run = currentRun();
        return run != null ? VarroaActiveLocksAction.currentTokenMap(run) : null;
    }

    /** The run of the current CPS thread's flow execution, or {@code null} off a CPS thread. */
    static Run<?, ?> currentRun() {
        CpsThread thread = CpsThread.current();
        if (thread == null) {
            return null;
        }
        try {
            Queue.Executable exec = thread.getExecution().getOwner().getExecutable();
            return exec instanceof Run ? (Run<?, ?>) exec : null;
        } catch (IOException e) {
            return null;
        }
    }
}
