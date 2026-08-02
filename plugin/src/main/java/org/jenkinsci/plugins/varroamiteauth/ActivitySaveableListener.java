package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Item;
import hudson.model.Saveable;
import hudson.model.listeners.SaveableListener;
import hudson.XmlFile;

import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * {@link SaveableListener} that emits {@code config.changed} events into the
 * {@link ActivitySink}.
 *
 * <p>Disabled by default ({@code VARROA_ACTIVITY_SAVEABLE} must be {@code "1"}
 * or {@code "true"} to activate). When enabled, emits only for {@link Item}
 * saveables (job/folder config), skipping non-item saveables (global config,
 * queue, fingerprints).
 */
@Extension
public class ActivitySaveableListener extends SaveableListener {

    private static final Logger LOGGER = Logger.getLogger(ActivitySaveableListener.class.getName());

    @Override
    public void onChange(Saveable saveable, XmlFile file) {
        if (!isSaveableEnabled()) {
            return;
        }
        try {
            if (saveable instanceof Item) {
                Item item = (Item) saveable;
                String path = item.getFullName();
                String actor = ActivityItemListener.resolveActor();
                String message = "Config changed: " + path;

                ActivityEvent event = ActivityEvent.builder()
                        .type("config.changed")
                        .itemPath(path)
                        .actor(actor)
                        .message(message)
                        .build();

                ActivitySink.get().offer(event);
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivitySaveableListener.onChange caught exception", e);
        }
    }

    /**
     * Returns whether saveable events are enabled based on the
     * {@code VARROA_ACTIVITY_SAVEABLE} environment variable.
     *
     * <p>Truthy values: {@code "1"} or {@code "true"} (case-insensitive).
     * This method is overridable so tests can flip the flag without
     * environment injection.
     */
    protected boolean isSaveableEnabled() {
        String raw = System.getenv("VARROA_ACTIVITY_SAVEABLE");
        if (raw == null) {
            return false;
        }
        raw = raw.trim().toLowerCase();
        return "1".equals(raw) || "true".equals(raw);
    }
}
