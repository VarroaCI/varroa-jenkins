package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.List;
import java.util.stream.Stream;

import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/** Tests for {@link RunJournal}'s file handling — plain JUnit, no JenkinsRule. */
public class RunJournalTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private Path dir;
    private RunJournal journal;

    @Before
    public void setUp() throws Exception {
        dir = tmp.newFolder("live").toPath();
        journal = new RunJournal(dir);
    }

    private static RunJournal.Entry entry(String runId) {
        return new RunJournal.Entry(runId, "intent-1", "team/job", 12, "delivery-1", 1_000L, false);
    }

    @Test
    public void writeThenReadRoundTrips() {
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent-1", "team/job", 12, "delivery-1", 1_000L, true));

        List<RunJournal.Entry> entries = journal.readAll();
        assertEquals(1, entries.size());
        RunJournal.Entry read = entries.get(0);
        assertEquals(runId, read.runId);
        assertEquals("intent-1", read.intentKey);
        assertEquals("team/job", read.jobPath);
        assertEquals(12, read.buildNumber);
        assertEquals("delivery-1", read.deliveryId);
        assertEquals(1_000L, read.startedAtMillis);
        assertTrue(read.pipeline);
        assertTrue("a read entry carries its file time", read.lastSeenMillis > 0);
    }

    @Test
    public void readReturnsTheEntryOfOneRun() {
        String runId = Uuid7.next();
        journal.write(entry(runId));
        journal.write(entry(Uuid7.next()));

        RunJournal.Entry read = journal.read(runId);
        assertNotNull(read);
        assertEquals(runId, read.runId);
        assertEquals("team/job", read.jobPath);
        assertTrue(read.lastSeenMillis > 0);
    }

    @Test
    public void readAnswersNullForAMissingRun() {
        assertNull(journal.read(Uuid7.next()));
        assertNull("an id that is not a UUID names no file", journal.read("../escape"));
    }

    @Test
    public void writeIsAtomicNoTmpLeft() throws Exception {
        journal.write(entry(Uuid7.next()));
        journal.write(entry(Uuid7.next()));

        try (Stream<Path> files = Files.list(dir)) {
            assertTrue(files.noneMatch(p -> p.getFileName().toString().endsWith(".tmp")));
        }
        assertEquals(2, journal.readAll().size());
    }

    @Test
    public void removeIsIdempotent() {
        String runId = Uuid7.next();
        journal.write(entry(runId));

        journal.remove(runId);
        journal.remove(runId);

        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void corruptFileIsSkippedAndDeleted() throws Exception {
        String good = Uuid7.next();
        journal.write(entry(good));
        Path corrupt = dir.resolve(Uuid7.next() + ".json");
        Files.write(corrupt, "{\"run_id\":".getBytes(StandardCharsets.UTF_8));

        List<RunJournal.Entry> entries = journal.readAll();

        assertEquals(1, entries.size());
        assertEquals(good, entries.get(0).runId);
        assertFalse("a corrupt entry is deleted", Files.exists(corrupt));
    }

    @Test
    public void entryUnderAnotherFileNameIsDeleted() throws Exception {
        String runId = Uuid7.next();
        journal.write(entry(runId));
        Path renamed = dir.resolve(Uuid7.next() + ".json");
        Files.move(dir.resolve(runId + ".json"), renamed);

        assertTrue(journal.readAll().isEmpty());
        assertFalse(Files.exists(renamed));
    }

    @Test
    public void rejectsNonUuidRunId() throws Exception {
        journal.write(entry("../escape"));
        journal.write(entry("run-a"));
        journal.write(entry(null));

        try (Stream<Path> files = Files.list(dir)) {
            assertEquals(0, files.count());
        }
        assertFalse(Files.exists(dir.getParent().resolve("escape.json")));
    }

    @Test
    public void touchAdvancesMtime() throws Exception {
        String runId = Uuid7.next();
        journal.write(entry(runId));
        Path file = dir.resolve(runId + ".json");
        Files.setLastModifiedTime(file, FileTime.fromMillis(5_000L));

        journal.touch(runId);

        assertTrue(Files.getLastModifiedTime(file).toMillis() > 5_000L);
    }

    @Test
    public void touchCreatesNoEntry() throws Exception {
        journal.touch(Uuid7.next());

        try (Stream<Path> files = Files.list(dir)) {
            assertEquals(0, files.count());
        }
    }

    @Test
    public void readOfMissingDirectoryIsEmpty() {
        RunJournal absent = new RunJournal(dir.resolve("absent"));

        assertTrue(absent.readAll().isEmpty());
    }
}
