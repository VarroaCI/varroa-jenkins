package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.Extension;
import hudson.model.TaskListener;
import hudson.scm.SCM;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.UnaryOperator;

import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMHeadEvent;
import jenkins.scm.api.SCMHeadObserver;
import jenkins.scm.api.SCMProbeStat;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;
import jenkins.scm.api.SCMSourceCriteria;
import jenkins.scm.api.SCMSourceDescriptor;

/**
 * A minimal in-memory {@link SCMSource} test double for {@code
 * VarroaMarkerFlowDefinitionTest} (task 5.12): one branch head, a
 * revision store keyed by an opaque revision id, a settable tip, and an
 * overridable {@link #getTrustedRevision} for the trust tests (5.12e).
 * {@link #setLightweightSupported} toggles whether {@link TestFileSystemBuilder}
 * answers this source, exercising D8's checkout fallback (5.12c).
 */
final class TestScmSource extends SCMSource {

    static final SCMHead HEAD = new SCMHead("main");

    // Every field below is test-fixture state, never round-tripped through
    // config.xml, and some hold values (a lambda, an AtomicBoolean, a
    // ConcurrentHashMap key set) XStream cannot marshal. All transient:
    // Jenkins persists this SCMSource's owning project during the test,
    // and a config-XML save must not try to serialize any of it.
    private final transient Map<String, Map<String, byte[]>> revisions = new LinkedHashMap<>();
    private transient volatile String tipRevisionId;
    private transient volatile boolean noTip;
    private transient volatile boolean lightweightSupported = true;
    private transient volatile UnaryOperator<String> trustOverride;
    private final transient Set<String> poisonedRevisions = ConcurrentHashMap.newKeySet();
    private final transient AtomicBoolean checkedOut = new AtomicBoolean(false);

    TestScmSource(String id) {
        super(id);
    }

    /** Commits {@code content} at {@code path} into revision {@code revisionId} (the "commit a marker" helper). */
    void commit(String revisionId, String path, String content) {
        revisions.computeIfAbsent(revisionId, k -> new LinkedHashMap<>())
                .put(path, content.getBytes(StandardCharsets.UTF_8));
    }

    void setTip(String revisionId) {
        this.tipRevisionId = revisionId;
    }

    void setNoTip(boolean noTip) {
        this.noTip = noTip;
    }

    void setLightweightSupported(boolean lightweightSupported) {
        this.lightweightSupported = lightweightSupported;
    }

    boolean isLightweightSupported() {
        return lightweightSupported;
    }

    /** Overrides {@link #getTrustedRevision}: maps a revision id to the trusted one (5.12e). {@code null} restores identity. */
    void setTrustOverride(UnaryOperator<String> trustOverride) {
        this.trustOverride = trustOverride;
    }

    Map<String, byte[]> filesAt(String revisionId) {
        return revisions.getOrDefault(revisionId, Map.of());
    }

    /** Marks {@code revisionId}'s marker read as throwing an unchecked exception (5.12e's "tip read that throws"). */
    void setThrowsOnRead(String revisionId) {
        poisonedRevisions.add(revisionId);
    }

    boolean isPoisoned(String revisionId) {
        return poisonedRevisions.contains(revisionId);
    }

    void markCheckedOut() {
        checkedOut.set(true);
    }

    boolean wasCheckedOut() {
        return checkedOut.get();
    }

    private TestRevision revisionFor(String id) {
        return id == null ? null : new TestRevision(HEAD, id);
    }

    @Override
    protected void retrieve(SCMSourceCriteria criteria, SCMHeadObserver observer, SCMHeadEvent<?> event,
            TaskListener listener) throws IOException, InterruptedException {
        if (tipRevisionId == null) {
            return;
        }
        if (criteria == null || criteria.isHead(new TestProbe(tipRevisionId), listener)) {
            observer.observe(HEAD, revisionFor(tipRevisionId));
        }
    }

    @Override
    protected SCMRevision retrieve(SCMHead head, TaskListener listener) {
        return noTip ? null : revisionFor(tipRevisionId);
    }

    @Override
    public SCMRevision getTrustedRevision(SCMRevision revision, TaskListener listener) {
        UnaryOperator<String> override = trustOverride;
        if (override == null) {
            return revision;
        }
        String id = revision instanceof TestRevision ? ((TestRevision) revision).id : null;
        return revisionFor(override.apply(id));
    }

    @Override
    public SCM build(SCMHead head, SCMRevision revision) {
        String id = revision instanceof TestRevision ? ((TestRevision) revision).id : null;
        return new TestScm(this, id);
    }

    final class TestProbe extends SCMSourceCriteria.Probe {
        private final String revisionId;

        TestProbe(String revisionId) {
            this.revisionId = revisionId;
        }

        @Override
        public String name() {
            return revisionId;
        }

        @Override
        public long lastModified() {
            return 0;
        }

        @Override
        public boolean exists(String path) {
            return filesAt(revisionId).containsKey(path);
        }

        @Override
        public SCMProbeStat stat(String path) {
            return exists(path) ? SCMProbeStat.fromType(jenkins.scm.api.SCMFile.Type.REGULAR_FILE)
                    : SCMProbeStat.fromType(jenkins.scm.api.SCMFile.Type.NONEXISTENT);
        }
    }

    static final class TestRevision extends SCMRevision {
        final String id;

        TestRevision(SCMHead head, String id) {
            super(head);
            this.id = id;
        }

        @Override
        public boolean equals(Object o) {
            return o instanceof TestRevision && ((TestRevision) o).id.equals(id);
        }

        @Override
        public int hashCode() {
            return id.hashCode();
        }

        @Override
        public String toString() {
            return id;
        }
    }

    @Extension
    public static final class DescriptorImpl extends SCMSourceDescriptor {
        @Override
        public String getDisplayName() {
            return "Test SCM Source";
        }
    }
}
