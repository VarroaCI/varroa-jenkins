package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Cause;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.model.listeners.RunListener;

import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

/**
 * {@link hudson.model.listeners.RunListener} that emits {@code build.started}
 * and {@code build.completed} events into the {@link ActivitySink}.
 *
 * <p>Build events carry the job full name as {@code itemPath}, the build number
 * as {@code buildNumber}, and the result (for completed builds). Actor is
 * resolved from the build's causes.
 */
@Extension
public class ActivityRunListener extends RunListener<Run<?, ?>> {

    private static final Logger LOGGER = Logger.getLogger(ActivityRunListener.class.getName());

    @Override
    public void onStarted(Run<?, ?> run, TaskListener listener) {
        try {
            String path = run.getParent().getFullName();
            int number = run.getNumber();
            String url = run.getUrl();
            String actor = resolveActor(run);
            String message = "Build #" + number + " of " + path + " started";

            ActivityEvent event = ActivityEvent.builder()
                    .type("build.started")
                    .itemPath(path)
                    .buildNumber(number)
                    .result("")
                    .url(url)
                    .actor(actor)
                    .message(message)
                    .build();

            ActivitySink.get().offer(event);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityRunListener.onStarted caught exception", e);
        }
    }

    @Override
    public void onCompleted(Run<?, ?> run, TaskListener listener) {
        try {
            String path = run.getParent().getFullName();
            int number = run.getNumber();
            String url = run.getUrl();
            String actor = resolveActor(run);
            String result = run.getResult() != null ? run.getResult().toString() : "";
            String message = "Build #" + number + " of " + path + " completed: " + result;

            ActivityEvent event = ActivityEvent.builder()
                    .type("build.completed")
                    .itemPath(path)
                    .buildNumber(number)
                    .result(result)
                    .url(url)
                    .actor(actor)
                    .message(message)
                    .build();

            ActivitySink.get().offer(event);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityRunListener.onCompleted caught exception", e);
        }
    }

    /**
     * Resolves the actor from the build's causes. {@link
     * VarroaUserCause#actorFromCauses} holds the precedence, and a cause list
     * that names no actor reads as {@code "unknown"}.
     */
    static String resolveActor(Run<?, ?> run) {
        return resolveActorFromCauses(run.getCauses());
    }

    /**
     * Package-private wrapper around {@link VarroaUserCause#actorFromCauses},
     * which holds the precedence. It stays package-private, and takes a cause
     * list, so the unit tests read it without a full Run instance. A list that
     * names no actor reads as {@code "unknown"}, this event's own last resort.
     */
    static String resolveActorFromCauses(List<Cause> causes) {
        String actor = VarroaUserCause.actorFromCauses(causes);
        return actor != null ? actor : "unknown";
    }
}
