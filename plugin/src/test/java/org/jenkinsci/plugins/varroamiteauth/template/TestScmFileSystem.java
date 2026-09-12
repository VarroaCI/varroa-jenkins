package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.Extension;
import hudson.scm.SCM;
import hudson.scm.SCMDescriptor;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.util.Map;

import jenkins.scm.api.SCMFile;
import jenkins.scm.api.SCMFileSystem;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;
import jenkins.scm.api.SCMSourceDescriptor;

/**
 * The lightweight {@link SCMFileSystem} for {@link TestScmSource} (task
 * 5.12): backed directly by the source's in-memory revision map, with no
 * checkout. {@link TestScmSource#setLightweightSupported} toggling to
 * {@code false} makes {@link TestFileSystemBuilder} refuse this source,
 * exercising D8's checkout fallback (5.12c).
 */
final class TestScmFileSystem extends SCMFileSystem {

    private final TestScmSource source;
    private final String revisionId;

    TestScmFileSystem(TestScmSource source, SCMRevision revision, String revisionId) {
        super(revision);
        this.source = source;
        this.revisionId = revisionId;
    }

    @Override
    public long lastModified() {
        return 0;
    }

    @Override
    public SCMFile getRoot() {
        return new TestScmFile(source, revisionId);
    }

    /** One node of the in-memory tree, resolved against {@link TestScmSource#filesAt}. */
    static final class TestScmFile extends SCMFile {

        private final TestScmSource source;
        private final String revisionId;

        TestScmFile(TestScmSource source, String revisionId) {
            this.source = source;
            this.revisionId = revisionId;
        }

        private TestScmFile(TestScmFile parent, String name, TestScmSource source, String revisionId) {
            super(parent, name);
            this.source = source;
            this.revisionId = revisionId;
        }

        @Override
        protected SCMFile newChild(String name, boolean assumeIsDirectory) {
            return new TestScmFile(this, name, source, revisionId);
        }

        @Override
        public Iterable<SCMFile> children() {
            throw new UnsupportedOperationException("not needed: the flow definition only reads one fixed path");
        }

        @Override
        public long lastModified() {
            return 0;
        }

        @Override
        protected Type type() {
            Map<String, byte[]> files = source.filesAt(revisionId);
            String path = getPath();
            if (files.containsKey(path)) {
                return Type.REGULAR_FILE;
            }
            for (String key : files.keySet()) {
                if (key.startsWith(path + "/")) {
                    return Type.DIRECTORY;
                }
            }
            return Type.NONEXISTENT;
        }

        @Override
        public InputStream content() throws IOException {
            if (source.isPoisoned(revisionId)) {
                // 5.12e: a tip read that throws is a log line only, never a
                // build failure. An unchecked exception here mirrors a
                // real SCM implementation's runtime failure.
                throw new RuntimeException("simulated read failure for revision " + revisionId);
            }
            byte[] content = source.filesAt(revisionId).get(getPath());
            if (content == null) {
                throw new IOException("no such file: " + getPath());
            }
            return new ByteArrayInputStream(content);
        }
    }

    @Extension
    public static final class TestFileSystemBuilder extends Builder {
        @Override
        public boolean supports(SCM scm) {
            return false;
        }

        @Override
        public boolean supports(SCMSource source) {
            return source instanceof TestScmSource && ((TestScmSource) source).isLightweightSupported();
        }

        @Override
        protected boolean supportsDescriptor(SCMDescriptor descriptor) {
            return false;
        }

        @Override
        protected boolean supportsDescriptor(SCMSourceDescriptor descriptor) {
            return descriptor instanceof TestScmSource.DescriptorImpl;
        }

        @Override
        public SCMFileSystem build(SCMSource source, SCMHead head, SCMRevision revision) {
            if (!(source instanceof TestScmSource) || !(revision instanceof TestScmSource.TestRevision)) {
                return null;
            }
            String revisionId = ((TestScmSource.TestRevision) revision).id;
            return new TestScmFileSystem((TestScmSource) source, revision, revisionId);
        }
    }
}
