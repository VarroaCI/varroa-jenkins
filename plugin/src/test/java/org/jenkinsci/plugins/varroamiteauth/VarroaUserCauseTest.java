package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotEquals;
import static org.junit.Assert.assertNull;

import hudson.model.Cause;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;

import java.util.List;

import org.junit.Test;

/**
 * Tests for {@link VarroaUserCause}: the short description names the actor, the
 * equality keeps one cause for one actor, and the class holds the one copy of
 * the cause-to-actor precedence.
 */
public class VarroaUserCauseTest {

    @Test
    public void shortDescriptionNamesTheActorAndVarroa() {
        assertEquals("Started by alice through Varroa",
                new VarroaUserCause("alice").getShortDescription());
    }

    @Test
    public void userIdAccessorAnswersTheActor() {
        assertEquals("alice", new VarroaUserCause("alice").getUserId());
    }

    @Test
    public void equalActorsAreEqualAndShareAHash() {
        VarroaUserCause one = new VarroaUserCause("alice");
        VarroaUserCause other = new VarroaUserCause("alice");
        assertEquals(one, other);
        assertEquals(one.hashCode(), other.hashCode());
    }

    @Test
    public void differentActorsAreNotEqual() {
        assertNotEquals(new VarroaUserCause("alice"), new VarroaUserCause("bob"));
    }

    @Test
    public void nullActorReadsAsEmpty() {
        assertEquals("", new VarroaUserCause(null).getUserId());
    }

    @Test
    public void noCauseNamesNoActor() {
        assertNull(VarroaUserCause.actorFromCauses(List.of()));
        assertNull(VarroaUserCause.actorFromCauses(null));
    }

    @Test
    public void varroaCauseNamesTheActor() {
        assertEquals("alice",
                VarroaUserCause.actorFromCauses(List.of(new VarroaUserCause("alice"))));
    }

    @Test
    public void userIdCauseNamesTheUser() {
        assertEquals("bob",
                VarroaUserCause.actorFromCauses(List.of(new Cause.UserIdCause("bob"))));
    }

    @Test
    public void varroaCauseWinsOverUserIdCause() {
        assertEquals("alice", VarroaUserCause.actorFromCauses(
                List.of(new Cause.UserIdCause("anonymous"), new VarroaUserCause("alice"))));
    }

    @Test
    public void classifiedCausesNameTheirKind() {
        assertEquals("scm", VarroaUserCause.actorFromCauses(
                List.of(new SCMTrigger.SCMTriggerCause())));
        assertEquals("scm", VarroaUserCause.actorFromCauses(
                List.of(new Cause.RemoteCause("10.0.0.1", "remote build"))));
        assertEquals("timer", VarroaUserCause.actorFromCauses(
                List.of(new TimerTrigger.TimerTriggerCause())));
    }

    @Test
    public void descriptionReplacesTheDefaultShortDescription() {
        assertEquals("Started by a pull request comment of alice on acme/app#7",
                new VarroaUserCause("alice", "Started by a pull request comment of alice on acme/app#7")
                        .getShortDescription());
    }

    @Test
    public void emptyOrNullDescriptionKeepsTheDefault() {
        assertEquals("Started by alice through Varroa",
                new VarroaUserCause("alice", "").getShortDescription());
        assertEquals("Started by alice through Varroa",
                new VarroaUserCause("alice", null).getShortDescription());
    }

    @Test
    public void descriptionTakesPartInEquality() {
        assertNotEquals(new VarroaUserCause("alice", "one"), new VarroaUserCause("alice", "two"));
        assertEquals(new VarroaUserCause("alice", "one"), new VarroaUserCause("alice", "one"));
        assertNotEquals(new VarroaUserCause("alice"), new VarroaUserCause("alice", "one"));
    }

    @Test
    public void nullDescriptionEqualsNoDescription() {
        assertEquals(new VarroaUserCause("alice"), new VarroaUserCause("alice", null));
        assertEquals(new VarroaUserCause("alice").hashCode(), new VarroaUserCause("alice", null).hashCode());
    }
}
