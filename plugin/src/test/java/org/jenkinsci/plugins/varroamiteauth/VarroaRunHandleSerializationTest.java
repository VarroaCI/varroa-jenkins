package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import hudson.util.XStream2;

import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.junit.Test;

/**
 * Proves the run-handle map (D1) survives the exact serialization
 * mechanism a CPS program save uses: {@link XStream2}, the same class
 * {@code CpsFlowExecution} persists a program's local variables with.
 *
 * <p>{@code varroa.VarroaRunHandle} itself is a CPS Groovy resource
 * compiled only inside a live {@code CpsFlowExecution}, so its full
 * pipeline round trip is covered instead by {@link VarroaGlobalVariableTest}
 * running the whole build to completion; a genuine build-to-restart-to-resume
 * proof needs a loopback stub that survives a Jenkins restart, which the
 * {@code RestartableJenkinsRule} in-process teardown does not offer.
 */
public class VarroaRunHandleSerializationTest {

    @Test
    public void theHandleMapRoundTripsThroughXStream() {
        Map<String, Object> handle = new LinkedHashMap<>();
        handle.put("runId", "run-42");
        handle.put("tenant", "beta");
        handle.put("job", "beta/deploy");
        handle.put("buildNumber", 7);
        handle.put("state", "completed");
        handle.put("result", "SUCCESS");
        handle.put("startedAt", "2026-01-01T00:00:00Z");
        handle.put("endedAt", "2026-01-01T00:05:00Z");
        handle.put("url", "http://varroa.test/run-42");
        handle.put("artifacts", List.of(Map.of("path", "out.jar")));
        handle.put("intentKey", "upstream:run-1:s1");
        handle.put("callerJobPath", "alpha/caller");

        XStream2 xstream = new XStream2();
        String xml = xstream.toXML(handle);
        @SuppressWarnings("unchecked")
        Map<String, Object> reloaded = (Map<String, Object>) xstream.fromXML(xml);

        assertEquals(handle, reloaded);
    }
}
