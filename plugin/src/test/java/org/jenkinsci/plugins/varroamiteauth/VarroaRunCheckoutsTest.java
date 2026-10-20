package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.List;
import java.util.Set;

import org.eclipse.jgit.lib.ObjectId;
import org.junit.Test;

/** Tests the pure checkout choice of {@link VarroaRunCheckouts}. */
public class VarroaRunCheckoutsTest {

    private static final String APP = "https://git.example.com/acme/app.git";
    private static final String LIB = "https://git.example.com/acme/shared-lib.git";

    /** A git checkout stand-in with the three reads the picker and the facts use. */
    public static final class Checkout {
        private final String remote;
        private final String sha;

        Checkout(String remote, String sha) {
            this.remote = remote;
            this.sha = sha;
        }

        public List<String> getRemoteUrls() {
            return List.of(remote);
        }

        public Revision getLastBuiltRevision() {
            return new Revision(sha);
        }

        public List<Object> getBranches() {
            return List.of();
        }
    }

    /** A revision stand-in. */
    public static final class Revision {
        private final String sha;

        Revision(String sha) {
            this.sha = sha;
        }

        public String getSha1String() {
            return sha;
        }
    }

    private static List<String> noLibraries() {
        return List.of();
    }

    @Test
    public void aJobRemotePicksItsMatchingCheckoutOverAnEarlierOne() {
        Object library = new Checkout(LIB, "1111");
        Object app = new Checkout(APP, "2222");

        assertSame(app, VarroaRunCheckouts.select(APP, VarroaRunCheckoutsTest::noLibraries, List.of(library, app)));
    }

    @Test
    public void aJobRemoteWithNoMatchingCheckoutAnswersNull() {
        Object library = new Checkout(LIB, "1111");

        assertNull(VarroaRunCheckouts.select(APP, VarroaRunCheckoutsTest::noLibraries, List.of(library)));
    }

    @Test
    public void aJobRemoteMatchesAcrossSchemeUserinfoAndGitSuffix() {
        Object app = new Checkout("https://git.example.com/acme/app", "2222");

        assertSame(app, VarroaRunCheckouts.select("git@git.example.com:acme/app.git",
                VarroaRunCheckoutsTest::noLibraries, List.of(app)));
    }

    @Test
    public void noJobRemoteSkipsALibraryCheckout() {
        Object library = new Checkout(LIB, "1111");
        Object inline = new Checkout(APP, "2222");

        assertSame("the checkout that names no library wins",
                inline, VarroaRunCheckouts.select("", () -> List.of(LIB), List.of(library, inline)));
    }

    @Test
    public void noJobRemoteAndOnlyLibraryCheckoutsAnswersNull() {
        Object library = new Checkout(LIB, "1111");

        assertNull(VarroaRunCheckouts.select("", () -> List.of(LIB), List.of(library)));
    }

    @Test
    public void aJobRemoteNeverReadsTheLibraryRemotes() {
        Object app = new Checkout(APP, "2222");

        assertSame(app, VarroaRunCheckouts.select(APP, () -> {
            throw new AssertionError("the library remotes must not be read");
        }, List.of(app)));
    }

    @Test
    public void forgeForNamesGiteaFromTheSourceClass() {
        assertEquals("gitea", VarroaRunCheckouts.forgeFor("GiteaSCMSource", "https://gitea.example.com/acme/app"));
    }

    @Test
    public void forgeForFallsBackToTheHost() {
        assertEquals("github", VarroaRunCheckouts.forgeFor("GitSCM", "https://github.com/a/b.git"));
        assertEquals("gitlab", VarroaRunCheckouts.forgeFor("", "git@gitlab.com:a/b.git"));
    }

    @Test
    public void forgeForAnswersEmptyForAnUnknownHost() {
        assertEquals("", VarroaRunCheckouts.forgeFor("GitSCM", "file:///tmp/x"));
        assertEquals("", VarroaRunCheckouts.forgeFor("", ""));
    }

    private static final String PRE = "a".repeat(40);
    private static final String MERGED = "b".repeat(40);

    /** A git build record whose build chooser picked PRE and whose merge extension built MERGED. */
    private static hudson.plugins.git.util.BuildData mergeBuild() {
        ObjectId pre = ObjectId.fromString(PRE);
        ObjectId merged = ObjectId.fromString(MERGED);
        List<hudson.plugins.git.Branch> branches = List.of(new hudson.plugins.git.Branch("origin/PR-12", pre));
        hudson.plugins.git.util.BuildData data = new hudson.plugins.git.util.BuildData();
        data.saveBuild(new hudson.plugins.git.util.Build(
                new hudson.plugins.git.Revision(pre, branches),
                new hudson.plugins.git.Revision(merged, branches), 1, null));
        return data;
    }

    @Test
    public void headRevisionAnswersThePreMergeCommitOfAMergeBuild() {
        assertEquals(PRE, VarroaRunCheckouts.headRevision(mergeBuild()));
    }

    @Test
    public void headRevisionAnswersEmptyWithNoBuild() {
        assertEquals("", VarroaRunCheckouts.headRevision(new hudson.plugins.git.util.BuildData()));
        assertEquals("", VarroaRunCheckouts.headRevision(null));
    }

    @Test
    public void otherCheckoutRevisionsSkipsTheRunsOwnCheckout() {
        Object library = new Checkout(LIB, "1111");
        Object app = new Checkout(APP, "2222");

        assertEquals(Set.of("1111"), VarroaRunCheckouts.otherCheckoutRevisions(APP,
                VarroaRunCheckoutsTest::noLibraries, List.of(library, app)));
    }

    @Test
    public void otherCheckoutRevisionsWithNoJobRemoteSkipsTheFirstNonLibraryCheckout() {
        Object library = new Checkout(LIB, "1111");
        Object app = new Checkout(APP, "2222");

        assertEquals(Set.of("1111"), VarroaRunCheckouts.otherCheckoutRevisions("",
                () -> List.of(LIB), List.of(library, app)));
    }

    @Test
    public void otherCheckoutRevisionsFiltersNothingWhenTheRunsCheckoutIsUnknown() {
        Object library = new Checkout(LIB, "1111");
        Object app = new Checkout(APP, "2222");

        assertEquals(Set.of(), VarroaRunCheckouts.otherCheckoutRevisions("git@elsewhere.example.com:x/y.git",
                VarroaRunCheckoutsTest::noLibraries, List.of(library, app)));
    }

    @Test
    public void otherCheckoutRevisionsKeepsACommitTheRunsOwnCheckoutBuilt() {
        Object library = new Checkout(LIB, "2222");
        Object app = new Checkout(APP, "2222");

        assertEquals(Set.of(), VarroaRunCheckouts.otherCheckoutRevisions(APP,
                VarroaRunCheckoutsTest::noLibraries, List.of(library, app)));
    }

    @Test
    public void otherCheckoutRevisionsFiltersNothingWhenNoCheckoutIsTheRuns() {
        Object library = new Checkout(LIB, "1111");

        assertEquals(Set.of(), VarroaRunCheckouts.otherCheckoutRevisions(APP,
                VarroaRunCheckoutsTest::noLibraries, List.of(library)));
    }
}
