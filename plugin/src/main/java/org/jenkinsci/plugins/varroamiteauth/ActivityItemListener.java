package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Item;
import hudson.security.ACL;
import hudson.model.listeners.ItemListener;

import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

/**
 * {@link ItemListener} that emits {@code item.created}, {@code item.updated},
 * {@code item.deleted}, and {@code item.moved} events into the {@link ActivitySink}.
 *
 * <p>Only {@code onLocationChanged} is overridden for move/rename (not
 * {@code onRenamed}) to avoid double-emitting. Actor is resolved from the
 * current authentication context.
 */
@Extension
public class ActivityItemListener extends ItemListener {

    private static final Logger LOGGER = Logger.getLogger(ActivityItemListener.class.getName());

    @Override
    public void onCreated(Item item) {
        try {
            emit(item, "item.created", "Created " + item.getFullName());
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityItemListener.onCreated caught exception", e);
        }
    }

    @Override
    public void onUpdated(Item item) {
        try {
            emit(item, "item.updated", "Updated " + item.getFullName());
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityItemListener.onUpdated caught exception", e);
        }
    }

    @Override
    public void onDeleted(Item item) {
        try {
            emit(item, "item.deleted", "Deleted " + item.getFullName());
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityItemListener.onDeleted caught exception", e);
        }
    }

    @Override
    public void onLocationChanged(Item item, String oldFullName, String newFullName) {
        try {
            String actor = resolveActor();
            String message = "Moved " + oldFullName + " \u2192 " + newFullName;

            ActivityEvent event = ActivityEvent.builder()
                    .type("item.moved")
                    .itemPath(newFullName)
                    .url(item.getUrl())
                    .actor(actor)
                    .message(message)
                    .build();

            ActivitySink.get().offer(event);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityItemListener.onLocationChanged caught exception", e);
        }
    }

    private void emit(Item item, String type, String message) {
        String actor = resolveActor();
        ActivityEvent event = ActivityEvent.builder()
                .type(type)
                .itemPath(item.getFullName())
                .url(item.getUrl())
                .actor(actor)
                .message(message)
                .build();
        ActivitySink.get().offer(event);
    }

    /**
     * Resolves the actor from the current authentication context.
     * Returns {@code ""} when the principal is anonymous or the system user.
     */
    static String resolveActor() {
        String name = Jenkins.getAuthentication2().getName();
        if (name == null) {
            return "";
        }
        if ("anonymous".equals(name) || ACL.SYSTEM2.getName().equals(name)) {
            return "";
        }
        return name;
    }
}
