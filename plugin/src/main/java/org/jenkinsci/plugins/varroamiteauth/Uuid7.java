package org.jenkinsci.plugins.varroamiteauth;

import java.security.SecureRandom;
import java.util.Locale;

/**
 * Mints a version-7 UUID string. The JDK ships no UUIDv7 generator.
 *
 * <p>Layout: 48 bits of Unix epoch milliseconds, 4 version bits, 12 random
 * bits, 2 variant bits, and 62 random bits. Every random bit comes from
 * {@link SecureRandom}.
 */
public final class Uuid7 {

    private static final SecureRandom RANDOM = new SecureRandom();

    private Uuid7() {
    }

    /** Returns a fresh UUIDv7 string, timestamped at the current instant. */
    public static String next() {
        return next(System.currentTimeMillis(), RANDOM);
    }

    /**
     * Returns a UUIDv7 string built from the given epoch millis and random
     * source. Package-private so a test can pin both inputs.
     */
    static String next(long unixMillis, SecureRandom random) {
        byte[] bytes = new byte[16];
        bytes[0] = (byte) (unixMillis >>> 40);
        bytes[1] = (byte) (unixMillis >>> 32);
        bytes[2] = (byte) (unixMillis >>> 24);
        bytes[3] = (byte) (unixMillis >>> 16);
        bytes[4] = (byte) (unixMillis >>> 8);
        bytes[5] = (byte) unixMillis;

        byte[] randomTail = new byte[10];
        random.nextBytes(randomTail);
        System.arraycopy(randomTail, 0, bytes, 6, 10);

        // Version 7 occupies the high nibble of byte 6.
        bytes[6] = (byte) ((bytes[6] & 0x0F) | 0x70);
        // Variant 0b10 occupies the top two bits of byte 8.
        bytes[8] = (byte) ((bytes[8] & 0x3F) | 0x80);

        return format(bytes);
    }

    /**
     * Returns the Unix epoch milliseconds a version-7 UUID string carries,
     * or {@code -1} for a string that is not a version-7 UUID.
     */
    static long timestampMillis(String uuid) {
        if (uuid == null || uuid.length() != 36 || uuid.charAt(14) != '7') {
            return -1L;
        }
        String hex = uuid.substring(0, 8) + uuid.substring(9, 13);
        try {
            return Long.parseLong(hex, 16);
        } catch (NumberFormatException e) {
            return -1L;
        }
    }

    private static String format(byte[] bytes) {
        StringBuilder hex = new StringBuilder(32);
        for (byte b : bytes) {
            hex.append(String.format(Locale.ROOT, "%02x", b & 0xff));
        }
        return hex.substring(0, 8) + "-" + hex.substring(8, 12) + "-"
                + hex.substring(12, 16) + "-" + hex.substring(16, 20) + "-" + hex.substring(20, 32);
    }
}
