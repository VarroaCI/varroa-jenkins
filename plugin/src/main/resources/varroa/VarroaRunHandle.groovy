package varroa

/**
 * CPS class returned by {@code varroa.build} and {@code
 * varroa.lastSuccessful} (D1). Holds only plain values, so a CPS program
 * save can serialize it. The target run lives in another JVM, often in
 * another cluster, so this replaces a {@code RunWrapper}: no local {@code
 * Run} object exists for it to wrap.
 */
class VarroaRunHandle implements Serializable {
    private static final long serialVersionUID = 1L

    final String runId
    final String tenant
    final String job
    final Integer buildNumber
    final String state
    final String result
    final String startedAt
    final String endedAt
    final String url
    final List artifacts
    final String intentKey

    private final script

    VarroaRunHandle(Map fields, script) {
        this.runId = fields.runId ?: ''
        this.tenant = fields.tenant ?: ''
        this.job = fields.job ?: ''
        this.buildNumber = fields.buildNumber
        this.state = fields.state ?: 'pending'
        this.result = fields.result
        this.startedAt = fields.startedAt
        this.endedAt = fields.endedAt
        this.url = fields.url ?: ''
        this.artifacts = fields.artifacts ?: []
        this.intentKey = fields.intentKey ?: ''
        this.script = script
    }

    /**
     * Re-reads the record and returns a new handle (D1). The bridge names
     * the running pipeline's own job as the caller: a script can build a
     * handle from any map, so the handle holds no caller identity.
     */
    def refresh() {
        def raw = org.jenkinsci.plugins.varroamiteauth.VarroaCrossTenantBridge.refresh(
                tenant, job, runId, intentKey)
        return new VarroaRunHandle(raw, script)
    }

    /** Copies one artifact from the target run onto the caller's workspace (D12). */
    def copyArtifact(Map args = [:]) {
        return script.varroaArtifact(runId: runId, path: args.path, target: args.target ?: '')
    }
}
