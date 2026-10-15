package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.security.SecureRandom;
import java.util.HashSet;
import java.util.Set;
import org.junit.Test;

/** Tests for {@link Uuid7} — plain JUnit, no JenkinsRule. */
public class Uuid7Test {

    @Test
    public void carriesVersionAndVariantBits() {
        String id = Uuid7.next();
        assertEquals(36, id.length());
        assertEquals('-', id.charAt(8));
        assertEquals('-', id.charAt(13));
        assertEquals('-', id.charAt(18));
        assertEquals('-', id.charAt(23));
        assertEquals('7', id.charAt(14));
        char variantNibble = id.charAt(19);
        assertTrue("variant nibble should be 8-b, was " + variantNibble,
                variantNibble == '8' || variantNibble == '9' || variantNibble == 'a' || variantNibble == 'b');
    }

    @Test
    public void encodesTheGivenMillisInTheFirst48Bits() {
        long millis = 0x0123_4567_89ABL; // 48-bit value
        String id = Uuid7.next(millis, new SecureRandom());
        String timeHex = id.substring(0, 8) + id.substring(9, 13);
        assertEquals("0123456789ab", timeHex);
    }

    @Test
    public void isUniqueAcrossManyCalls() {
        Set<String> seen = new HashSet<>();
        for (int i = 0; i < 1000; i++) {
            assertTrue(seen.add(Uuid7.next()));
        }
    }
}
