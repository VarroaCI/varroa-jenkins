package org.jenkinsci.plugins.varroamiteauth.rehydrate;

import java.util.Collection;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;

import hudson.model.Run;
import hudson.model.User;
import hudson.scm.ChangeLogSet;
import hudson.scm.RepositoryBrowser;

/**
 * The changeset {@link VarroaChangeLogParser} builds from a rehydrated
 * run's {@code changelog0.xml} document: one {@link Entry} per recorded
 * commit.
 */
public final class VarroaChangeLogSet extends ChangeLogSet<VarroaChangeLogSet.Entry> {

    private final List<Entry> entries;

    VarroaChangeLogSet(Run<?, ?> run, RepositoryBrowser<?> browser, List<Entry> entries) {
        super(run, browser);
        this.entries = entries;
        for (Entry entry : entries) {
            entry.attachTo(this);
        }
    }

    @Override
    public boolean isEmptySet() {
        return entries.isEmpty();
    }

    @Override
    public Iterator<Entry> iterator() {
        return entries.iterator();
    }

    /**
     * One recorded commit. The record store carries only an
     * affected-path count, never the paths themselves, so
     * {@link #getAffectedPaths()} always answers no path.
     */
    public static final class Entry extends ChangeLogSet.Entry {

        private final String commitId;
        private final String authorId;
        private final String msg;
        private final long timestamp;

        Entry(String commitId, String authorId, String msg, long timestamp) {
            this.commitId = commitId;
            this.authorId = authorId;
            this.msg = msg;
            this.timestamp = timestamp;
        }

        /**
         * Sets this entry's parent set. Called only from
         * {@link VarroaChangeLogSet}'s constructor; a wrapper method is
         * needed because {@link ChangeLogSet.Entry#setParent} is
         * protected, and Java's protected-access rule requires the call
         * to happen inside a subclass body, not merely inside the
         * subclass's enclosing class.
         */
        void attachTo(VarroaChangeLogSet parent) {
            setParent(parent);
        }

        @Override
        public String getCommitId() {
            return commitId;
        }

        @Override
        public long getTimestamp() {
            return timestamp;
        }

        @Override
        public String getMsg() {
            return msg;
        }

        @Override
        public User getAuthor() {
            User user = User.getById(authorId, false);
            return user != null ? user : User.getUnknown();
        }

        @Override
        public Collection<String> getAffectedPaths() {
            return Collections.emptyList();
        }
    }
}
