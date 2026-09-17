package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.AsyncPeriodicWork;
import hudson.model.TaskListener;

/**
 * Refreshes {@link VarroaHomeCache} on its own thread, every
 * {@link VarroaHomeCache#pollMillis()} milliseconds.
 *
 * <p>The queue decision handler and the queue task dispatcher read the
 * cache from memory alone, so the read must never reach the loopback.
 * This task is the only caller of the refresh.
 *
 * <p>The task runs only with the claim gate on. Every other class of the
 * claim gate is a no-op without it, and no other caller reads the cache.
 */
@Extension
public class VarroaHomeRefresher extends AsyncPeriodicWork {

    public VarroaHomeRefresher() {
        super("Varroa tenant home map refresh");
    }

    @Override
    public long getRecurrencePeriod() {
        return VarroaHomeCache.pollMillis();
    }

    @Override
    protected void execute(TaskListener listener) {
        if (!gateEnabled(System.getenv(VarroaQueueDecisionHandler.GATE_ENV))) {
            return;
        }
        VarroaHomeCache.get().refresh();
    }

    private static boolean gateEnabled(String raw) {
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }
}
