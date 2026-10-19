package org.jenkinsci.plugins.varroamiteauth;

import hudson.init.Terminator;
import hudson.model.Computer;
import hudson.model.Node;
import hudson.model.Queue;
import hudson.slaves.OfflineCause;
import hudson.slaves.AbstractCloudSlave;

import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.Predicate;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

/**
 * Releases the idle cloud agents of a cell when its Jenkins stops.
 *
 * <p>On a cell, a stopped Jenkins has no future holder for its cloud
 * agents. The kubernetes plugin keeps an idle pod for {@code idleMinutes}
 * and deletes it only on a later garbage collection. A new cell instance
 * does not refresh a pod that a previous instance created. This class
 * deletes the idle pods during the stop, so they do not wait for that
 * garbage collection.
 *
 * <p>A busy agent stays. A resumed build can adopt its pod. Only an
 * {@link AbstractCloudSlave} is released. A permanent agent is never
 * released.
 *
 * <p>A safe restart also runs this release. A warm idle pod is then
 * deleted too. The cost is a loss of pod reuse only.
 *
 * <p>The release is active only when {@code VARROA_CELL} is {@code true}.
 * It reads that value once. A change needs a pod restart.
 *
 * <p>A hard kill skips this release. When the operator confirms the instance
 * gone, it deletes the idle pods that no resume can adopt.
 */
public final class VarroaCellAgentRelease {

    private static final Logger LOGGER = Logger.getLogger(VarroaCellAgentRelease.class.getName());

    static final Duration BUDGET = Duration.ofSeconds(30);
    private static final int MAX_THREADS = 4;

    private static volatile boolean cellEnabled = readCellEnv();

    private VarroaCellAgentRelease() {
    }

    private static boolean readCellEnv() {
        return "true".equals(System.getenv(VarroaCellJobDslBlock.CELL_ENV));
    }

    /**
     * Terminates the idle cloud agents while Jenkins stops.
     *
     * <p>The method runs before the computers are disconnected. It does
     * nothing when the cell flag is off.
     */
    @Terminator
    public static void releaseIdleAgentsAtStop() {
        if (!cellEnabled) {
            return;
        }
        Jenkins j = Jenkins.getInstanceOrNull();
        if (j == null) {
            return;
        }
        release(j, Computer::isIdle, BUDGET);
    }

    /** Lists the cloud agents whose computer passes the idle test. */
    static List<AbstractCloudSlave> idleCloudAgents(Jenkins j, Predicate<Computer> idle) {
        List<AbstractCloudSlave> out = new ArrayList<>();
        for (Node n : j.getNodes()) {
            if (!(n instanceof AbstractCloudSlave)) {
                continue;
            }
            Computer c = n.toComputer();
            if (c == null || !idle.test(c)) {
                continue;
            }
            out.add((AbstractCloudSlave) n);
        }
        return out;
    }

    /**
     * Terminates the idle cloud agents in parallel within one deadline.
     *
     * <p>A failure of one agent does not stop the others. The method
     * returns the number of agents that terminated normally.
     */
    static int release(Jenkins j, Predicate<Computer> idle, Duration budget) {
        List<AbstractCloudSlave> agents = idleCloudAgents(j, idle);
        if (agents.isEmpty()) {
            return 0;
        }
        AtomicBoolean abandoned = new AtomicBoolean();
        ExecutorService pool = Executors.newFixedThreadPool(
                Math.min(MAX_THREADS, agents.size()), VarroaCellAgentRelease::daemonThread);
        int released = 0;
        try {
            List<Future<Boolean>> futures = new ArrayList<>();
            for (AbstractCloudSlave a : agents) {
                futures.add(pool.submit(() -> {
                    if (!claim(a, idle, abandoned)) {
                        return false;
                    }
                    a.terminate();
                    return true;
                }));
            }
            long deadline = System.nanoTime() + budget.toNanos();
            for (int i = 0; i < futures.size(); i++) {
                Future<Boolean> f = futures.get(i);
                String name = agents.get(i).getNodeName();
                try {
                    long left = Math.max(deadline - System.nanoTime(), 0L);
                    if (f.get(left, TimeUnit.NANOSECONDS)) {
                        released++;
                    }
                } catch (ExecutionException e) {
                    LOGGER.log(Level.WARNING, "VarroaCellAgentRelease: cannot terminate agent " + name,
                            e.getCause());
                } catch (TimeoutException e) {
                    abandoned.set(true);
                    cancelFrom(futures, i);
                    LOGGER.log(Level.INFO, "VarroaCellAgentRelease: budget spent, cancelled {0} agents",
                            futures.size() - i);
                    break;
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    abandoned.set(true);
                    cancelFrom(futures, i);
                    break;
                }
            }
        } finally {
            abandoned.set(true);
            pool.shutdownNow();
        }
        LOGGER.log(Level.INFO, "VarroaCellAgentRelease: released {0} idle cloud agents at stop",
                released);
        return released;
    }

    /**
     * Takes an agent out of scheduling if it is still idle.
     *
     * <p>The check runs under the queue lock. The agent is marked offline in
     * the same step, so no build can take it between the check and the
     * termination. A worker that acquires the lock after the release has
     * returned claims nothing, because the lock can wait past the deadline.
     */
    private static boolean claim(AbstractCloudSlave a, Predicate<Computer> idle, AtomicBoolean abandoned) {
        Computer c = a.toComputer();
        if (c == null) {
            return false;
        }
        boolean[] claimed = new boolean[1];
        Queue.withLock(() -> {
            if (!abandoned.get() && idle.test(c)) {
                c.setTemporarilyOffline(true, new OfflineCause.ByCLI("Cell stop: releasing idle agent"));
                claimed[0] = true;
            }
        });
        return claimed[0];
    }

    private static void cancelFrom(List<Future<Boolean>> futures, int from) {
        for (int k = from; k < futures.size(); k++) {
            futures.get(k).cancel(true);
        }
    }

    private static Thread daemonThread(Runnable r) {
        Thread t = new Thread(r, "varroa-cell-agent-release");
        t.setDaemon(true);
        return t;
    }

    /** Sets the cell flag for a test. */
    static void setCellForTest(boolean v) {
        cellEnabled = v;
    }

    /** Restores the cell flag from the environment after a test. */
    static void resetCellForTest() {
        cellEnabled = readCellEnv();
    }
}
