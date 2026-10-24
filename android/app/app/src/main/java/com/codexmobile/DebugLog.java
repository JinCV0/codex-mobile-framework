package com.codexmobile;

import android.content.Context;
import android.content.SharedPreferences;

import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.text.SimpleDateFormat;
import java.util.ArrayDeque;
import java.util.Date;
import java.util.Locale;

/**
 * 手机端调试：把日志攒起来，需要的时候连同截图一起发回电脑。
 *
 * 为什么放在原生而不是网页里：
 *  1. 网页自己坏掉的时候，正是最需要日志的时候——原生这条路不依赖页面脚本；
 *  2. 截图只有原生抓得到（WebView 内容）；
 *  3. 上传用配对时存下来的地址和口令，网页刷新/断网都不影响。
 */
public final class DebugLog {

    private static final int MAX_LINES = 600;
    private static final ArrayDeque<String> LINES = new ArrayDeque<>();
    private static File file;
    private static final SimpleDateFormat STAMP =
            new SimpleDateFormat("MM-dd HH:mm:ss.SSS", Locale.US);

    private DebugLog() {}

    public static void init(Context ctx) {
        try {
            file = new File(ctx.getFilesDir(), "手机调试.log");
        } catch (Throwable ignored) {}
        add("app", "启动 " + BuildConfig.VERSION_NAME + "（" + BuildConfig.VERSION_CODE + "）");
    }

    /** 记一行。tag 用短的英文（app/web/js/net/shot/crash），方便一眼分类。 */
    public static void add(String tag, String msg) {
        if (msg == null) return;
        String line = STAMP.format(new Date()) + " [" + tag + "] " + msg;
        synchronized (LINES) {
            LINES.addLast(line);
            while (LINES.size() > MAX_LINES) LINES.removeFirst();
        }
        appendFile(line);
    }

    private static void appendFile(String line) {
        if (file == null) return;
        try (FileOutputStream out = new FileOutputStream(file, true)) {
            out.write((line + "\n").getBytes("UTF-8"));
        } catch (Throwable ignored) {}
    }

    /** 取当前攒下的日志（给界面看，也用来上传）。 */
    public static String dump() {
        StringBuilder sb = new StringBuilder();
        synchronized (LINES) {
            for (String l : LINES) sb.append(l).append('\n');
        }
        return sb.toString();
    }

    public static void clear() {
        synchronized (LINES) { LINES.clear(); }
        if (file != null) {
            //noinspection ResultOfMethodCallIgnored
            file.delete();
        }
        add("app", "日志已清空");
    }

    /** 把日志发回电脑。返回一句人话，界面直接显示。 */
    public static String sendLog(Context ctx) {
        return upload(ctx, "log", "手机日志", dump(), Device.info(ctx));
    }

    /**
     * 发一份东西回电脑。native 直接 POST，走配对时存下的地址：
     * serverBase 形如 https://xxx.ts.net，serverToken 就是口令。
     */
    public static String upload(Context ctx, String kind, String name, String data, String note) {
        try {
            SharedPreferences p = ctx.getSharedPreferences(App.PREFS, Context.MODE_PRIVATE);
            String base = p.getString("serverBase", "");
            String token = p.getString("serverToken", "");
            if (base == null || base.isEmpty()) return "还没配对过，传不回去";
            String url = base + "/api/phone/debug?k=" + URLEncoder.encode(token == null ? "" : token, "UTF-8");

            JSONObject body = new JSONObject();
            body.put("kind", kind);
            body.put("name", name);
            body.put("data", data);
            body.put("note", note == null ? "" : note);
            byte[] payload = body.toString().getBytes("UTF-8");

            HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
            conn.setRequestMethod("POST");
            conn.setDoOutput(true);
            conn.setConnectTimeout(8000);
            conn.setReadTimeout(30000);
            conn.setRequestProperty("Content-Type", "application/json; charset=utf-8");
            conn.getOutputStream().write(payload);

            int code = conn.getResponseCode();
            InputStream in = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
            String text = "";
            if (in != null) {
                ByteArrayOutputStream buf = new ByteArrayOutputStream();
                byte[] tmp = new byte[4096];
                int n;
                while ((n = in.read(tmp)) > 0) buf.write(tmp, 0, n);
                in.close();
                text = buf.toString("UTF-8");
            }
            conn.disconnect();
            return code == 200 ? "已传到电脑" : ("电脑回 " + code + "：" + trim(text));
        } catch (Throwable t) {
            return "传不回去：" + t;
        }
    }

    private static String trim(String s) {
        if (s == null) return "";
        s = s.replaceAll("\\s+", " ").trim();
        return s.length() > 120 ? s.substring(0, 120) + "…" : s;
    }
}
