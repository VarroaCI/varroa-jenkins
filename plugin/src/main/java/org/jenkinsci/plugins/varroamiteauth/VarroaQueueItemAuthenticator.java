package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Queue;
import java.util.List;
import jenkins.security.QueueItemAuthenticator;
import jenkins.security.QueueItemAuthenticatorDescriptor;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Authenticates a queued task as the tenant service identity that owns it.
 *
 * <p>The {@link Queue.Task} overload holds the logic.
 * {@code hudson.model.queue.Tasks.getAuthenticationOf2} calls that overload,
 * and that is the path a Pipeline {@code node} block takes. The
 * {@link Queue.Item} overload delegates to it with {@code item.task}: core's
 * default {@code authenticate2(Queue.Task)} would otherwise build a
 * synthetic {@code Queue.WaitingItem} to reach the {@code Item} overload,
 * incrementing core's queue-item counter on every call.
 */
public class VarroaQueueItemAuthenticator extends QueueItemAuthenticator {

    @Override
    public Authentication authenticate2(Queue.Task task) {
        String tenant = VarroaTenantResolver.tenantOf(task);
        if (tenant == null) {
            return null;
        }
        return new UsernamePasswordAuthenticationToken(
            "svc-" + tenant, "",
            List.of(
                new SimpleGrantedAuthority("authenticated"),
                new SimpleGrantedAuthority("ROLE:varroa:tenant:" + tenant)));
    }

    @Override
    public Authentication authenticate2(Queue.Item item) {
        return authenticate2(item.task);
    }

    /**
     * A trivial descriptor. {@link QueueItemAuthenticator} is
     * {@code Describable}; a diagnostic caller that reaches
     * {@code getDescriptor()} would otherwise throw.
     */
    @Extension
    public static final class DescriptorImpl extends QueueItemAuthenticatorDescriptor {

        @Override
        public String getDisplayName() {
            return "Varroa tenant queue item authenticator";
        }
    }
}
