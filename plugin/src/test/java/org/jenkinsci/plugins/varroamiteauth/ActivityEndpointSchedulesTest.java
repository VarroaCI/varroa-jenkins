package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.model.FreeStyleProject;
import hudson.model.Job;
import hudson.scheduler.CronTab;
import hudson.scheduler.CronTabList;
import hudson.scheduler.Hash;
import hudson.security.ACL;
import hudson.security.ACLContext;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;
import hudson.triggers.Trigger;
import jenkins.triggers.ReverseBuildTrigger;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.lang.reflect.Field;
import java.time.DayOfWeek;
import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.Calendar;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.TimeZone;

import net.sf.json.JSONObject;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Tests for the job-schedule export in {@link ActivityEndpoint} (D8).
 */
public class ActivityEndpointSchedulesTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    // ---- 8.1: route gate and authorization ----

    @Test
    public void nonMiteCallerGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");

            endpoint.doSchedules(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void postMethodReturns405() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("POST");

            endpoint.doSchedules(req, rsp);

            verify(rsp).sendError(405, "Method Not Allowed: only GET is served");
        }
    }

    @Test
    public void authorizedMiteReadsSchedules() throws Exception {
        j.createFreeStyleProject("plain-job");

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))))) {

            ActivityEndpoint endpoint = new ActivityEndpoint();
            StaplerRequest2 req = mock(StaplerRequest2.class);
            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            when(req.getMethod()).thenReturn("GET");

            StringWriter writer = new StringWriter();
            when(rsp.getWriter()).thenReturn(new PrintWriter(writer));

            endpoint.doSchedules(req, rsp);

            verify(rsp).setContentType("application/json");
            verify(rsp).setStatus(200);
            String json = writer.toString();
            assertTrue("response should carry schedules: " + json, json.contains("\"schedules\""));
            assertTrue("response should carry total: " + json, json.contains("\"total\""));
            assertTrue("response should carry truncated: " + json, json.contains("\"truncated\""));
        }
    }

    // ---- 8.2/8.3: one row per trigger, one none row for an untriggered job ----

    @Test
    public void timerJobAndUntriggeredJobProduceTwoRows() throws Exception {
        FreeStyleProject timed = j.createFreeStyleProject("timed-job");
        timed.addTrigger(new TimerTrigger("H/15 * * * *"));
        FreeStyleProject plain = j.createFreeStyleProject("plain-job");

        List<JSONObject> rows = ActivityEndpoint.buildCandidateRows(j.jenkins);

        JSONObject timedRow = findRow(rows, timed.getFullName());
        JSONObject plainRow = findRow(rows, plain.getFullName());

        assertEquals("cron", timedRow.getString("triggerType"));
        assertEquals("none", plainRow.getString("triggerType"));
        assertEquals("", plainRow.getString("spec"));
        assertEquals("", plainRow.getString("timezone"));
        assertTrue(plainRow.getJSONArray("nextFireTimes").isEmpty());
    }

    // ---- 8.3: the complete trigger-type map ----

    @Test
    public void mapTriggerTypeCoversTheCompleteTable() {
        assertEquals("cron", ActivityEndpoint.mapTriggerType("TimerTrigger"));
        assertEquals("scm-poll", ActivityEndpoint.mapTriggerType("SCMTrigger"));
        assertEquals("upstream", ActivityEndpoint.mapTriggerType("ReverseBuildTrigger"));
        assertEquals("varroa-upstream", ActivityEndpoint.mapTriggerType("VarroaUpstreamTrigger"));
        assertEquals("other", ActivityEndpoint.mapTriggerType("SomeUnknownTrigger"));
    }

    // ---- 10.2: the varroaUpstream trigger's row ----

    @Test
    public void varroaUpstreamTriggerRowCarriesTheThreeFields() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("varroa-upstream-job");
        Trigger<?> trigger = new VarroaUpstreamTrigger("payments", "release");

        JSONObject row = ActivityEndpoint.buildTriggerRow(job, trigger, false);

        assertEquals("varroa-upstream", row.getString("triggerType"));
        assertEquals("payments", row.getString("upstreamTenant"));
        assertEquals("release", row.getString("upstreamJob"));
        assertEquals("SUCCESS", row.getString("upstreamResult"));
        assertTrue("a varroaUpstream trigger carries no occurrence",
                row.getJSONArray("nextFireTimes").isEmpty());
    }

    @Test
    public void timerAndVarroaUpstreamTriggersProduceTwoRows() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("timer-and-upstream-job");
        job.addTrigger(new TimerTrigger("H/15 * * * *"));
        job.addTrigger(new VarroaUpstreamTrigger("payments", "release"));

        List<JSONObject> rows = ActivityEndpoint.buildCandidateRows(j.jenkins);
        List<JSONObject> jobRows = findAllRows(rows, job.getFullName());

        assertEquals("a job with a timer trigger and a varroaUpstream trigger must produce two rows",
                2, jobRows.size());
        List<String> types = new ArrayList<>();
        for (JSONObject row : jobRows) {
            types.add(row.getString("triggerType"));
        }
        assertTrue("expected a cron row among " + types, types.contains("cron"));
        assertTrue("expected a varroa-upstream row among " + types, types.contains("varroa-upstream"));
    }

    @Test
    public void ordinaryRowCarriesNoUpstreamFields() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("ordinary-cron-job");
        Trigger<?> trigger = new TimerTrigger("H/15 * * * *");

        JSONObject row = ActivityEndpoint.buildTriggerRow(job, trigger, false);

        assertFalse("a non-upstream row must carry no upstreamTenant key",
                row.containsKey("upstreamTenant"));
        assertFalse("a non-upstream row must carry no upstreamJob key",
                row.containsKey("upstreamJob"));
        assertFalse("a non-upstream row must carry no upstreamResult key",
                row.containsKey("upstreamResult"));
    }

    @Test
    public void upstreamRowCarriesNoOccurrence() throws Exception {
        FreeStyleProject upstream = j.createFreeStyleProject("upstream-of-job");
        FreeStyleProject job = j.createFreeStyleProject("upstream-job");
        Trigger<?> trigger = new ReverseBuildTrigger(upstream.getFullName());

        JSONObject row = ActivityEndpoint.buildTriggerRow(job, trigger, false);

        assertEquals("upstream", row.getString("triggerType"));
        assertTrue(row.getJSONArray("nextFireTimes").isEmpty());
    }

    // ---- Finding 9 (review round 1): the complete job inventory, not just the
    // ordinary freestyle case. ----

    @Test
    public void twoTriggersOnOneJobProduceTwoRows() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("dual-trigger-job");
        job.addTrigger(new TimerTrigger("H/15 * * * *"));
        job.addTrigger(new SCMTrigger("H/5 * * * *"));

        List<JSONObject> rows = ActivityEndpoint.buildCandidateRows(j.jenkins);
        List<JSONObject> jobRows = findAllRows(rows, job.getFullName());

        assertEquals("a job with two triggers must produce two rows", 2, jobRows.size());
        List<String> types = new ArrayList<>();
        for (JSONObject row : jobRows) {
            types.add(row.getString("triggerType"));
        }
        assertTrue("expected a cron row among " + types, types.contains("cron"));
        assertTrue("expected a scm-poll row among " + types, types.contains("scm-poll"));
    }

    @Test
    public void scmTriggerProducesScmPollType() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("scm-poll-job");
        Trigger<?> trigger = new SCMTrigger("H/5 * * * *");

        JSONObject row = ActivityEndpoint.buildTriggerRow(job, trigger, false);

        assertEquals("scm-poll", row.getString("triggerType"));
    }

    @Test
    public void unknownTriggerSubclassProducesOtherType() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("other-trigger-job");
        Trigger<?> trigger = new UnmappedFakeTrigger("H/10 * * * *");

        JSONObject row = ActivityEndpoint.buildTriggerRow(job, trigger, false);

        assertEquals("other", row.getString("triggerType"));
    }

    @Test
    public void disabledJobStillReportsOccurrences() throws Exception {
        FreeStyleProject job = j.createFreeStyleProject("disabled-cron-job");
        job.addTrigger(new TimerTrigger("H/15 * * * *"));
        job.disable();

        List<JSONObject> rows = ActivityEndpoint.buildCandidateRows(j.jenkins);
        JSONObject row = findRow(rows, job.getFullName());

        assertTrue(row.getBoolean("disabled"));
        assertFalse("a disabled job must still report its occurrences",
                row.getJSONArray("nextFireTimes").isEmpty());
    }

    @Test
    public void folderNestedJobAppearsInTheInventory() throws Exception {
        // workflow-multibranch is not a test-scope dependency of this plugin
        // (plugin/pom.xml carries no such entry). A folder-nested job exercises the
        // same recursive walk: Items.getAllItems(Job.class) descends into any
        // ItemGroup, which is what a multibranch project's branch container is.
        MockFolder folder = j.createFolder("team-a-folder");
        FreeStyleProject nested = folder.createProject(FreeStyleProject.class, "nested-job");

        List<JSONObject> rows = ActivityEndpoint.buildCandidateRows(j.jenkins);

        JSONObject row = findRow(rows, nested.getFullName());
        assertEquals("none", row.getString("triggerType"));
    }

    // ---- 8.3b: occurrences computed with Jenkins's own CronTab ----

    @Test
    public void weekdayOnlySpecReportsNoWeekendOccurrence() {
        // 2026-09-12T10:00:00Z is a Saturday.
        Instant saturday = Instant.parse("2026-09-12T10:00:00Z");

        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "H H(8-16) * * 1-5", "weekday-job", "UTC", saturday);

        assertEquals(3, occurrences.size());
        for (String occurrence : occurrences) {
            DayOfWeek day = Instant.parse(occurrence).atZone(ZoneOffset.UTC).getDayOfWeek();
            assertNotEquals("occurrence " + occurrence + " fell on Saturday", DayOfWeek.SATURDAY, day);
            assertNotEquals("occurrence " + occurrence + " fell on Sunday", DayOfWeek.SUNDAY, day);
        }
    }

    @Test
    public void hashedFieldResolvesToTheMinuteTriggerStartSchedules() throws Exception {
        FreeStyleProject jobOne = j.createFreeStyleProject("hash-job-one");
        FreeStyleProject jobTwo = j.createFreeStyleProject("hash-job-two");

        TimerTrigger triggerOne = new TimerTrigger("H/15 * * * *");
        triggerOne.start(jobOne, true);
        TimerTrigger triggerTwo = new TimerTrigger("H/15 * * * *");
        triggerTwo.start(jobTwo, true);

        List<String> occurrencesOne = ActivityEndpoint.computeOccurrences(
                "H/15 * * * *", jobOne.getFullName(), "", null);
        List<String> occurrencesTwo = ActivityEndpoint.computeOccurrences(
                "H/15 * * * *", jobTwo.getFullName(), "", null);

        assertNotEquals("two jobs with different names must hash to different minutes",
                occurrencesOne.get(0), occurrencesTwo.get(0));

        CronTabList tabsOne = readTabs(triggerOne);
        assertTrue("the exported occurrence must match Trigger.start's own schedule",
                tabsOne.check(calendarAt(occurrencesOne.get(0))));
    }

    @Test
    public void dstTransitionKeepsTheLocalHour() {
        // 2027-03-14 is the US spring-forward Sunday for America/New_York.
        // A fixed clock the afternoon before walks the occurrences across the transition.
        Instant fixedNow = Instant.parse("2027-03-12T20:00:00Z");

        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "0 9 * * *", "dst-job", "America/New_York", fixedNow);

        assertEquals(3, occurrences.size());
        // Before the transition: 09:00 EST == 14:00Z. After: 09:00 EDT == 13:00Z.
        assertEquals("2027-03-13T14:00:00Z", occurrences.get(0));
        assertEquals("2027-03-14T13:00:00Z", occurrences.get(1));
        assertEquals("2027-03-15T13:00:00Z", occurrences.get(2));
    }

    // ---- Finding 7 (review round 1): a 02:30 local specification must match
    // Jenkins CronTab.ceil across a daylight-saving change, with no dropped and no
    // duplicated occurrence. ----

    @Test
    public void dstSpringForwardMatchesCronTabCeil() {
        // 2027-03-14 is the US spring-forward Sunday for America/New_York. Local
        // time 02:30 does not exist that day, so CronTab.ceil reports no occurrence
        // on that day rather than an invented one.
        Instant fixedNow = Instant.parse("2027-03-12T20:00:00Z");

        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "30 2 * * *", "dst-spring-job", "America/New_York", fixedNow);
        List<String> expected = ceilDirectlyWithCronTab(
                "30 2 * * *", "dst-spring-job", "America/New_York", fixedNow, 3);

        assertEquals("the endpoint must report exactly what CronTab.ceil produces",
                expected, occurrences);
        assertNoDuplicateOccurrence(occurrences);
    }

    @Test
    public void dstFallBackMatchesCronTabCeil() {
        // 2027-11-07 is the US fall-back Sunday for America/New_York. Local time
        // 02:30 happens twice that day; CronTab.ceil reports one instant for it,
        // not two.
        Instant fixedNow = Instant.parse("2027-11-05T20:00:00Z");

        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "30 2 * * *", "dst-fall-job", "America/New_York", fixedNow);
        List<String> expected = ceilDirectlyWithCronTab(
                "30 2 * * *", "dst-fall-job", "America/New_York", fixedNow, 3);

        assertEquals("the endpoint must report exactly what CronTab.ceil produces",
                expected, occurrences);
        assertNoDuplicateOccurrence(occurrences);
    }

    // ---- 8.3c: a line that throws leaves the row with what it already holds ----

    @Test
    public void impossibleSpecificationReportsNoOccurrenceAndNoError() {
        // February never has a 30th day.
        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "0 0 30 2 *", "impossible-job", "UTC", null);

        assertTrue(occurrences.isEmpty());
    }

    // ---- Finding 8 (review round 1): a later line that throws does not clear the
    // occurrences an earlier valid line already produced. ----

    @Test
    public void laterLineThrowsAndRowKeepsTheEarlierLinesOccurrences() {
        Instant now = Instant.parse("2026-09-05T13:00:00Z");
        // Line 1 is valid. Line 2 never matches, because February has no 30th day.
        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "0 9 * * *\n0 0 30 2 *", "multi-line-job", "UTC", now);
        List<String> validLineOnly = ActivityEndpoint.computeOccurrences(
                "0 9 * * *", "multi-line-job", "UTC", now);

        assertEquals("the row must report exactly the valid line's occurrences",
                validLineOnly, occurrences);
        assertEquals(3, occurrences.size());
    }

    // ---- 8.3d: every occurrence is RFC 3339 UTC ----

    @Test
    public void occurrencesAreRfc3339Utc() {
        Instant now = Instant.parse("2026-09-05T13:00:00Z");
        List<String> occurrences = ActivityEndpoint.computeOccurrences(
                "H/15 * * * *", "rfc-job", "", now);

        assertFalse(occurrences.isEmpty());
        for (String occurrence : occurrences) {
            assertTrue(occurrence + " must be RFC 3339 UTC", occurrence.endsWith("Z"));
            Instant.parse(occurrence); // throws if not a valid instant string
        }
    }

    // ---- 8.4: TZ= line split out of the specification ----

    @Test
    public void splitsTimezoneLineWhenPresent() {
        String[] result = ActivityEndpoint.splitTimezone("TZ=America/New_York\nH/15 * * * *");
        assertEquals("H/15 * * * *", result[0]);
        assertEquals("America/New_York", result[1]);
    }

    @Test
    public void leavesSpecUnchangedWhenNoTimezoneLine() {
        String[] result = ActivityEndpoint.splitTimezone("H/15 * * * *");
        assertEquals("H/15 * * * *", result[0]);
        assertEquals("", result[1]);
    }

    // ---- 8.5: the row bound and the byte bound ----

    @Test
    public void rowBoundStopsAtFiveThousand() {
        List<JSONObject> candidates = new java.util.ArrayList<>();
        for (int i = 0; i < 5200; i++) {
            JSONObject row = new JSONObject();
            row.put("jobPath", "job-" + i);
            row.put("triggerType", "none");
            row.put("spec", "");
            row.put("timezone", "");
            row.put("disabled", false);
            row.put("nextFireTimes", new net.sf.json.JSONArray());
            candidates.add(row);
        }

        JSONObject envelope = ActivityEndpoint.applyScheduleBounds(candidates);

        assertEquals(5000, envelope.getJSONArray("schedules").size());
        assertTrue(envelope.getBoolean("truncated"));
        assertEquals(5200, envelope.getInt("total"));
    }

    @Test
    public void byteBoundStopsBeforeTheRowBound() {
        // Each row carries three occurrences, padded so the byte bound is crossed
        // well before 5000 rows accumulate.
        List<JSONObject> candidates = new java.util.ArrayList<>();
        String occurrence = "2026-09-05T13:07:00Z";
        for (int i = 0; i < 4500; i++) {
            JSONObject row = new JSONObject();
            row.put("jobPath", "team-a/service-" + i + "/main-branch-job-name-padding");
            row.put("triggerType", "cron");
            row.put("spec", "H/15 * * * *");
            row.put("timezone", "America/New_York");
            row.put("disabled", false);
            net.sf.json.JSONArray occurrences = new net.sf.json.JSONArray();
            occurrences.add(occurrence);
            occurrences.add(occurrence);
            occurrences.add(occurrence);
            row.put("nextFireTimes", occurrences);
            candidates.add(row);
        }

        JSONObject envelope = ActivityEndpoint.applyScheduleBounds(candidates);

        int kept = envelope.getJSONArray("schedules").size();
        assertTrue("the byte bound must stop the walk before the row bound", kept < 5000);
        assertTrue("the byte bound must stop the walk before the candidate count", kept < 4500);
        assertTrue(envelope.getBoolean("truncated"));
        assertEquals(4500, envelope.getInt("total"));
    }

    // ---- helpers ----

    private static JSONObject findRow(List<JSONObject> rows, String jobPath) {
        for (JSONObject row : rows) {
            if (jobPath.equals(row.getString("jobPath"))) {
                return row;
            }
        }
        throw new AssertionError("no row for job path " + jobPath);
    }

    private static List<JSONObject> findAllRows(List<JSONObject> rows, String jobPath) {
        List<JSONObject> result = new ArrayList<>();
        for (JSONObject row : rows) {
            if (jobPath.equals(row.getString("jobPath"))) {
                result.add(row);
            }
        }
        return result;
    }

    private static CronTabList readTabs(Trigger<?> trigger) throws Exception {
        Field field = Trigger.class.getDeclaredField("tabs");
        field.setAccessible(true);
        return (CronTabList) field.get(trigger);
    }

    private static Calendar calendarAt(String rfc3339) {
        Calendar cal = Calendar.getInstance(TimeZone.getTimeZone("UTC"));
        cal.setTimeInMillis(Instant.parse(rfc3339).toEpochMilli());
        return cal;
    }

    /**
     * Computes occurrences by calling {@link CronTab#ceil} directly, mirroring
     * {@code ActivityEndpoint.computeOccurrences} step by step. Serves as the ground
     * truth for the daylight-saving boundary tests, so a pass proves the endpoint
     * reports exactly what Jenkins's own cron implementation produces.
     */
    private static List<String> ceilDirectlyWithCronTab(String spec, String jobFullName,
            String timezone, Instant fixedNow, int count) {
        Hash hash = Hash.from(jobFullName);
        CronTab tab = new CronTab(spec, 1, hash, timezone);
        TimeZone tz = tab.getTimeZone();
        Calendar seed = Calendar.getInstance(tz);
        seed.setTimeInMillis(fixedNow.toEpochMilli());
        List<String> result = new ArrayList<>();
        for (int i = 0; i < count; i++) {
            Calendar candidate = (Calendar) seed.clone();
            Calendar ceiled = tab.ceil(candidate);
            result.add(DateTimeFormatter.ISO_INSTANT.format(ceiled.toInstant()));
            seed = (Calendar) ceiled.clone();
            seed.add(Calendar.MINUTE, 1);
        }
        return result;
    }

    private static void assertNoDuplicateOccurrence(List<String> occurrences) {
        assertEquals("no occurrence may repeat",
                new LinkedHashSet<>(occurrences).size(), occurrences.size());
    }

    /**
     * A trigger whose simple class name carries no entry in
     * {@link ActivityEndpoint#mapTriggerType}, so it must map to {@code other}.
     */
    private static final class UnmappedFakeTrigger extends Trigger<Job<?, ?>> {
        UnmappedFakeTrigger(String spec) {
            super(spec);
        }
    }

}
