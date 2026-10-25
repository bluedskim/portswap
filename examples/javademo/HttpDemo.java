import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;

/**
 * portswap 시연용 HTTP 서버. 환경변수 PORT 로 127.0.0.1 에 리슨하고 /health 와 / 를 제공한다.
 * APP_VERSION 환경변수(또는 -Dapp.version)가 응답에 포함된다.
 */
public class HttpDemo {
    public static void main(String[] args) throws Exception {
        String portEnv = System.getenv("PORT");
        if (portEnv == null || portEnv.isEmpty()) {
            System.err.println("PORT 환경변수가 필요합니다");
            System.exit(1);
        }
        int port = Integer.parseInt(portEnv);
        final String version = resolveVersion();

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
        server.setExecutor(Executors.newCachedThreadPool());

        server.createContext("/health", ex -> {
            if ("true".equalsIgnoreCase(System.getenv("FAIL_HEALTH"))) {
                respond(ex, 503, "unhealthy");
            } else {
                respond(ex, 200, "ok");
            }
        });

        long pid = ProcessHandle.current().pid();
        server.createContext("/", ex -> {
            String body = "pid=" + pid + " version=" + version + "\n";
            respond(ex, 200, body);
        });

        server.start();
        System.err.println("HttpDemo listening on 127.0.0.1:" + port + " version=" + version);

        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            server.stop(5);
        }));

        // SIGTERM 은 JVM 종료로 전달되므로 shutdown hook 으로 연결을 닫는다.
        Thread.currentThread().join();
    }

    private static String resolveVersion() {
        String v = System.getProperty("app.version");
        if (v == null || v.isEmpty()) {
            v = System.getenv("APP_VERSION");
        }
        if (v == null || v.isEmpty()) {
            v = "unknown";
        }
        return v;
    }

    private static void respond(HttpExchange ex, int code, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().set("Content-Type", "text/plain; charset=utf-8");
        ex.sendResponseHeaders(code, bytes.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(bytes);
        }
    }
}
