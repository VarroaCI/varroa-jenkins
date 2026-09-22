package varroa

/**
 * CPS class backing the {@code varroa} global variable (D1). One instance
 * per run, loaded dynamically by {@code VarroaGlobalVariable} the same way
 * docker-workflow loads {@code docker.build}. Every method reaches the
 * mite only through a registered step; {@code getLock} is the one
 * exception, a plain in-memory read of the run's own active-lock stack.
 */
class Varroa implements Serializable {
    private static final long serialVersionUID = 1L

    private final script

    Varroa(script) {
        this.script = script
    }

    /** Triggers a job on another tenant, or the caller's own, and returns a run handle. */
    def build(Map args = [:]) {
        def raw = script.varroaBuild(args)
        return new VarroaRunHandle(raw, script)
    }

    /** Reads the last successful run of a job, or null when it has none. */
    def lastSuccessful(Map args = [:]) {
        def raw = script.varroaLastSuccessful(args)
        return raw == null ? null : new VarroaRunHandle(raw, script)
    }

    /**
     * Acquires a fleet lock, runs the body, and releases the lock in a
     * finally. The step receives a closure created here that calls the
     * caller's body, never the caller's closure itself: a nested
     * {@code varroa.lock} block resumes correctly only through this
     * wrapper, the same shape docker-workflow uses for {@code inside}.
     */
    def lock(Map args = [:], Closure body) {
        return script.varroaLock(args) { body() }
    }

    /** Attaches one summary line to this run, cut at 512 bytes. */
    def summary(String text) {
        script.varroaSummary(text)
    }

    /**
     * Backs the read-only property {@code varroa.lock.token}: {@code
     * varroa.lock} is always a map carrying {@code name} and {@code
     * token}, with both null with no lock held (D11), so {@code .token}
     * never throws and reads the innermost active lock's fence token.
     */
    def getLock() {
        def current = org.jenkinsci.plugins.varroamiteauth.VarroaCrossTenantBridge.currentLockToken(script)
        return current != null ? current : [name: null, token: null]
    }
}
