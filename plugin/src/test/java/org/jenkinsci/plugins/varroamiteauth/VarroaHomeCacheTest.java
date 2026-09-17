package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.util.Map;
import java.util.OptionalInt;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Tests for {@link VarroaHomeCache}: the two fallbacks, the standby
 * answer, a failed refresh, and the interval override.
 */
public class VarroaHomeCacheTest {

    private final HomeForwardHarness harness = new HomeForwardHarness();

    @Before
    public void setUp() throws Throwable {
        harness.before();
    }

    @After
    public void tearDown() {
        harness.after();
    }

    // ---- The two fallbacks ----

    @Test
    public void anEmptyEntryListIsNoOpinion() {
        harness.seedNoOpinion();

        assertFalse("an empty entry list must read as no opinion", harness.cache().hasOpinion());
        assertFalse("no opinion must forward nothing",
                harness.cache().forwardSlotFor("payments/deploy").isPresent());
    }

    @Test
    public void aJobWithNoEntryResolvesToSlotZero() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/build", 0));

        assertEquals("a job with no entry takes the recorder's own fallback",
                0, harness.cache().homeSlotOf("payments/new-job"));
    }

    @Test
    public void aJobEntryNamesItsHomeSlot() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/deploy", 1));

        assertEquals(1, harness.cache().homeSlotOf("payments/deploy"));
        assertTrue("slot 0 must forward a job homed on slot 1",
                harness.cache().forwardSlotFor("payments/deploy").isPresent());
        assertEquals(1, harness.cache().forwardSlotFor("payments/deploy").getAsInt());
    }

    // ---- The standby ----

    @Test
    public void aStandbyHoldsNoSlotAndForwardsEverySubmission() {
        harness.seedHomes("payments", 0, "standby", Map.of("payments/build", 0));

        assertEquals(OptionalInt.empty(), harness.cache().heldSlotFor("payments"));
        assertTrue("a standby forwards a job homed on slot 0",
                harness.cache().forwardSlotFor("payments/build").isPresent());
        assertTrue("a standby forwards a job with no entry",
                harness.cache().forwardSlotFor("payments/other").isPresent());
    }

    @Test
    public void aJobOfAnUnhostedTenantIsNotForwarded() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/build", 0));

        assertFalse("a job outside every hosted tenant carries no home here",
                harness.cache().forwardSlotFor("admin-job").isPresent());
    }

    // ---- Refresh ----

    @Test
    public void aReplyWithNoEntryListKeepsThePreviousMap() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/deploy", 1));

        // The route always carries an entry list, so a body without one is
        // undecodable, never an empty map.
        harness.stub().setHomesBody("{\"ok\":true}");
        harness.cache().refresh();

        assertTrue("a reply with no entry list must keep the previous answer",
                harness.cache().hasOpinion());
        assertEquals(1, harness.cache().homeSlotOf("payments/deploy"));
    }

    @Test
    public void anEntriesValueThatIsNotAListKeepsThePreviousMap() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/deploy", 1));

        harness.stub().setHomesBody("{\"entries\":\"none\"}");
        harness.cache().refresh();

        assertTrue("an entries value that is not a list must keep the previous answer",
                harness.cache().hasOpinion());
        assertEquals(1, harness.cache().homeSlotOf("payments/deploy"));
    }

    @Test
    public void anEmptyEntryListClearsTheOpinion() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/deploy", 1));
        assertTrue(harness.cache().hasOpinion());

        harness.seedNoOpinion();

        assertFalse("an empty entry list is the one legitimate no opinion",
                harness.cache().hasOpinion());
        assertFalse("no opinion must forward nothing",
                harness.cache().forwardSlotFor("payments/deploy").isPresent());
    }

    @Test
    public void aFailedRefreshKeepsThePreviousMap() {
        harness.seedHomes("payments", 0, "active", Map.of("payments/deploy", 1));
        harness.stub().stop();

        harness.cache().refresh();

        assertTrue("a failed refresh must keep the previous answer", harness.cache().hasOpinion());
        assertEquals(1, harness.cache().homeSlotOf("payments/deploy"));
    }

    // ---- The interval ----

    @Test
    public void theEnvironmentVariableSetsTheInterval() {
        assertEquals(VarroaHomeCache.DEFAULT_POLL_MILLIS, VarroaHomeCache.pollMillis(null));
        assertEquals(VarroaHomeCache.DEFAULT_POLL_MILLIS, VarroaHomeCache.pollMillis(""));
        assertEquals(VarroaHomeCache.DEFAULT_POLL_MILLIS, VarroaHomeCache.pollMillis("not-a-number"));
        assertEquals(VarroaHomeCache.DEFAULT_POLL_MILLIS, VarroaHomeCache.pollMillis("0"));
        assertEquals(5000L, VarroaHomeCache.pollMillis("5000"));
    }

    // ---- Path helpers ----

    @Test
    public void thePathHelpersSplitOnTheTenantFolder() {
        assertEquals("payments", VarroaHomeCache.tenantOf("payments/api/deploy"));
        assertEquals("api/deploy", VarroaHomeCache.pathBelowTenant("payments/api/deploy"));
        assertEquals("solo", VarroaHomeCache.tenantOf("solo"));
        assertEquals("", VarroaHomeCache.tenantOf(""));
    }
}
