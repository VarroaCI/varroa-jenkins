package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import java.util.List;

import org.junit.Test;

/**
 * Plain unit tests for the reflective library repository resolver. The
 * fakes carry the simple class names the resolver matches. No Jenkins
 * runtime is needed, so no test here reaches a global configuration.
 */
public class VarroaLibrarySourcesTest {

    static final class GitSCMSource {
        private final String remote;

        GitSCMSource(String remote) {
            this.remote = remote;
        }

        public String getRemote() {
            return remote;
        }
    }

    static final class GitHubSCMSource {
        private final String repositoryUrl;

        GitHubSCMSource(String repositoryUrl) {
            this.repositoryUrl = repositoryUrl;
        }

        public String getRepositoryUrl() {
            return repositoryUrl;
        }
    }

    static final class GiteaSCMSource {
        public String getServerUrl() {
            return "https://git.example.com";
        }

        public String getRepoOwner() {
            return "tenant";
        }

        public String getRepository() {
            return "lib";
        }
    }

    static final class GitSCM {
        public List<UserRemoteConfig> getUserRemoteConfigs() {
            return List.of(new UserRemoteConfig("ssh://git@github.com/example/lib.git"));
        }
    }

    static final class UserRemoteConfig {
        private final String url;

        UserRemoteConfig(String url) {
            this.url = url;
        }

        public String getUrl() {
            return url;
        }
    }

    static final class Retriever {
        private final Object scm;

        Retriever(Object scm) {
            this.scm = scm;
        }

        public Object getScm() {
            return scm;
        }
    }

    static final class Revision {
        public String getSha1String() {
            return "deadbeef";
        }
    }

    static final class BuildData {
        private final List<String> remotes;

        BuildData(List<String> remotes) {
            this.remotes = remotes;
        }

        public List<String> getRemoteUrls() {
            return remotes;
        }

        public Revision getLastBuiltRevision() {
            return new Revision();
        }
    }

    @Test
    public void fromRetrieverReadsAGitSourceRemote() {
        VarroaLibrarySources.Source source = VarroaLibrarySources.fromRetriever(
                new Retriever(new GitSCMSource("https://github.com/example/shared-lib.git")));

        assertEquals("https://github.com/example/shared-lib.git", source.repositoryUrl);
        assertEquals("github", source.forge);
    }

    @Test
    public void fromRetrieverReadsAGitHubSourceAsGithub() {
        VarroaLibrarySources.Source source = VarroaLibrarySources.fromRetriever(
                new Retriever(new GitHubSCMSource("https://github.com/example/lib")));

        assertEquals("https://github.com/example/lib", source.repositoryUrl);
        assertEquals("github", source.forge);
    }

    @Test
    public void fromRetrieverBuildsAGiteaSourceUrl() {
        VarroaLibrarySources.Source source = VarroaLibrarySources.fromRetriever(
                new Retriever(new GiteaSCMSource()));

        assertEquals("https://git.example.com/tenant/lib", source.repositoryUrl);
        assertEquals("gitea", source.forge);
    }

    @Test
    public void fromRetrieverReadsALegacyGitScmRemote() {
        VarroaLibrarySources.Source source = VarroaLibrarySources.fromRetriever(new Retriever(new GitSCM()));

        assertEquals("ssh://github.com/example/lib.git", source.repositoryUrl);
        assertEquals("github", source.forge);
    }

    @Test
    public void sanitizeStripsHttpsUserinfo() {
        assertEquals("https://github.com/example/lib.git",
                VarroaLibrarySources.sanitize("https://user:token@github.com/example/lib.git"));
    }

    @Test
    public void sanitizeRewritesAnScpRemoteWithoutItsUser() {
        assertEquals("ssh://github.com/example/lib.git",
                VarroaLibrarySources.sanitize("git@github.com:example/lib.git"));
    }

    @Test
    public void sanitizeStripsSshUserinfo() {
        assertEquals("ssh://host.example.com/org/lib.git",
                VarroaLibrarySources.sanitize("ssh://user:secret@host.example.com/org/lib.git"));
    }

    @Test
    public void sanitizeDropsAQueryOrFragment() {
        assertEquals("", VarroaLibrarySources.sanitize("https://github.com/org/lib.git?access_token=secret"));
        assertEquals("", VarroaLibrarySources.sanitize("https://github.com/org/lib#frag"));
    }

    @Test
    public void sanitizeDropsAFileUrlAndAnOverCapUrl() {
        assertEquals("", VarroaLibrarySources.sanitize("file:///srv/repos/lib"));
        String overCap = "https://github.com/" + "a".repeat(600);
        assertEquals("", VarroaLibrarySources.sanitize(overCap));
    }

    @Test
    public void forgeForHostKnowsGithubAndGitlab() {
        assertEquals("github", VarroaLibrarySources.forgeForHost("github.com"));
        assertEquals("gitlab", VarroaLibrarySources.forgeForHost("gitlab.com"));
        assertEquals("", VarroaLibrarySources.forgeForHost(""));
    }

    @Test
    public void revisionForMatchesANormalizedRemote() {
        BuildData checkout = new BuildData(List.of("git@github.com:example/shared-lib.git"));

        assertEquals("deadbeef",
                VarroaLibrarySources.revisionFor("https://github.com/example/shared-lib", List.of(checkout)));
    }

    @Test
    public void revisionForKeepsPathCaseDistinct() {
        BuildData checkout = new BuildData(List.of("https://github.com/team/lib"));

        assertEquals("", VarroaLibrarySources.revisionFor("https://github.com/Tenant/Lib", List.of(checkout)));
    }

    @Test
    public void revisionForAnswersEmptyWithoutAMatch() {
        BuildData checkout = new BuildData(List.of("https://github.com/example/other.git"));

        assertEquals("",
                VarroaLibrarySources.revisionFor("https://github.com/example/shared-lib", List.of(checkout)));
    }

    @Test
    public void stripCredentialsDropsHttpsUserinfo() {
        assertEquals("https://github.com/example/app.git",
                VarroaLibrarySources.stripCredentials("https://user:token@github.com/example/app.git"));
        assertEquals("https://github.com/example/app.git",
                VarroaLibrarySources.stripCredentials("https://token@github.com/example/app.git"));
    }

    @Test
    public void stripCredentialsDropsAnSshPasswordAndKeepsAnSshUser() {
        assertEquals("ssh://host.example.com/org/app.git",
                VarroaLibrarySources.stripCredentials("ssh://user:secret@host.example.com/org/app.git"));
        assertEquals("ssh://git@host.example.com/org/app.git",
                VarroaLibrarySources.stripCredentials("ssh://git@host.example.com/org/app.git"));
    }

    @Test
    public void stripCredentialsKeepsAnScpUser() {
        assertEquals("git@github.com:example/app.git",
                VarroaLibrarySources.stripCredentials("git@github.com:example/app.git"));
    }

    @Test
    public void stripCredentialsDropsAnScpLikePassword() {
        assertEquals("host.example.com:org/app.git",
                VarroaLibrarySources.stripCredentials("user:pw@host.example.com:org/app.git"));
    }

    @Test
    public void stripCredentialsDropsAQueryAndAFragment() {
        assertEquals("https://github.com/org/app.git",
                VarroaLibrarySources.stripCredentials("https://github.com/org/app.git?access_token=secret"));
        assertEquals("https://github.com/org/app",
                VarroaLibrarySources.stripCredentials("https://github.com/org/app#frag"));
        assertEquals("https://github.com/org/app.git",
                VarroaLibrarySources.stripCredentials("https://user:pa#ss@github.com/org/app.git"));
    }

    @Test
    public void stripCredentialsKeepsAFileUrl() {
        assertEquals("file:/srv/repos/app", VarroaLibrarySources.stripCredentials("file:/srv/repos/app"));
        assertEquals("file:///srv/repos/app", VarroaLibrarySources.stripCredentials("file:///srv/repos/app"));
    }

    @Test
    public void stripCredentialsKeepsAnAtSignInThePath() {
        assertEquals("https://host.example.com/org/a@b.git",
                VarroaLibrarySources.stripCredentials("https://host.example.com/org/a@b.git"));
    }

    @Test
    public void stripCredentialsAnswersEmptyForNull() {
        assertEquals("", VarroaLibrarySources.stripCredentials(null));
    }
}
