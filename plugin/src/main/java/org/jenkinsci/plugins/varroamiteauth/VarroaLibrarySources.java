package org.jenkinsci.plugins.varroamiteauth;

import com.cloudbees.hudson.plugins.folder.AbstractFolder;
import hudson.model.Action;
import hudson.model.Item;
import hudson.model.ItemGroup;
import hudson.model.Run;
import jenkins.model.GlobalConfiguration;

import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;
import java.util.Locale;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Resolves the repository of a shared library a run loaded. The lookup
 * is fully reflective. The pipeline-groovy-lib and the forge plugins are
 * not compile dependencies. Every method answers an empty value when a
 * read fails.
 *
 * <p>The lookup reads the current library configuration. It does not
 * read the configuration at build time.
 */
final class VarroaLibrarySources {

    private static final Logger LOGGER = Logger.getLogger(VarroaLibrarySources.class.getName());

    static final String FORGE_GITHUB = "github";
    static final String FORGE_GITLAB = "gitlab";
    static final String FORGE_GITEA = "gitea";

    private static final int MAX_URL_LENGTH = 512;
    private static final String BUILD_DATA_CLASS = "hudson.plugins.git.util.BuildData";

    private VarroaLibrarySources() {
    }

    /** The repository one library configuration names. */
    static final class Source {
        /** The sanitized repository URL. Empty when no safe URL is known. */
        final String repositoryUrl;
        /** The remote as the configuration names it. Used to match a build's checkout. */
        final String rawRemote;
        /** The forge of {@link #repositoryUrl}. Empty when unknown. */
        final String forge;

        Source(String repositoryUrl, String rawRemote, String forge) {
            this.repositoryUrl = repositoryUrl;
            this.rawRemote = rawRemote;
            this.forge = forge;
        }
    }

    /**
     * Finds the configuration of one loaded library and reads its
     * repository. A trusted record reads the global trusted libraries
     * only. An untrusted record reads the folder chain, nearest ancestor
     * first, then the global untrusted libraries. An unreadable trust
     * flag reads the folder chain, then the global trusted libraries,
     * then the global untrusted libraries. Answers null when no
     * configuration matches the name.
     */
    static Source sourceFor(Run<?, ?> run, String libraryName, Boolean trusted) {
        try {
            Object config;
            if (Boolean.TRUE.equals(trusted)) {
                config = globalConfig("GlobalLibraries", libraryName);
            } else if (Boolean.FALSE.equals(trusted)) {
                config = folderConfig(run, libraryName);
                if (config == null) {
                    config = globalConfig("GlobalUntrustedLibraries", libraryName);
                }
            } else {
                config = folderConfig(run, libraryName);
                if (config == null) {
                    config = globalConfig("GlobalLibraries", libraryName);
                }
                if (config == null) {
                    config = globalConfig("GlobalUntrustedLibraries", libraryName);
                }
            }
            if (config == null) {
                return null;
            }
            return fromRetriever(VarroaRunRecorder.invokeNoArg(config, "getRetriever"));
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "library repository lookup failed for " + libraryName, e);
            return null;
        }
    }

    /**
     * Reads the SCM of a library retriever. The remote is read from the
     * first source shape that answers: a Git source remote, a GitHub
     * repository URL, a Gitea server, owner, and name, or the first
     * remote of a legacy Git SCM.
     */
    static Source fromRetriever(Object retriever) {
        try {
            if (retriever == null) {
                return null;
            }
            Object scm = VarroaRunRecorder.invokeNoArg(retriever, "getScm");
            if (scm == null) {
                return null;
            }
            String raw = remoteOf(scm);
            if (raw.isEmpty()) {
                return new Source("", "", "");
            }
            String url = sanitize(raw);
            if (url.isEmpty()) {
                return new Source("", raw, "");
            }
            String forge = forgeFromSourceClass(scm.getClass().getSimpleName());
            if (forge.isEmpty()) {
                forge = forgeForHost(hostOf(url));
            }
            return new Source(url, raw, forge);
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "library retriever read failed", e);
            return null;
        }
    }

    /** Strips credentials and admits only http, https, ssh, and scp-like Git URLs. */
    static String sanitize(String url) {
        if (url == null) {
            return "";
        }
        String trimmed = url.trim();
        if (trimmed.isEmpty() || trimmed.length() > MAX_URL_LENGTH) {
            return "";
        }
        for (int i = 0; i < trimmed.length(); i++) {
            char c = trimmed.charAt(i);
            // A query or fragment can carry a token. Such a URL names no safe repository.
            if (c == '?' || c == '#' || Character.isWhitespace(c) || Character.isISOControl(c)) {
                return "";
            }
        }
        int scheme = trimmed.indexOf("://");
        if (scheme < 0) {
            if (!isScpLike(trimmed)) {
                return "";
            }
            // Rewrite the scp form as an ssh URL, so the user part is dropped.
            int at = trimmed.indexOf('@');
            int colon = trimmed.indexOf(':', at);
            return "ssh://" + trimmed.substring(at + 1, colon) + "/" + trimmed.substring(colon + 1);
        }
        String name = trimmed.substring(0, scheme).toLowerCase(Locale.ROOT);
        if (name.equals("https") || name.equals("http") || name.equals("ssh")) {
            // Userinfo can hold a password. Every admitted scheme drops it.
            String rest = trimmed.substring(scheme + 3);
            int slash = rest.indexOf('/');
            String authority = slash < 0 ? rest : rest.substring(0, slash);
            int at = authority.lastIndexOf('@');
            if (at >= 0) {
                rest = authority.substring(at + 1) + (slash < 0 ? "" : rest.substring(slash));
            }
            return name + "://" + rest;
        }
        return "";
    }

    /** Maps a host to a forge: github.com, gitlab.com, or a configured Gitea server. */
    static String forgeForHost(String host) {
        if (host == null || host.isEmpty()) {
            return "";
        }
        String h = host.toLowerCase(Locale.ROOT);
        if (h.equals("github.com")) {
            return FORGE_GITHUB;
        }
        if (h.equals("gitlab.com")) {
            return FORGE_GITLAB;
        }
        for (String giteaHost : giteaHosts()) {
            if (giteaHost.equals(h)) {
                return FORGE_GITEA;
            }
        }
        return "";
    }

    /**
     * Answers the revision a build checked out from {@code rawRemote}. The
     * remote matches after normalization. Answers empty when no checkout
     * of that remote carries a commit, for example after a cache hit.
     */
    static String revisionFor(String rawRemote, Iterable<?> buildDatas) {
        if (rawRemote == null || rawRemote.isEmpty() || buildDatas == null) {
            return "";
        }
        String want = normalizeForMatch(rawRemote);
        // Two checkouts of one repository at different commits name no single
        // revision for the library, so the answer stays empty.
        java.util.Set<String> shas = new java.util.LinkedHashSet<>();
        for (Object buildData : buildDatas) {
            try {
                Object remotes = VarroaRunRecorder.invokeNoArg(buildData, "getRemoteUrls");
                if (!(remotes instanceof Iterable) || !containsRemote((Iterable<?>) remotes, want)) {
                    continue;
                }
                Object revision = VarroaRunRecorder.invokeNoArg(buildData, "getLastBuiltRevision");
                Object sha = revision == null ? null : VarroaRunRecorder.invokeNoArg(revision, "getSha1String");
                if (sha instanceof String && !((String) sha).isEmpty()) {
                    shas.add((String) sha);
                }
            } catch (RuntimeException | LinkageError e) {
                LOGGER.log(Level.FINE, "checkout read failed", e);
            }
        }
        if (shas.size() == 1) {
            return shas.iterator().next();
        }
        return "";
    }

    /** Answers the git checkout actions of a run, in order. */
    static List<Object> buildDataActions(Run<?, ?> run) {
        List<Object> out = new ArrayList<>();
        try {
            for (Action action : run.getAllActions()) {
                if (action != null && action.getClass().getName().equals(BUILD_DATA_CLASS)) {
                    out.add(action);
                }
            }
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "checkout actions read failed", e);
            return Collections.emptyList();
        }
        return out;
    }

    // --- Lookup helpers ---

    private static boolean containsRemote(Iterable<?> remotes, String want) {
        for (Object remote : remotes) {
            if (remote instanceof String && normalizeForMatch((String) remote).equals(want)) {
                return true;
            }
        }
        return false;
    }

    private static Object folderConfig(Run<?, ?> run, String libraryName) {
        for (AbstractFolder<?> folder : ancestorFolders(run)) {
            for (Object property : folder.getProperties()) {
                if (property == null
                        || !VarroaRunRecorder.hasSimpleNameInHierarchy(property.getClass(), "FolderLibraries")) {
                    continue;
                }
                Object config = findNamed(VarroaRunRecorder.invokeNoArg(property, "getLibraries"), libraryName);
                if (config != null) {
                    return config;
                }
            }
        }
        return null;
    }

    private static List<AbstractFolder<?>> ancestorFolders(Run<?, ?> run) {
        List<AbstractFolder<?>> out = new ArrayList<>();
        Item item = run.getParent();
        ItemGroup<?> group = item == null ? null : item.getParent();
        while (group instanceof AbstractFolder) {
            out.add((AbstractFolder<?>) group);
            if (!(group instanceof Item)) {
                break;
            }
            group = ((Item) group).getParent();
        }
        return out;
    }

    /**
     * Scans every global configuration of the given simple name. A
     * runtime can carry more than one class of that name, so the scan
     * does not stop at the first one.
     */
    private static Object globalConfig(String simpleName, String libraryName) {
        for (GlobalConfiguration global : GlobalConfiguration.all()) {
            if (global != null && global.getClass().getSimpleName().equals(simpleName)) {
                Object config = findNamed(VarroaRunRecorder.invokeNoArg(global, "getLibraries"), libraryName);
                if (config != null) {
                    return config;
                }
            }
        }
        return null;
    }

    private static Object findNamed(Object configs, String name) {
        if (!(configs instanceof Iterable)) {
            return null;
        }
        for (Object config : (Iterable<?>) configs) {
            Object configName = VarroaRunRecorder.invokeNoArg(config, "getName");
            if (configName instanceof String && configName.equals(name)) {
                return config;
            }
        }
        return null;
    }

    /** Reads the remote of an SCM source, trying each supported shape in order. */
    private static String remoteOf(Object scm) {
        Object remote = VarroaRunRecorder.invokeNoArg(scm, "getRemote");
        if (remote instanceof String && !((String) remote).isEmpty()) {
            return (String) remote;
        }
        Object repositoryUrl = VarroaRunRecorder.invokeNoArg(scm, "getRepositoryUrl");
        if (repositoryUrl instanceof String && !((String) repositoryUrl).isEmpty()) {
            return (String) repositoryUrl;
        }
        Object server = VarroaRunRecorder.invokeNoArg(scm, "getServerUrl");
        Object owner = VarroaRunRecorder.invokeNoArg(scm, "getRepoOwner");
        Object repo = VarroaRunRecorder.invokeNoArg(scm, "getRepository");
        if (server instanceof String && owner instanceof String && repo instanceof String
                && !((String) server).isEmpty() && !((String) owner).isEmpty() && !((String) repo).isEmpty()) {
            return server + "/" + owner + "/" + repo;
        }
        Object remotes = VarroaRunRecorder.invokeNoArg(scm, "getUserRemoteConfigs");
        if (remotes instanceof Iterable) {
            Iterator<?> first = ((Iterable<?>) remotes).iterator();
            if (first.hasNext()) {
                Object url = VarroaRunRecorder.invokeNoArg(first.next(), "getUrl");
                if (url instanceof String) {
                    return (String) url;
                }
            }
        }
        return "";
    }

    private static String forgeFromSourceClass(String simpleName) {
        switch (simpleName) {
            case "GitHubSCMSource":
                return FORGE_GITHUB;
            case "GiteaSCMSource":
                return FORGE_GITEA;
            case "GitLabSCMSource":
                return FORGE_GITLAB;
            default:
                return "";
        }
    }

    /** Lists the host of every configured Gitea server. */
    private static List<String> giteaHosts() {
        List<String> hosts = new ArrayList<>();
        for (GlobalConfiguration global : GlobalConfiguration.all()) {
            if (global == null || !global.getClass().getSimpleName().equals("GiteaServers")) {
                continue;
            }
            Object servers = VarroaRunRecorder.invokeNoArg(global, "getServers");
            if (!(servers instanceof Iterable)) {
                continue;
            }
            for (Object server : (Iterable<?>) servers) {
                for (String method : new String[] {"getServerUrl", "getAliasUrl"}) {
                    Object value = VarroaRunRecorder.invokeNoArg(server, method);
                    if (value instanceof String && !((String) value).isEmpty()) {
                        String host = hostOf((String) value);
                        if (!host.isEmpty()) {
                            hosts.add(host.toLowerCase(Locale.ROOT));
                        }
                    }
                }
            }
        }
        return hosts;
    }

    /** Extracts the host of a URL or an scp-like remote, without userinfo or port. */
    static String hostOf(String url) {
        if (url == null || url.isEmpty()) {
            return "";
        }
        String rest = url;
        int scheme = rest.indexOf("://");
        if (scheme >= 0) {
            rest = rest.substring(scheme + 3);
        }
        int slash = rest.indexOf('/');
        String authority = slash < 0 ? rest : rest.substring(0, slash);
        int at = authority.lastIndexOf('@');
        if (at >= 0) {
            authority = authority.substring(at + 1);
        }
        int colon = authority.indexOf(':');
        return colon < 0 ? authority : authority.substring(0, colon);
    }

    private static boolean isScpLike(String url) {
        int at = url.indexOf('@');
        int colon = url.indexOf(':');
        int slash = url.indexOf('/');
        return at > 0 && colon > at && (slash < 0 || slash > colon) && colon < url.length() - 1;
    }

    /** Normalizes a remote for comparison: no scheme, no userinfo, no .git suffix, lowercase. */
    static String normalizeForMatch(String remote) {
        String s = remote.trim();
        int scheme = s.indexOf("://");
        if (scheme >= 0) {
            s = s.substring(scheme + 3);
        } else if (isScpLike(s)) {
            s = s.replaceFirst(":", "/");
        }
        int slash = s.indexOf('/');
        String authority = slash < 0 ? s : s.substring(0, slash);
        int at = authority.lastIndexOf('@');
        if (at >= 0) {
            s = s.substring(at + 1);
        }
        // Only the host is case-insensitive. Path case names a distinct repository on some servers.
        int hostEnd = s.indexOf('/');
        s = hostEnd < 0
                ? s.toLowerCase(Locale.ROOT)
                : s.substring(0, hostEnd).toLowerCase(Locale.ROOT) + s.substring(hostEnd);
        while (s.endsWith("/")) {
            s = s.substring(0, s.length() - 1);
        }
        if (s.endsWith(".git")) {
            s = s.substring(0, s.length() - 4);
        }
        return s;
    }
}
