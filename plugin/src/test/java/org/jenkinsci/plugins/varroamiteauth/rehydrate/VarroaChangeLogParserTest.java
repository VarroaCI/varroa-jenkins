package org.jenkinsci.plugins.varroamiteauth.rehydrate;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import java.io.File;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

import hudson.model.User;
import hudson.scm.ChangeLogSet;

/**
 * Tests for {@link VarroaChangeLogParser} and {@link VarroaChangeLogSet}.
 */
public class VarroaChangeLogParserTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private WorkflowRun completedRun() throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition("echo 'hello'", true));
        return j.buildAndAssertSuccess(p);
    }

    private File changelogFile(String xml) throws Exception {
        File f = tmp.newFile("changelog0.xml");
        Files.write(f.toPath(), xml.getBytes(StandardCharsets.UTF_8));
        return f;
    }

    @Test
    public void parsesATwoEntryDocument() throws Exception {
        User ada = User.getById("ada", true);
        ada.setFullName("Ada Lovelace");

        String xml = "<varroa-changelog>"
                + "<entry><commitId>abc123</commitId><author>ada</author><authorEmail></authorEmail>"
                + "<timestamp>1700000000000</timestamp><msg>fix the queue lock</msg></entry>"
                + "<entry><commitId>def456</commitId><author>grace</author><authorEmail></authorEmail>"
                + "<timestamp>1700000100000</timestamp><msg>add a test</msg></entry>"
                + "</varroa-changelog>";

        VarroaChangeLogParser parser = new VarroaChangeLogParser();
        ChangeLogSet<? extends ChangeLogSet.Entry> set = parser.parse(completedRun(), null, changelogFile(xml));

        assertFalse("a two-entry document must not parse as empty", set.isEmptySet());
        List<ChangeLogSet.Entry> entries = new ArrayList<>();
        for (ChangeLogSet.Entry e : set) {
            entries.add(e);
        }
        assertEquals(2, entries.size());

        ChangeLogSet.Entry first = entries.get(0);
        assertEquals("abc123", first.getCommitId());
        assertEquals("fix the queue lock", first.getMsg());
        assertEquals(1700000000000L, first.getTimestamp());
        assertNotNull(first.getAuthor());
        assertEquals("ada", first.getAuthor().getId());
        assertTrue("a rehydrated entry names no affected path",
                first.getAffectedPaths().isEmpty());
    }

    @Test
    public void anAuthorNoJenkinsUserMatchesFallsBackToUnknown() throws Exception {
        String xml = "<varroa-changelog>"
                + "<entry><commitId>abc123</commitId><author>nobody-such-user</author>"
                + "<authorEmail></authorEmail><timestamp>1700000000000</timestamp>"
                + "<msg>a commit from a user Jenkins never saw</msg></entry>"
                + "</varroa-changelog>";

        VarroaChangeLogParser parser = new VarroaChangeLogParser();
        ChangeLogSet<? extends ChangeLogSet.Entry> set = parser.parse(completedRun(), null, changelogFile(xml));

        ChangeLogSet.Entry entry = set.iterator().next();
        assertEquals(User.getUnknown().getId(), entry.getAuthor().getId());
    }

    @Test
    public void aMissingChangelogFileParsesAsAnEmptySet() throws Exception {
        VarroaChangeLogParser parser = new VarroaChangeLogParser();
        File missing = new File(tmp.getRoot(), "does-not-exist.xml");

        ChangeLogSet<? extends ChangeLogSet.Entry> set = parser.parse(completedRun(), null, missing);

        assertTrue("a missing changelog file must parse as empty, not fail", set.isEmptySet());
    }
}
