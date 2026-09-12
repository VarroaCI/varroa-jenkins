package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.InvisibleAction;
import hudson.model.Run;

import java.util.ArrayDeque;
import java.util.Deque;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Tracks a run's active fleet locks as a stack (D11): the innermost active
 * lock is the one most recently pushed, and {@code varroa.lock.token}
 * reads it. Transient: this action exists only while the run's JVM is
 * alive. A lock only matters while its holder is running, so this
 * bookkeeping needs no durability across a restart.
 *
 * <p>{@link VarroaLockStep} also reads this stack to refuse a second
 * {@code varroa.lock} block while the run already holds one. The Jenkins
 * pipeline interpreter cannot resume a caller's next statement correctly
 * after a block step's body itself starts a second, nested block step;
 * the statement after the inner block fails with an interpreter-internal
 * error naming no plugin code. This holds for any two block steps nested
 * this way, not only {@code varroa.lock} — a plain, built-in {@code
 * timeout} block nested inside another reproduces the same failure on
 * this Jenkins core and Pipeline: Groovy version pair. Refusing the
 * second acquire trades the design's stated nested-lock support for a
 * clear, immediate failure instead of a corrupted build.
 */
public final class VarroaActiveLocksAction extends InvisibleAction {

    /**
     * XStream restores a saved build without calling this class's
     * constructor and never touches a transient field, so this field is
     * {@code null} on a restored action until {@link #stack()} first
     * initializes it.
     */
    private transient Deque<Entry> stack;

    private static final class Entry {
        final String name;
        final long fenceToken;

        Entry(String name, long fenceToken) {
            this.name = name;
            this.fenceToken = fenceToken;
        }
    }

    /** Returns the run's active-locks action, creating and attaching one on first use. */
    static VarroaActiveLocksAction of(Run<?, ?> run) {
        synchronized (run) {
            VarroaActiveLocksAction action = run.getAction(VarroaActiveLocksAction.class);
            if (action == null) {
                action = new VarroaActiveLocksAction();
                run.addAction(action);
            }
            return action;
        }
    }

    /**
     * Returns the stack, initializing it on first use. Every reader and
     * writer of {@link #stack} goes through this method, never the field
     * directly, so a restored action's null stack never reaches a null
     * dereference.
     */
    private Deque<Entry> stack() {
        if (stack == null) {
            stack = new ArrayDeque<>();
        }
        return stack;
    }

    /** Pushes a newly acquired lock; it becomes the innermost active lock. */
    synchronized void push(String name, long fenceToken) {
        stack().push(new Entry(name, fenceToken));
    }

    /**
     * Removes {@code name}'s entry. Removes the top entry when it matches,
     * so a correctly nested release is O(1); otherwise scans, so a release
     * out of nesting order still clears its own entry.
     */
    synchronized void pop(String name) {
        Deque<Entry> s = stack();
        Entry top = s.peek();
        if (top != null && top.name.equals(name)) {
            s.pop();
            return;
        }
        s.removeIf(e -> e.name.equals(name));
    }

    /**
     * Returns a map carrying the innermost active lock's {@code name} and
     * {@code token}, or {@code null} when no lock is held (D11).
     */
    synchronized Map<String, Object> currentTokenMap() {
        Entry top = stack().peek();
        if (top == null) {
            return null;
        }
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("name", top.name);
        m.put("token", top.fenceToken);
        return m;
    }

    /**
     * Reads {@code run}'s innermost active lock without attaching a new,
     * empty action to a run that never acquired one.
     */
    static Map<String, Object> currentTokenMap(Run<?, ?> run) {
        VarroaActiveLocksAction action = run.getAction(VarroaActiveLocksAction.class);
        return action != null ? action.currentTokenMap() : null;
    }

    private synchronized boolean hasAny() {
        return !stack().isEmpty();
    }
}
