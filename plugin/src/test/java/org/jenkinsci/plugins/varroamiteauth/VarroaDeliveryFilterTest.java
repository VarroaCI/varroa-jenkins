package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

import jakarta.servlet.FilterChain;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/** Tests for {@link VarroaDeliveryFilter} — plain JUnit, no JenkinsRule. */
public class VarroaDeliveryFilterTest {

    @Before
    @After
    public void resetRegistry() {
        DeliveryRegistry.get().resetForTesting();
    }

    @Test
    public void headerValueIsRecorded() throws Exception {
        VarroaDeliveryFilter filter = new VarroaDeliveryFilter();
        HttpServletRequest req = mock(HttpServletRequest.class);
        HttpServletResponse rsp = mock(HttpServletResponse.class);
        FilterChain chain = mock(FilterChain.class);
        when(req.getHeader("X-Varroa-Delivery")).thenReturn("github:d-1");

        filter.doFilter(req, rsp, chain);

        assertEquals("github:d-1", DeliveryRegistry.get().newest());
        verify(chain).doFilter(req, rsp);
    }

    @Test
    public void missingHeaderRecordsNothing() throws Exception {
        VarroaDeliveryFilter filter = new VarroaDeliveryFilter();
        HttpServletRequest req = mock(HttpServletRequest.class);
        HttpServletResponse rsp = mock(HttpServletResponse.class);
        FilterChain chain = mock(FilterChain.class);
        when(req.getHeader("X-Varroa-Delivery")).thenReturn(null);

        filter.doFilter(req, rsp, chain);

        assertNull(DeliveryRegistry.get().newest());
        verify(chain).doFilter(req, rsp);
    }

    @Test
    public void nonHttpRequestIsPassedThroughUnharmed() throws Exception {
        VarroaDeliveryFilter filter = new VarroaDeliveryFilter();
        jakarta.servlet.ServletRequest req = mock(jakarta.servlet.ServletRequest.class);
        jakarta.servlet.ServletResponse rsp = mock(jakarta.servlet.ServletResponse.class);
        FilterChain chain = mock(FilterChain.class);

        filter.doFilter(req, rsp, chain);

        verify(chain).doFilter(req, rsp);
    }
}
