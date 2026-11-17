import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.HexFormat;
import java.util.regex.*;

// Oracle reads one hex-encoded UTF-8 pattern per line from stdin. For each
// line it prints 1 when Pattern.compile accepts the pattern and 0 when it
// throws PatternSyntaxException.
public class Oracle {
    public static void main(String[] a) throws Exception {
        BufferedReader r = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
        PrintWriter w = new PrintWriter(new BufferedWriter(new OutputStreamWriter(System.out, StandardCharsets.UTF_8)));
        HexFormat hf = HexFormat.of();
        String line;
        while ((line = r.readLine()) != null) {
            String p = new String(hf.parseHex(line), StandardCharsets.UTF_8);
            try {
                Pattern.compile(p);
                w.println("1");
            } catch (PatternSyntaxException e) {
                w.println("0");
            }
        }
        w.flush();
    }
}
