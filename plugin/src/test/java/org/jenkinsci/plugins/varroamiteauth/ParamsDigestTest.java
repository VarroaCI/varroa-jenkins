package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.security.MessageDigest;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Set;
import org.junit.Test;

/** Tests for {@link IntentKey#paramsDigest(Map, Set)} — the canonical digest rule. */
public class ParamsDigestTest {

    private static String expectedHex(String canonical) throws Exception {
        MessageDigest md = MessageDigest.getInstance("SHA-256");
        byte[] hash = md.digest(canonical.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        StringBuilder hex = new StringBuilder();
        for (byte b : hash) {
            hex.append(String.format("%02x", b & 0xff));
        }
        return hex.toString();
    }

    @Test
    public void namesAreSortedAndJoinedWithNewline() throws Exception {
        Map<String, String> values = new LinkedHashMap<>();
        values.put("zeta", "2");
        values.put("alpha", "1");

        String digest = IntentKey.paramsDigest(values, Set.of());
        assertEquals(expectedHex("alpha=1\nzeta=2"), digest);
    }

    @Test
    public void aSensitiveParameterContributesAMaskedValue() throws Exception {
        Map<String, String> values = new LinkedHashMap<>();
        values.put("password", "hunter2");
        values.put("branch", "main");

        String digest = IntentKey.paramsDigest(values, Set.of("password"));
        assertEquals(expectedHex("branch=main\npassword=<masked>"), digest);
        // The digest never carries the secret value in plain sight.
        assertFalse(digest.contains("hunter2"));
    }

    @Test
    public void emptyParametersDigestTheEmptyString() throws Exception {
        String digest = IntentKey.paramsDigest(Map.of(), Set.of());
        assertEquals(expectedHex(""), digest);
    }

    @Test
    public void sameParametersInDifferentInsertionOrderDigestTheSame() {
        Map<String, String> a = new LinkedHashMap<>();
        a.put("b", "2");
        a.put("a", "1");
        Map<String, String> b = new LinkedHashMap<>();
        b.put("a", "1");
        b.put("b", "2");

        assertEquals(IntentKey.paramsDigest(a, Set.of()), IntentKey.paramsDigest(b, Set.of()));
    }
}
