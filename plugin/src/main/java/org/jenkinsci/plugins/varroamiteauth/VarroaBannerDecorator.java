package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.PageDecorator;
import hudson.model.User;
import hudson.model.UserProperty;
import hudson.security.csrf.CrumbIssuer;
import java.util.LinkedHashSet;
import java.util.Set;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;
import jenkins.model.Jenkins;
import org.kohsuke.stapler.Stapler;
import org.kohsuke.stapler.StaplerRequest2;
import org.springframework.security.core.GrantedAuthority;

@Extension
public class VarroaBannerDecorator extends PageDecorator {

    private static final Logger LOGGER = Logger.getLogger(VarroaBannerDecorator.class.getName());
    private static final AtomicBoolean ROLE_FAILURE_LOGGED = new AtomicBoolean();

    public String getBannerConfigURL() {
        return System.getenv("VARROA_BANNER_URL");
    }

    /**
     * Returns the current user's id, or "" if anonymous.
     */
    public String getCurrentUserId() {
        User u = User.current();
        return u == null ? "" : u.getId();
    }

    /**
     * Returns the current user's display name: full name when non-blank,
     * otherwise the user id. Returns "" when anonymous.
     */
    public String getCurrentUserName() {
        User u = User.current();
        if (u == null) {
            return "";
        }
        String fn = u.getFullName();
        return (fn != null && !fn.isBlank()) ? fn : u.getId();
    }

    /**
     * Returns the current user's email address via the Mailer UserProperty
     * (read reflectively, mirroring VarroaSecurityRealm), or "" when unavailable.
     */
    public String getCurrentUserEmail() {
        User u = User.current();
        if (u == null) {
            return "";
        }
        try {
            Class<? extends UserProperty> mailerPropClass =
                Class.forName("hudson.tasks.Mailer$UserProperty").asSubclass(UserProperty.class);
            Object prop = u.getProperty(mailerPropClass);
            if (prop == null) {
                return "";
            }
            Object addr = prop.getClass().getMethod("getAddress").invoke(prop);
            return addr == null ? "" : (String) addr;
        } catch (Exception e) {
            return "";
        }
    }

    /**
     * Returns the current user's authorities (minus authenticated/anonymous and
     * any ROLE:* role-strategy sids) as a comma-joined string, de-duplicated
     * preserving order, or "" when empty.
     */
    public String getCurrentUserGroups() {
        try {
            Set<String> groups = new LinkedHashSet<>();
            for (GrantedAuthority auth : Jenkins.get().getAuthentication2().getAuthorities()) {
                String a = auth.getAuthority();
                if (a == null) {
                    continue;
                }
                if ("authenticated".equals(a) || "anonymous".equals(a) || a.startsWith("ROLE:")) {
                    continue;
                }
                groups.add(a);
            }
            return groups.isEmpty() ? "" : String.join(",", groups);
        } catch (Exception e) {
            return "";
        }
    }

    /**
     * Returns the crumb request field name, or "" when the crumb issuer is
     * disabled or unavailable.
     */
    public String getCrumbFieldName() {
        try {
            CrumbIssuer issuer = Jenkins.get().getCrumbIssuer();
            if (issuer == null) {
                return "";
            }
            return issuer.getCrumbRequestField();
        } catch (Exception e) {
            return "";
        }
    }

    /**
     * Returns the crumb value for the current request, or "" when the crumb
     * issuer is disabled or we are off the request thread.
     */
    public String getCrumbValue() {
        try {
            CrumbIssuer issuer = Jenkins.get().getCrumbIssuer();
            if (issuer == null) {
                return "";
            }
            StaplerRequest2 req = Stapler.getCurrentRequest2();
            if (req == null) {
                return "";
            }
            return issuer.getCrumb(req);
        } catch (Exception e) {
            return "";
        }
    }

    /**
     * Returns the primary Varroa role label (Admin/Operator/Developer/Viewer)
     * for the current user, resolved via role-strategy reflection, or "" if
     * role-strategy is not the active strategy / not installed / unreadable.
     *
     * <p>role-strategy is not a compile-time dependency; everything is reflection
     * wrapped in a catch-all so API drift degrades to "no role".
     */
    public String getCurrentUserRole() {
        try {
            String label = resolveCurrentUserRole();
            // A successful resolution re-arms the WARNING so the next real
            // failure is audible again (a transient boot-time failure must not
            // permanently demote later breakage to FINE).
            ROLE_FAILURE_LOGGED.set(false);
            return label;
        } catch (Throwable t) {
            // Degrade to "no role", but leave a trace — a silent catch-all here
            // hid a wrong-package Class.forName for months (issue #533). WARNING
            // only once per failure streak: this runs on every page render, and
            // a persistent breakage would otherwise flood the log.
            if (ROLE_FAILURE_LOGGED.compareAndSet(false, true)) {
                LOGGER.log(Level.WARNING, "banner role resolution failed (further failures log at FINE)", t);
            } else {
                LOGGER.log(Level.FINE, "banner role resolution failed", t);
            }
            return "";
        }
    }

    private String resolveCurrentUserRole() throws Exception {
        {
            String userId = getCurrentUserId();
            if (userId.isEmpty()) {
                return "";
            }

            Object strat = Jenkins.get().getAuthorizationStrategy();

            // Load the RoleBasedAuthorizationStrategy class via the strategy's own classloader.
            // Absence is the expected "role-strategy not installed" case, not a
            // failure worth a WARNING in the catch-all below.
            Class<?> rbasClass;
            try {
                rbasClass = Class.forName(
                    "com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy",
                    false,
                    strat.getClass().getClassLoader());
            } catch (ClassNotFoundException e) {
                return "";
            }

            if (!rbasClass.isInstance(strat)) {
                return "";
            }

            // Resolve the global RoleMap. RoleType lives in the synopsys.arc
            // package — unlike RoleMap/Role/PermissionEntry, which are under
            // com.michelin.cio (split-package layout in role-strategy).
            Class<?> roleTypeCls = Class.forName(
                "com.synopsys.arc.jenkins.plugins.rolestrategy.RoleType",
                false,
                strat.getClass().getClassLoader());
            // RoleType.Global is a public static final enum constant.
            Object globalType = roleTypeCls.getField("Global").get(null);
            Object roleMap = strat.getClass().getMethod("getRoleMap", roleTypeCls).invoke(strat, globalType);

            // Build the user's identity set: raw authorities (no exclusions here —
            // "authenticated" may legitimately be granted a Varroa role).
            Set<String> groupSet = new LinkedHashSet<>();
            for (GrantedAuthority auth : Jenkins.get().getAuthentication2().getAuthorities()) {
                String a = auth.getAuthority();
                if (a != null) {
                    groupSet.add(a);
                }
            }

            // Precedence-ordered roles (system-mite is intentionally excluded).
            String[][] roles = {
                {"varroa:admin", "Admin"},
                {"varroa:operator", "Operator"},
                {"varroa:developer", "Developer"},
                {"varroa:viewer", "Viewer"}
            };

            // Primary typed path: getSidEntriesForRole -> Set<PermissionEntry> (role-strategy 878.x).
            java.lang.reflect.Method getSidEntriesMethod = null;
            try {
                getSidEntriesMethod = roleMap.getClass().getMethod("getSidEntriesForRole", String.class);
            } catch (NoSuchMethodException e) {
                getSidEntriesMethod = null;
            }

            java.lang.reflect.Method mGetSid = null;
            java.lang.reflect.Method mGetType = null;

            for (String[] role : roles) {
                String roleId = role[0];
                String label = role[1];

                if (getSidEntriesMethod != null) {
                    Set<?> entries = (Set<?>) getSidEntriesMethod.invoke(roleMap, roleId);
                    if (entries == null) {
                        continue;
                    }
                    for (Object entry : entries) {
                        if (mGetSid == null) {
                            mGetSid = entry.getClass().getMethod("getSid");
                            mGetType = entry.getClass().getMethod("getType");
                        }
                        String sid = (String) mGetSid.invoke(entry);
                        String type = String.valueOf(mGetType.invoke(entry));

                        boolean userMatch = ("USER".equals(type) || "EITHER".equals(type)) && sid.equals(userId);
                        boolean groupMatch = ("GROUP".equals(type) || "EITHER".equals(type)) && groupSet.contains(sid);
                        if (userMatch || groupMatch) {
                            return label;
                        }
                    }
                } else {
                    // Fallback: deprecated getSidsForRole -> Set<String> with prefixed entries.
                    Set<String> sids;
                    try {
                        @SuppressWarnings("unchecked")
                        Set<String> raw = (Set<String>) roleMap.getClass()
                            .getMethod("getSidsForRole", String.class).invoke(roleMap, roleId);
                        sids = raw;
                    } catch (NoSuchMethodException e2) {
                        continue;
                    }
                    if (sids == null) {
                        continue;
                    }
                    for (String raw : sids) {
                        String stripped = raw.replaceFirst("(?i)^(user:|group:|role:)", "");
                        if (stripped.equals(userId) || groupSet.contains(stripped)) {
                            return label;
                        }
                    }
                }
            }

            return "";
        }
    }
}
