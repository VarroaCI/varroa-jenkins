package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/** Tests for {@link RunSpool} — plain JUnit, no JenkinsRule. */
public class RunSpoolTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static BuildEvent event(long seq, String phase) {
        return BuildEvent.builder().seq(seq).phase(phase).runId("run-1").build();
    }

    /** Serialized byte size of one minimal event, used to size caps deterministically. */
    private static long oneEventBytes() {
        return event(0, BuildEvent.PHASE_STARTED).toJson().toString()
                .getBytes(java.nio.charset.StandardCharsets.UTF_8).length;
    }

    @Test
    public void deleteRunIfEmptyIgnoresAStaleTemporaryFile() throws Exception {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_COMPLETED));
        Path dir = tmp.getRoot().toPath().resolve("run-1");
        Path stale = dir.resolve(".abandoned.tmp");
        Files.write(stale, new byte[] {1, 2, 3});

        spool.acknowledge("run-1", 1);
        spool.deleteRunIfEmpty("run-1");
        assertTrue("an unacked event keeps the directory", Files.isDirectory(dir));

        spool.acknowledge("run-1", 0);
        spool.deleteRunIfEmpty("run-1");
        assertFalse("a stale temporary file must not keep the directory", Files.exists(dir));
        assertFalse(Files.exists(stale));
    }

    @Test
    public void appendWritesOneFilePerEvent() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_STAGE_STARTED));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        assertEquals(2, entries.size());
    }

    @Test
    public void heartbeatIsNeverWritten() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_HEARTBEAT));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        assertTrue(entries.isEmpty());
    }

    @Test
    public void readOrderedReturnsAscendingSequence() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(2, BuildEvent.PHASE_STAGE));
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_STAGE_STARTED));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        assertEquals(3, entries.size());
        assertEquals(0, entries.get(0).seq);
        assertEquals(1, entries.get(1).seq);
        assertEquals(2, entries.get(2).seq);
    }

    @Test
    public void capEvictionDropsTheOldestNonTerminalFile() throws Exception {
        // Cap fits two events but not three, forcing eviction on the third.
        long cap = (long) (oneEventBytes() * 2.5);
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), cap);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_STAGE_STARTED));
        spool.append("run-1", event(2, BuildEvent.PHASE_STAGE));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        // The oldest (seq 0) was evicted to make room; the newest write
        // (seq 2) always survives its own append.
        assertTrue(entries.stream().noneMatch(e -> e.seq == 0));
        assertTrue(entries.stream().anyMatch(e -> e.seq == 2));
        assertTrue(spool.droppedCountForTesting() >= 1);
    }

    @Test
    public void completedFileIsNeverEvicted() {
        long cap = (long) (oneEventBytes() * 1.5);
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), cap);
        spool.append("run-1", event(0, BuildEvent.PHASE_COMPLETED));
        // Force further writes past the cap; the COMPLETED file must
        // survive every eviction pass even though it is the oldest.
        spool.append("run-1", event(1, BuildEvent.PHASE_STAGE));
        spool.append("run-1", event(2, BuildEvent.PHASE_STAGE));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        assertTrue(entries.stream().anyMatch(e -> e.seq == 0 && BuildEvent.PHASE_COMPLETED.equals(e.phase)));
    }

    @Test
    public void acknowledgeDeletesOnlyThatSeq() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_STAGE));

        spool.acknowledge("run-1", 0);

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        assertEquals(1, entries.size());
        assertEquals(1, entries.get(0).seq);
    }

    // ---- Lock table reference counting (defect: deleteRun races a stale lockFor() caller) ----

    @Test
    public void deleteRunNeverRetiresAMonitorAnotherCallerStillHolds() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));

        // Simulate a caller (append(), readOrdered(), ...) that has
        // already resolved run-1's monitor but has not yet entered its
        // synchronized block — the exact window deleteRun's old
        // unconditional removal raced.
        Object outstanding = spool.acquireLockForTesting("run-1");

        spool.deleteRun("run-1");

        // A second, concurrent caller resolving the same run's monitor
        // right now must share the still-outstanding one, never mint a
        // fresh one that lets it run unsynchronized against the first.
        Object second = spool.acquireLockForTesting("run-1");
        assertSame("a concurrent caller must share the outstanding monitor, not a freshly minted one",
                outstanding, second);

        spool.releaseLockForTesting("run-1", second);
        spool.releaseLockForTesting("run-1", outstanding);
    }

    @Test
    public void deleteRunRemovesTheDirectory() throws Exception {
        Path base = tmp.getRoot().toPath();
        RunSpool spool = new RunSpool(base, RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event(1, BuildEvent.PHASE_COMPLETED));

        spool.deleteRun("run-1");

        assertFalse(Files.exists(base.resolve("run-1")));
    }

    @Test
    public void listRunsReturnsRunIdsInAscendingOrder() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        // Not real UUIDv7 values, but the same ascending-string property
        // holds: listRuns sorts, it does not read run creation time.
        spool.append("01960000-run-b", event(0, BuildEvent.PHASE_STARTED));
        spool.append("01950000-run-a", event(0, BuildEvent.PHASE_STARTED));

        List<String> runs = spool.listRuns();

        assertEquals(List.of("01950000-run-a", "01960000-run-b"), runs);
    }

    @Test
    public void listRunsIsEmptyWithNoSpoolDirectory() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath().resolve("does-not-exist"), RunSpool.DEFAULT_CAP_BYTES);

        assertTrue(spool.listRuns().isEmpty());
    }

    // ---- Concurrency (defect 5) ----

    @Test
    public void concurrentAppendsToOneRunNeverLoseOrCorruptAFile() throws Exception {
        // A cap tight enough that most writes from both threads trigger
        // eviction, so a missing per-run lock would race the directory
        // listing, the size calculation, and the delete against a
        // concurrent writer on the same run.
        long cap = (long) (oneEventBytes() * 5.5);
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), cap);
        int perThread = 150;

        Runnable writerA = () -> {
            for (int i = 0; i < perThread; i++) {
                spool.append("run-1", event(i, BuildEvent.PHASE_STAGE));
            }
        };
        Runnable writerB = () -> {
            for (int i = perThread; i < perThread * 2; i++) {
                spool.append("run-1", event(i, BuildEvent.PHASE_STAGE));
            }
        };
        Thread t1 = new Thread(writerA);
        Thread t2 = new Thread(writerB);
        t1.start();
        t2.start();
        t1.join(20_000L);
        t2.join(20_000L);
        assertFalse("writer thread A must finish", t1.isAlive());
        assertFalse("writer thread B must finish", t2.isAlive());

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");
        // Every surviving file parses cleanly and its body matches the
        // sequence its name carries — proof that no partial write or
        // interleaved eviction corrupted a file.
        for (RunSpool.Entry e : entries) {
            assertEquals(e.seq, e.toJsonObject().getLong("seq"));
        }
        // Eviction kept the run close to the cap, not letting the two
        // threads' writes race past it unbounded.
        assertTrue("expected eviction to keep the run near the cap, got " + entries.size() + " files",
                entries.size() < perThread * 2);
    }

    // ---- Cross-instance locking (defect 2) ----

    @Test
    public void twoInstancesOverOneDirectorySerializeAppendAndReadWithNoParseFailure() throws Exception {
        // VarroaRunRecorder and BuildEventDispatcher each hold their own
        // RunSpool over the same directory. A shared, directory-keyed
        // lock table must serialize them the same way a single instance
        // serializes itself.
        Path base = tmp.getRoot().toPath();
        RunSpool writer = new RunSpool(base, RunSpool.DEFAULT_CAP_BYTES);
        RunSpool reader = new RunSpool(base, RunSpool.DEFAULT_CAP_BYTES);
        int total = 200;

        Thread writerThread = new Thread(() -> {
            for (int i = 0; i < total; i++) {
                writer.append("run-1", event(i, BuildEvent.PHASE_STAGE));
            }
        });
        java.util.concurrent.atomic.AtomicBoolean readerFailed = new java.util.concurrent.atomic.AtomicBoolean(false);
        Thread readerThread = new Thread(() -> {
            for (int i = 0; i < 500; i++) {
                for (RunSpool.Entry e : reader.readOrdered("run-1")) {
                    try {
                        // A torn read from the other instance's
                        // in-progress write would fail here.
                        e.toJsonObject();
                    } catch (RuntimeException ex) {
                        readerFailed.set(true);
                    }
                }
            }
        });
        writerThread.start();
        readerThread.start();
        writerThread.join(20_000L);
        readerThread.join(20_000L);

        assertFalse("writer thread must finish", writerThread.isAlive());
        assertFalse("reader thread must finish", readerThread.isAlive());
        assertFalse("a concurrent read must never observe a partially written file", readerFailed.get());
        assertEquals(total, writer.readOrdered("run-1").size());
    }

    @Test
    public void readOrderedSkipsAnUnparsableFileInsteadOfThrowing() throws Exception {
        Path base = tmp.getRoot().toPath();
        RunSpool spool = new RunSpool(base, RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event(0, BuildEvent.PHASE_STARTED));
        // Simulate a truncated write: a file that carries a valid spool
        // name but a body that is not complete JSON.
        Path truncated = base.resolve("run-1").resolve(String.format(java.util.Locale.ROOT,
                "%020d-%s.json", 1L, BuildEvent.PHASE_STAGE));
        Files.write(truncated, "{\"seq\":1,\"phase\":\"STAGE\"".getBytes(java.nio.charset.StandardCharsets.UTF_8));

        List<RunSpool.Entry> entries = spool.readOrdered("run-1");

        assertEquals(1, entries.size());
        assertEquals(0, entries.get(0).seq);
    }
}
