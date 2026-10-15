package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;

import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.OptionalInt;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Process-wide cache of {@code GET /v1/homes}: for each tenant this cell
 * hosts, the slot this cell holds, the role, and the tenant's job-path to
 * slot map.
 *
 * <p>The plugin computes no home. It reads the map the recorder wrote and
 * the operator delivered, so the plugin and the claim fence read one home
 * for every job.
 *
 * <p>Two fallbacks keep the plugin and the fence in step. An empty entry
 * list means the mite has applied no desired state carrying the map, and
 * the plugin then has no opinion. A job with no entry in a present map
 * also has no opinion: the recorder homes a new job before the map
 * reaches this cell. The cell then claims, and the claim fence decides.
 * A {@code NOT_HOME} veto names the home slot, and the claimer forwards
 * there.
 */
public final class VarroaHomeCache {

    private static final Logger LOGGER = Logger.getLogger(VarroaHomeCache.class.getName());

    /** Refresh interval, overridden by {@code VARROA_HOMES_POLL_MS}. */
    static final long DEFAULT_POLL_MILLIS = 30_000L;

    static final String POLL_ENV = "VARROA_HOMES_POLL_MS";

    /** The role value of a member that holds no slot and homes no job. */
    static final String ROLE_STANDBY = "standby";

    private static final VarroaHomeCache INSTANCE = new VarroaHomeCache(new MiteLoopbackClient());

    private final MiteLoopbackClient client;

    /** The newest answer, keyed by tenant. Null until the first refresh succeeds. */
    private volatile Map<String, Entry> entries;

    /** Returns the process-wide singleton. */
    public static VarroaHomeCache get() {
        return INSTANCE;
    }

    /** Package-private constructor for testing: injects the loopback client. */
    VarroaHomeCache(MiteLoopbackClient client) {
        this.client = client;
    }

    /** One hosted tenant's entry. */
    static final class Entry {
        final String tenant;
        final int slot;
        final String role;
        final Map<String, Integer> jobHomes;

        Entry(String tenant, int slot, String role, Map<String, Integer> jobHomes) {
            this.tenant = tenant;
            this.slot = slot;
            this.role = role != null ? role : "";
            this.jobHomes = jobHomes != null ? jobHomes : Collections.emptyMap();
        }
    }

    /**
     * Reads the route once and replaces the held answer. A failed read
     * keeps the previous answer, so one lost reply never turns a present
     * map into "no opinion".
     */
    public void refresh() {
        MiteLoopbackClient.Response resp = client.homes();
        if (resp.isTransportFailure() || resp.statusCode != 200 || resp.body == null) {
            LOGGER.log(Level.FINE, "VarroaHomeCache: the home map read failed; keeping the previous answer");
            return;
        }
        Map<String, Entry> parsed = parse(resp.body);
        if (parsed == null) {
            LOGGER.log(Level.FINE, "VarroaHomeCache: the home map did not decode; keeping the previous answer");
            return;
        }
        entries = parsed;
    }

    /**
     * True once a refresh has answered at least one hosted tenant. False
     * before the first successful read, and false for an empty entry
     * list: the plugin then forwards nothing and claims as it does with
     * no home map.
     */
    public boolean hasOpinion() {
        Map<String, Entry> held = entries;
        return held != null && !held.isEmpty();
    }

    /**
     * The home slot of {@code jobPath}, or empty when the held map has no
     * entry for it. An empty answer is "no opinion", never slot 0.
     */
    public OptionalInt homeSlotOf(String jobPath) {
        Entry entry = entryFor(tenantOf(jobPath));
        if (entry == null) {
            return OptionalInt.empty();
        }
        Integer slot = entry.jobHomes.get(jobPath);
        return slot != null ? OptionalInt.of(slot) : OptionalInt.empty();
    }

    /**
     * The slot this cell holds for {@code tenant}. Empty for a standby,
     * which holds no slot, and empty for a tenant this cell does not
     * host.
     */
    public OptionalInt heldSlotFor(String tenant) {
        Entry entry = entryFor(tenant);
        if (entry == null || ROLE_STANDBY.equals(entry.role)) {
            return OptionalInt.empty();
        }
        return OptionalInt.of(entry.slot);
    }

    /**
     * True when the held answer names {@code tenant}. A job of a tenant
     * this cell does not host carries no home here, so the claimer must
     * not forward it.
     */
    public boolean hostsTenant(String tenant) {
        return entryFor(tenant) != null;
    }

    /**
     * The home slot {@code jobPath} must be forwarded to, or empty when
     * this cell claims for it. A standby holds no slot, so every
     * submission of a hosted tenant with a map entry answers that entry.
     * A job with no entry answers empty: the cell claims, and a
     * {@code NOT_HOME} veto then names the home slot.
     */
    public OptionalInt forwardSlotFor(String jobPath) {
        String tenant = tenantOf(jobPath);
        if (!hasOpinion() || !hostsTenant(tenant)) {
            return OptionalInt.empty();
        }
        OptionalInt home = homeSlotOf(jobPath);
        if (home.isEmpty()) {
            return OptionalInt.empty();
        }
        OptionalInt held = heldSlotFor(tenant);
        if (held.isPresent() && held.getAsInt() == home.getAsInt()) {
            return OptionalInt.empty();
        }
        return home;
    }

    /** The tenant folder of a cell job path: its first path segment. */
    public static String tenantOf(String jobPath) {
        if (jobPath == null || jobPath.isEmpty()) {
            return "";
        }
        int slash = jobPath.indexOf('/');
        return slash >= 0 ? jobPath.substring(0, slash) : jobPath;
    }

    /** The path below the tenant folder, which the cross-tenant build kind carries. */
    public static String pathBelowTenant(String jobPath) {
        if (jobPath == null) {
            return "";
        }
        int slash = jobPath.indexOf('/');
        return slash >= 0 ? jobPath.substring(slash + 1) : jobPath;
    }

    /** The refresh interval in milliseconds. */
    static long pollMillis() {
        return pollMillis(System.getenv(POLL_ENV));
    }

    /**
     * Parses the interval from its raw value. Package-private, and takes
     * the raw value rather than reading the environment itself, so a test
     * can drive every input without mutating the process environment.
     */
    static long pollMillis(String raw) {
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_POLL_MILLIS;
        }
        try {
            long parsed = Long.parseLong(raw.trim());
            return parsed > 0 ? parsed : DEFAULT_POLL_MILLIS;
        } catch (NumberFormatException e) {
            return DEFAULT_POLL_MILLIS;
        }
    }

    private Entry entryFor(String tenant) {
        Map<String, Entry> held = entries;
        if (held == null || tenant == null || tenant.isEmpty()) {
            return null;
        }
        return held.get(tenant);
    }

    /** Package-private seam for testing: seeds the held answer directly. */
    void seedForTesting(List<Entry> seeded) {
        Map<String, Entry> map = new LinkedHashMap<>();
        for (Entry entry : seeded) {
            map.put(entry.tenant, entry);
        }
        entries = Collections.unmodifiableMap(map);
    }

    /** Package-private seam for testing: drops the held answer. */
    void clearForTesting() {
        entries = null;
    }

    /**
     * Decodes one answer. Returns null for a body the route never serves,
     * so the caller keeps the previous answer.
     *
     * <p>The route always carries an entry list, empty or not. A body that
     * is no JSON object, and a body whose {@code entries} key is absent or
     * is not a list, are therefore undecodable, not empty. Reading either
     * as an empty map would overwrite a valid map with "no opinion", and
     * the cell would then claim for a job it does not home.
     *
     * <p>An empty list is the one legitimate "no opinion": the mite has
     * applied no desired state that carries the map.
     */
    @SuppressWarnings("unchecked")
    private static Map<String, Entry> parse(String body) {
        Object decoded;
        try {
            decoded = new JsonSlurper().parseText(body);
        } catch (Exception e) {
            return null;
        }
        if (!(decoded instanceof Map)) {
            return null;
        }
        Object rawEntries = ((Map<String, Object>) decoded).get("entries");
        if (!(rawEntries instanceof List)) {
            return null;
        }
        Map<String, Entry> out = new LinkedHashMap<>();
        for (Object raw : (List<Object>) rawEntries) {
            if (!(raw instanceof Map)) {
                continue;
            }
            Entry entry = parseEntry((Map<String, Object>) raw);
            if (entry != null) {
                out.put(entry.tenant, entry);
            }
        }
        return Collections.unmodifiableMap(out);
    }

    @SuppressWarnings("unchecked")
    private static Entry parseEntry(Map<String, Object> raw) {
        Object tenant = raw.get("tenant");
        if (tenant == null || String.valueOf(tenant).isEmpty()) {
            return null;
        }
        Map<String, Integer> homes = new LinkedHashMap<>();
        Object rawHomes = raw.get("job_homes");
        if (rawHomes instanceof Map) {
            for (Map.Entry<String, Object> home : ((Map<String, Object>) rawHomes).entrySet()) {
                homes.put(home.getKey(), toInt(home.getValue()));
            }
        }
        Object role = raw.get("role");
        return new Entry(String.valueOf(tenant), toInt(raw.get("slot")),
                role != null ? String.valueOf(role) : "", Collections.unmodifiableMap(homes));
    }

    private static int toInt(Object value) {
        if (value instanceof Number) {
            return ((Number) value).intValue();
        }
        try {
            return Integer.parseInt(String.valueOf(value));
        } catch (NumberFormatException e) {
            return 0;
        }
    }
}
