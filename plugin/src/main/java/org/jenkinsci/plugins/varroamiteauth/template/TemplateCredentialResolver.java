package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.model.Item;
import jenkins.model.Jenkins;

import java.lang.reflect.Method;
import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Resolves a template's {@code scmDefinition.credentialsId} in the
 * credential store D7's folder rule allows: the tenant folder's own store
 * only when the template's {@code folder} names a tenant, or the system
 * store when {@code folder} is empty. Never widens the search to an
 * ancestor store.
 *
 * <p>The credentials plugin is not a compile dependency of this plugin, the
 * way {@code role-strategy} is not one for {@code VarroaTenantResolver}.
 * Every step below is reflection wrapped in a catch-all: a reflection
 * failure reports the credential as not found, so the fail-closed rule
 * still holds when the credentials plugin is absent or its API has moved.
 */
final class TemplateCredentialResolver {

    private static final Logger LOGGER = Logger.getLogger(TemplateCredentialResolver.class.getName());

    private TemplateCredentialResolver() {
    }

    /** The outcome of a scoped credential lookup: found, or not found with a reason. */
    static final class Result {
        final boolean found;
        final String message;

        private Result(boolean found, String message) {
            this.found = found;
            this.message = message;
        }

        static Result found() {
            return new Result(true, null);
        }

        static Result notFound(String message) {
            return new Result(false, message);
        }
    }

    /**
     * Resolves {@code credentialsId} in the store {@code folder} allows.
     * An empty {@code credentialsId} always resolves: there is nothing to
     * look up.
     */
    static Result resolve(String folder, String credentialsId) {
        if (credentialsId == null || credentialsId.isEmpty()) {
            return Result.found();
        }
        String f = folder != null ? folder : "";
        try {
            Jenkins jenkins = Jenkins.get();
            Object context;
            String storeLabel;
            if (f.isEmpty()) {
                context = jenkins;
                storeLabel = "the system credential store";
            } else {
                Item folderItem = jenkins.getItemByFullName(f);
                if (folderItem == null) {
                    return Result.notFound("folder \"" + f + "\" does not exist; its credential store is unavailable");
                }
                context = folderItem;
                storeLabel = "folder \"" + f + "\"'s credential store";
            }
            return lookup(context, credentialsId, storeLabel);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "TemplateCredentialResolver: reflective credential lookup failed", e);
            return Result.notFound("credential \"" + credentialsId + "\" could not be resolved: " + e.getMessage());
        }
    }

    private static Result lookup(Object context, String credentialsId, String storeLabel) throws Exception {
        ClassLoader loader = context.getClass().getClassLoader();
        Class<?> providerClass = Class.forName("com.cloudbees.plugins.credentials.CredentialsProvider", false, loader);
        Class<?> modelObjectClass = Class.forName("hudson.model.ModelObject", false, loader);
        Class<?> storeClass = Class.forName("com.cloudbees.plugins.credentials.CredentialsStore", false, loader);
        Class<?> domainClass = Class.forName("com.cloudbees.plugins.credentials.domains.Domain", false, loader);
        Class<?> idCredentialsClass = Class.forName("com.cloudbees.plugins.credentials.common.IdCredentials", false, loader);

        Method lookupStores = providerClass.getMethod("lookupStores", modelObjectClass);
        Method getContext = storeClass.getMethod("getContext");
        Method getDomains = storeClass.getMethod("getDomains");
        Method getCredentials = storeClass.getMethod("getCredentials", domainClass);
        Method getId = idCredentialsClass.getMethod("getId");

        Object stores = lookupStores.invoke(null, context);
        for (Object store : (Iterable<?>) stores) {
            // A store whose context is not this exact context belongs to an
            // ancestor (a parent folder, or the system store reached from a
            // folder). D7 forbids widening the search to it.
            if (getContext.invoke(store) != context) {
                continue;
            }
            for (Object domain : (Iterable<?>) getDomains.invoke(store)) {
                List<?> credentials = (List<?>) getCredentials.invoke(store, domain);
                for (Object credential : credentials) {
                    if (idCredentialsClass.isInstance(credential) && credentialsId.equals(getId.invoke(credential))) {
                        return Result.found();
                    }
                }
            }
        }
        return Result.notFound("credential \"" + credentialsId + "\" was not found in " + storeLabel);
    }
}
