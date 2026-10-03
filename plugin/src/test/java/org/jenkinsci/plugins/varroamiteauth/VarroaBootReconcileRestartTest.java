package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.Result;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.stream.Stream;

import org.apache.commons.io.FileUtils;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsSessionRule;

/**
 * Proves the boot reconcile on a real second Jenkins boot over the same
 * Jenkins home. The first session leaves the disk state a stopped
 * process leaves: a build record with no result, and a journal entry,
 * plus a completed build whose completion never left the sink.
 */
public class VarroaBootReconcileRestartTest {

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @After
    public void clearGateOverride() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
    }

    @Test
    public void aLostFreestyleBuildEndsAtTheNextBoot() throws Throwable {
        String lostRunId = Uuid7.next();
        String[] doneRunId = new String[1];
        sessions.then(j -> {
            VarroaRunRecorder.setGateOverrideForTesting("1");
            FreeStyleProject project = j.createFreeStyleProject("lost");
            VarroaIntentAction action = new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis());
            FreeStyleBuild done = j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));
            doneRunId[0] = action.getRunId();
            // No dispatcher runs here, so build 1's completion stays in the
            // sink and its entry stays: the stop loses that completion too.
            assertEquals(1, new RunJournal().readAll().size());

            // Build 2 stopped mid-run: its record holds no result and no
            // duration, and its journal entry stays.
            Path lostDir = done.getRootDir().toPath().resolveSibling("2");
            FileUtils.copyDirectory(done.getRootDir(), lostDir.toFile());
            Path xml = lostDir.resolve("build.xml");
            String record = Files.readString(xml, StandardCharsets.UTF_8)
                    .replaceAll("<result>[^<]*</result>", "")
                    .replaceAll("<duration>[^<]*</duration>", "")
                    .replace(action.getRunId(), lostRunId);
            Files.writeString(xml, record, StandardCharsets.UTF_8);
            project.updateNextBuildNumber(3);
            new RunJournal().write(new RunJournal.Entry(lostRunId, "intent-lost", "lost", 2, "",
                    done.getStartTimeInMillis(), false));
        });
        sessions.then(j -> {
            assertTrue("the boot reconcile deletes the entry", new RunJournal().readAll().isEmpty());

            FreeStyleProject project = j.jenkins.getItemByFullName("lost", FreeStyleProject.class);
            FreeStyleBuild lost = project.getBuildByNumber(2);
            assertNotNull(lost);
            assertFalse(lost.isBuilding());
            assertEquals(Result.ABORTED, lost.getResult());

            Path runSpool = j.jenkins.getRootDir().toPath().resolve("varroa").resolve("spool").resolve(lostRunId);
            List<RunSpool.Entry> spooled = new RunSpool(runSpool.getParent(), RunSpool.DEFAULT_CAP_BYTES)
                    .readOrdered(lostRunId);
            assertEquals(1, spooled.size());
            RunSpool.Entry completed = spooled.get(0);
            assertEquals(BuildEvent.PHASE_COMPLETED, completed.phase);
            assertEquals("ABORTED", completed.toJsonObject().getString("result"));
            assertEquals("intent-lost", completed.toJsonObject().getString("intent_key"));
            assertEquals(2, completed.toJsonObject().getLong("build_number"));
            try (Stream<Path> files = Files.list(runSpool)) {
                assertEquals(1, files.count());
            }

            List<RunSpool.Entry> done = new RunSpool(runSpool.getParent(), RunSpool.DEFAULT_CAP_BYTES)
                    .readOrdered(doneRunId[0]);
            assertEquals("a completion lost in the sink is sent again", 1, done.size());
            assertEquals("SUCCESS", done.get(0).toJsonObject().getString("result"));
        });
    }
}
