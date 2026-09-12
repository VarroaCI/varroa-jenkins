package org.jenkinsci.plugins.varroamiteauth;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.LongSupplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Reads and caches the mite loopback bearer token (D7).
 *
 * <p>The mite writes a fresh token to the shared token file before it opens
 * the loopback listener. This reader caches the value in memory, re-reads
 * the file when {@link #onUnauthorized()} reports a {@code 401}, and never
 * re-reads more than once per second.
 */
public class LoopbackTokenFile {

    private static final Logger LOGGER = Logger.getLogger(LoopbackTokenFile.class.getName());

    /** Path of the shared token file, mounted read-only in the Jenkins container. */
    static final String DEFAULT_PATH = "/var/run/varroa/loopback-token";

    /** The once-per-second bound on a forced re-read. */
    private static final long MIN_REREAD_INTERVAL_MILLIS = 1000L;

    private final Path path;
    private final LongSupplier clock;
    private final AtomicInteger readCount = new AtomicInteger();

    private volatile String cachedToken;
    private volatile long lastReadAtMillis = Long.MIN_VALUE;

    public LoopbackTokenFile() {
        this(Paths.get(DEFAULT_PATH), System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects a path and a clock. */
    LoopbackTokenFile(Path path, LongSupplier clock) {
        this.path = path;
        this.clock = clock;
    }

    /**
     * Returns the cached token, reading the file on first use. Returns
     * {@code null} when the file does not exist yet or cannot be read.
     */
    public String token() {
        if (cachedToken == null) {
            read();
        }
        return cachedToken;
    }

    /**
     * Reports a {@code 401} from the loopback and re-reads the file, unless
     * a read already happened within the last second. Returns the token
     * after the (possibly skipped) re-read.
     */
    public String onUnauthorized() {
        long now = clock.getAsLong();
        if (lastReadAtMillis == Long.MIN_VALUE || now - lastReadAtMillis >= MIN_REREAD_INTERVAL_MILLIS) {
            read();
        }
        return cachedToken;
    }

    private synchronized void read() {
        try {
            String content = Files.readString(path, StandardCharsets.UTF_8);
            cachedToken = content.trim();
        } catch (IOException e) {
            LOGGER.log(Level.FINE, "LoopbackTokenFile: could not read the token file", e);
        }
        lastReadAtMillis = clock.getAsLong();
        readCount.incrementAndGet();
    }

    /** Returns the number of file reads performed. Package-private for testing. */
    int readCountForTesting() {
        return readCount.get();
    }
}
