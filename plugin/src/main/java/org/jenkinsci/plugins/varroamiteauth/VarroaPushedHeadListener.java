package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import jenkins.scm.api.SCMEvent;
import jenkins.scm.api.SCMEventListener;
import jenkins.scm.api.SCMHeadEvent;

/**
 * Records each create or update head event in {@link PushedHeadRegistry}.
 *
 * <p>Jenkins calls the listeners with the highest ordinal first. The ordinal
 * must stay above the ordinal of the branch-api listeners, so that a push is
 * recorded before branch-api indexes the new branch job.
 */
@Extension(ordinal = 1000)
public class VarroaPushedHeadListener extends SCMEventListener {

    @Override
    public void onSCMHeadEvent(SCMHeadEvent<?> event) {
        SCMEvent.Type type = event.getType();
        if (type == SCMEvent.Type.CREATED || type == SCMEvent.Type.UPDATED) {
            PushedHeadRegistry.get().record(event);
        }
    }
}
