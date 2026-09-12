package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import java.util.Collections;
import java.util.List;
import jenkins.security.QueueItemAuthenticator;
import jenkins.security.QueueItemAuthenticatorProvider;

/**
 * Publishes {@link VarroaQueueItemAuthenticator} at an ordinal above every
 * other queue item authenticator provider Varroa depends on.
 *
 * <p>Core's own {@code QueueItemAuthenticatorConfiguration.ProviderImpl}
 * carries ordinal 100. The pinned {@code workflow-durable-task-step}
 * registers
 * {@code ExecutorStepExecution.PlaceholderTask.AuthenticationFromBuild} at
 * ordinal 959; that provider answers with the anonymous principal when no
 * user record exists, for every Pipeline {@code node} block.
 * {@code hudson.model.queue.Tasks.getAuthenticationOf2} takes the first
 * non-null answer in descending ordinal order, so 1000 is the only ordinal
 * that wins a {@code node} block.
 */
@Extension(ordinal = 1000)
public class VarroaQueueItemAuthenticatorProvider extends QueueItemAuthenticatorProvider {

    private final VarroaQueueItemAuthenticator authenticator = new VarroaQueueItemAuthenticator();

    @Override
    public List<QueueItemAuthenticator> getAuthenticators() {
        return Collections.singletonList(authenticator);
    }
}
