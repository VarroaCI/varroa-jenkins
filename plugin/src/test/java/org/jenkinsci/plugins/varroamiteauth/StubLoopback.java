package org.jenkinsci.plugins.varroamiteauth;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.IntFunction;

/**
 * Stub mite loopback listener for the forwarding tests: it serves
 * {@code GET /v1/homes}, {@code POST /v1/claim}, and
 * {@code POST /v1/cross-tenant/build}, and it records every request.
 *
 * <p>Each route answers from its own settable responder, so one test can
 * drive a claim veto and a forward reply in one run.
 */
final class StubLoopback {

    /** One recorded request: its path and its body. */
    static final class Recorded {
        final String path;
        final String body;

        Recorded(String path, String body) {
            this.path = path;
            this.body = body;
        }
    }

    /** One canned reply: a status code and a body. */
    static final class Reply {
        final int status;
        final String body;

        Reply(int status, String body) {
            this.status = status;
            this.body = body;
        }
    }

    static final String HOMES_PATH = "/v1/homes";
    static final String CLAIM_PATH = "/v1/claim";
    static final String FORWARD_PATH = "/v1/cross-tenant/build";

    final List<Recorded> requests = new CopyOnWriteArrayList<>();

    private final AtomicInteger claimCalls = new AtomicInteger();
    private final AtomicInteger forwardCalls = new AtomicInteger();

    private volatile String homesBody = "{\"entries\":[]}";
    private volatile IntFunction<Reply> claimResponder =
            n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");
    private volatile IntFunction<Reply> forwardResponder = n -> new Reply(200, "{\"state\":\"pending\"}");

    private HttpServer server;

    void start() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext(HOMES_PATH, exchange -> answer(exchange, new Reply(200, homesBody)));
        server.createContext(CLAIM_PATH, exchange -> answer(exchange, claimResponder.apply(claimCalls.incrementAndGet())));
        server.createContext(FORWARD_PATH, exchange -> answer(exchange, forwardResponder.apply(forwardCalls.incrementAndGet())));
        server.start();
    }

    void stop() {
        if (server != null) {
            server.stop(0);
        }
    }

    String baseUrl() {
        return "http://127.0.0.1:" + server.getAddress().getPort();
    }

    void setHomesBody(String body) {
        homesBody = body;
    }

    void setClaimResponder(IntFunction<Reply> responder) {
        claimResponder = responder;
    }

    void setForwardResponder(IntFunction<Reply> responder) {
        forwardResponder = responder;
    }

    int claimCallCount() {
        return claimCalls.get();
    }

    int forwardCallCount() {
        return forwardCalls.get();
    }

    /** Every recorded body for one path, oldest first. */
    List<String> bodiesFor(String path) {
        return requests.stream().filter(r -> r.path.equals(path)).map(r -> r.body).toList();
    }

    /** A client pointed at this stub, with its bearer token in {@code tokenFile}. */
    MiteLoopbackClient client(Path tokenFile) throws IOException {
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        return new MiteLoopbackClient(baseUrl(), tf, Duration.ofSeconds(5));
    }

    private void answer(HttpExchange exchange, Reply reply) throws IOException {
        byte[] requestBody = exchange.getRequestBody().readAllBytes();
        requests.add(new Recorded(exchange.getHttpContext().getPath(),
                new String(requestBody, StandardCharsets.UTF_8)));
        byte[] resp = reply.body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(reply.status, resp.length == 0 ? -1 : resp.length);
        if (resp.length > 0) {
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
        }
        exchange.close();
    }
}
