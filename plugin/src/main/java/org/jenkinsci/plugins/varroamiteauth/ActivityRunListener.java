package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Cause;
import hudson.model.Cause.UpstreamCause;
import hudson.model.Cause.UserIdCause;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.model.listeners.RunListener;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;

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
     * Resolves the actor from the build's causes per the precedence defined in
     * the design (§3.4):
     * <ol>
     *   <li>First {@link UserIdCause#getUserId()}
     *   <li>{@link SCMTrigger.SCMTriggerCause} or {@link Cause.RemoteCause} → {@code "scm"}
     *   <li>{@link TimerTrigger.TimerTriggerCause} → {@code "timer"}
     *   <li>{@link UpstreamCause} → {@code "upstream:<upstreamProject>"}
     *   <li>{@code "unknown"}
     * </ol>
     */
    static String resolveActor(Run<?, ?> run) {
        return resolveActorFromCauses(run.getCauses());
    }

    /**
     * Package-private helper that resolves an actor from a list of causes.
     * Exposed for direct testing without needing a full Run instance.
     */
    static String resolveActorFromCauses(List<Cause> causes) {
        for (Cause cause : causes) {
            if (cause instanceof UserIdCause) {
                String userId = ((UserIdCause) cause).getUserId();
                if (userId != null && !userId.isEmpty()) {
                    return userId;
                }
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof SCMTrigger.SCMTriggerCause
                    || cause instanceof Cause.RemoteCause) {
                return "scm";
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof TimerTrigger.TimerTriggerCause) {
                return "timer";
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof UpstreamCause) {
                return "upstream:" + ((UpstreamCause) cause).getUpstreamProject();
            }
        }
        return "unknown";
    }
}
