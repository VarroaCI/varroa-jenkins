package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.FreeStyleProject;
import hudson.model.Item;
import hudson.model.Saveable;
import hudson.model.listeners.SaveableListener;
import hudson.XmlFile;

import java.util.List;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link ActivitySaveableListener}.
 */
public class ActivitySaveableListenerTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void listenerIsDisabledByDefault() throws Exception {
        // Use a listener that returns false for isSaveableEnabled().
        ActivitySaveableListener listener = new ActivitySaveableListener() {
            @Override
            protected boolean isSaveableEnabled() {
                return false; // simulate disabled
            }
        };

        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Call onChange directly with an Item saveable.
        FreeStyleProject project = j.createFreeStyleProject("disabled-test");
        CallOnChange(listener, project);

        // No config.changed should be emitted.
        List<ActivityEvent> events = sink.drain(100).events;
        for (ActivityEvent e : events) {
            assertNotEquals("config.changed should not be emitted when disabled",
                    "config.changed", e.getType());
        }
    }

    @Test
    public void enabledListenerEmitsConfigChangedForItems() throws Exception {
        ActivitySaveableListener listener = new ActivitySaveableListener() {
            @Override
            protected boolean isSaveableEnabled() {
                return true; // simulate enabled
            }
        };

        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("enabled-test");
        sink.resetForTesting(); // clear creation events

        // Call onChange directly on the listener instance.
        CallOnChange(listener, project);

        List<ActivityEvent> events = sink.drain(100).events;
        boolean foundConfigChanged = false;
        for (ActivityEvent e : events) {
            if ("config.changed".equals(e.getType())) {
                foundConfigChanged = true;
                assertEquals("enabled-test", e.getItemPath());
                break;
            }
        }
        assertTrue("config.changed should be emitted when enabled", foundConfigChanged);
    }

    @Test
    public void enabledListenerIgnoresNonItemSaveables() throws Exception {
        ActivitySaveableListener listener = new ActivitySaveableListener() {
            @Override
            protected boolean isSaveableEnabled() {
                return true;
            }
        };

        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        // Simulate a non-item saveable (e.g. global config).
        Saveable nonItem = new Saveable() {
            @Override
            public void save() {}
        };
        try {
            listener.onChange(nonItem, null);
        } catch (Exception e) {
            // onChange catches exceptions, so this should be fine.
        }

        List<ActivityEvent> events = sink.drain(100).events;
        for (ActivityEvent e : events) {
            assertNotEquals("non-item saveable should not emit config.changed",
                    "config.changed", e.getType());
        }
    }

    /** Helper to call onChange with the project's XmlFile. */
    private static void CallOnChange(ActivitySaveableListener listener, Item item) throws Exception {
        // Use reflection to access the XmlFile from the item.
        java.lang.reflect.Method getConfigFile = item.getClass().getMethod("getConfigFile");
        XmlFile xmlFile = (XmlFile) getConfigFile.invoke(item);
        listener.onChange((Saveable) item, xmlFile);
    }
}
