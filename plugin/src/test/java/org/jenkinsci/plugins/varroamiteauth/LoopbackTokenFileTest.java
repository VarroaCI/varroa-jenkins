package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/** Tests for {@link LoopbackTokenFile} — plain JUnit, no JenkinsRule. */
public class LoopbackTokenFileTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private long clockMillis = 10_000L;

    private void writeToken(Path path, String value) throws IOException {
        Files.writeString(path, value, StandardCharsets.UTF_8);
    }

    @Test
    public void cachesTheValueAfterTheFirstRead() throws Exception {
        Path file = tmp.newFile("loopback-token").toPath();
        writeToken(file, "token-a\n");

        LoopbackTokenFile reader = new LoopbackTokenFile(file, () -> clockMillis);
        assertEquals("token-a", reader.token());
        assertEquals(1, reader.readCountForTesting());

        // Change the file; a plain token() call must keep serving the cache.
        writeToken(file, "token-b\n");
        assertEquals("token-a", reader.token());
        assertEquals(1, reader.readCountForTesting());
    }

    @Test
    public void reReadsAfterA401() throws Exception {
        Path file = tmp.newFile("loopback-token").toPath();
        writeToken(file, "token-a\n");

        LoopbackTokenFile reader = new LoopbackTokenFile(file, () -> clockMillis);
        assertEquals("token-a", reader.token());

        writeToken(file, "token-b\n");
        clockMillis += 2000L; // past the once-per-second bound
        assertEquals("token-b", reader.onUnauthorized());
        assertEquals(2, reader.readCountForTesting());
    }

    @Test
    public void honorsTheOncePerSecondBound() throws Exception {
        Path file = tmp.newFile("loopback-token").toPath();
        writeToken(file, "token-a\n");

        LoopbackTokenFile reader = new LoopbackTokenFile(file, () -> clockMillis);
        assertEquals("token-a", reader.token());
        assertEquals(1, reader.readCountForTesting());

        writeToken(file, "token-b\n");
        clockMillis += 500L; // inside the one-second bound
        assertEquals("token-a", reader.onUnauthorized());
        assertEquals(1, reader.readCountForTesting());

        clockMillis += 600L; // now past the bound (1100ms since the first read)
        assertEquals("token-b", reader.onUnauthorized());
        assertEquals(2, reader.readCountForTesting());
    }

    @Test
    public void missingFileReturnsNull() {
        Path missing = tmp.getRoot().toPath().resolve("does-not-exist");
        LoopbackTokenFile reader = new LoopbackTokenFile(missing, () -> clockMillis);
        assertNull(reader.token());
    }
}
