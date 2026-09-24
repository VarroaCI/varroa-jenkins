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
     * finally. {@code name} is a bare name with no {@code /}. {@code
     * tenant} names the tenant that owns the lock, and an absent {@code
     * tenant} names the caller's own tenant. The step receives a closure created here that calls the
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
     * Backs the read-only properties {@code varroa.lock.name}, {@code
     * varroa.lock.tenant}, and {@code varroa.lock.token}: {@code
     * varroa.lock} is always a map carrying the innermost active lock's
     * bare {@code name}, its owner {@code tenant}, and its fence {@code
     * token}, all null with no lock held (D11), so no read throws.
     */
    def getLock() {
        def current = org.jenkinsci.plugins.varroamiteauth.VarroaCrossTenantBridge.currentLockToken(script)
        return current != null ? current : [name: null, tenant: null, token: null]
    }
}
