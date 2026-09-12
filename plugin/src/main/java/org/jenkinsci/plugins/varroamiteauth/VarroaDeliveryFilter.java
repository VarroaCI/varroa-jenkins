package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.init.Initializer;
import hudson.util.PluginServletFilter;

import jakarta.servlet.Filter;
import jakarta.servlet.FilterChain;
import jakarta.servlet.FilterConfig;
import jakarta.servlet.ServletException;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.HttpServletRequest;

import java.io.IOException;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Servlet filter that reads the {@code X-Varroa-Delivery} header on every
 * request and records its value with its arrival time in {@link
 * DeliveryRegistry} (D5).
 *
 * <p>Registered via {@link PluginServletFilter#addFilter} from an {@link
 * Initializer}, as {@link HttpActivityFilter} already does.
 */
@Extension
public class VarroaDeliveryFilter implements Filter {

    private static final Logger LOGGER = Logger.getLogger(VarroaDeliveryFilter.class.getName());

    static final String DELIVERY_HEADER = "X-Varroa-Delivery";

    @Initializer
    public static void registerFilter() throws ServletException {
        PluginServletFilter.addFilter(new VarroaDeliveryFilter());
    }

    @Override
    public void init(FilterConfig filterConfig) {
        // Nothing to init.
    }

    @Override
    public void doFilter(ServletRequest request, ServletResponse response, FilterChain chain)
            throws IOException, ServletException {
        try {
            if (request instanceof HttpServletRequest) {
                String value = ((HttpServletRequest) request).getHeader(DELIVERY_HEADER);
                if (value != null && !value.isEmpty()) {
                    DeliveryRegistry.get().record(value);
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaDeliveryFilter: unexpected error reading the delivery header", e);
        }
        chain.doFilter(request, response);
    }

    @Override
    public void destroy() {
        // No resources to release.
    }
}
