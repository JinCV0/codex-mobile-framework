package com.codexmobile;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * 轮询电脑端通知缓存，把新消息变成手机通知。
 *
 * 这是一个前台服务：Android 要长期在后台听网络流，必须有常驻通知。
 * 常驻那条通知是系统要求，不是我在刷屏；真正的内容通知是另一条。
 */
public class NtfyService extends Service {

    private static final String TAG = "CodexNtfy";
    public static final String CH_WATCH = "codex-watch";
    public static final String CH_PUSH = "suixing-notify";
    private static final int ID_WATCH = 3001;
    private static int pushSeq = 4000;

    public static volatile boolean running = false;

    private Thread worker;
    private volatile boolean stopping = false;

    public static SharedPreferences prefs(Context c) {
        return c.getSharedPreferences(App.PREFS, Context.MODE_PRIVATE);
    }

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm != null) {
            NotificationChannel watch = new NotificationChannel(
                    CH_WATCH, "后台监听", NotificationManager.IMPORTANCE_MIN);
            watch.setShowBadge(false);
            nm.createNotificationChannel(watch);

            NotificationChannel push = new NotificationChannel(
                    CH_PUSH, "服务器通知", NotificationManager.IMPORTANCE_HIGH);
            push.setShowBadge(true);
            nm.createNotificationChannel(push);
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (!startForegroundSafely()) {
            // 系统这会儿不让起前台服务（比如它在后台把我们拉起来，Android 12+
            // 直接抛 ForegroundServiceStartNotAllowedException）。安静收手，
            // 等用户下次打开 App 再起，别把 App 崩掉。
            stopSelf();
            return START_NOT_STICKY;
        }
        if (worker == null) {
            stopping = false;
            running = true;
            worker = new Thread(this::loop, "codex-notify");
            worker.start();
        }
        return START_STICKY;
    }

    /**
     * 起前台服务，类型明确写成 specialUse。
     *
     * 为什么不是 dataSync：Android 15 给它加了"每 24 小时最多 6 小时"的上限，
     * 到点系统会抛 ForegroundServiceDidNotStopInTimeException 把 App 崩掉
     * （2026-10-07 的崩溃就是它）。这个服务要的是长时间连着自己那台电脑，
     * 不属于系统列的任何一类，specialUse 才是对的，而且没有时长上限。
     */
    private boolean startForegroundSafely() {
        try {
            if (Build.VERSION.SDK_INT >= 34) {
                startForeground(ID_WATCH, watchNotification(),
                        ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
            } else {
                startForeground(ID_WATCH, watchNotification());
            }
            return true;
        } catch (Throwable t) {
            Log.w(TAG, "起前台服务失败: " + t);
            DebugLog.add("svc", "起前台服务失败（已安静退出）：" + t);
            return false;
        }
    }

    /**
     * Android 14+ 在前台服务到点时叫这里。
     *
     * 正常不该走到（specialUse 没有时长上限），留着是兜底：万一某个厂商 ROM
     * 自己加限制，这里及时收手只是服务停掉，不会像以前那样把整个 App 崩掉。
     */
    @Override
    public void onTimeout(int startId) {
        Log.w(TAG, "前台服务超时，先安全停下");
        DebugLog.add("svc", "前台服务超时，已安全停下（打开 App 会重新起）");
        stopSelf();
    }

    @Override
    public void onTimeout(int startId, int fgsType) {
        Log.w(TAG, "前台服务超时（type=" + fgsType + "），先安全停下");
        DebugLog.add("svc", "前台服务超时（type=" + fgsType + "），已安全停下");
        stopSelf();
    }

    @Override
    public void onDestroy() {
        stopping = true;
        running = false;
        if (worker != null) {
            worker.interrupt();
            worker = null;
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) { return null; }

    private Notification watchNotification() {
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        return new Notification.Builder(this, CH_WATCH)
                .setSmallIcon(R.drawable.ic_notify)
                .setContentTitle("正在接收服务器通知")
                .setContentText(topicLabel())
                .setContentIntent(pi)
                .setOngoing(true)
                .build();
    }

    private String topicLabel() {
        String u = prefs(this).getString("ntfyUrl", "");
        try { return new URL(u).getPath().replace("/", ""); } catch (Throwable t) { return u; }
    }

    private void loop() {
        while (!stopping) {
            try {
                // 不自建推送通道，只问电脑要它替我们缓存好的消息。
                // 这样手机关机、断网期间也能从电脑端缓存补回。
                SharedPreferences sp = prefs(this);
                String base = sp.getString("serverBase", "").trim();
                String token = sp.getString("serverToken", "").trim();
                if (base.isEmpty()) { sleep(5000); continue; }

                long since = sp.getLong("ntfyLastTime", 0);
                String url = base.replaceAll("/+$", "") + "/api/notifications?k=" + token
                        + (since > 0 ? "&refresh=1" : "&refresh=1");
                HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
                conn.setConnectTimeout(15000);
                conn.setReadTimeout(30000);
                conn.connect();
                StringBuilder sb = new StringBuilder();
                try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream(), "UTF-8"))) {
                    String line;
                    while ((line = r.readLine()) != null) sb.append(line);
                } finally {
                    conn.disconnect();
                }
                JSONObject root = new JSONObject(sb.toString());
                org.json.JSONArray items = root.optJSONArray("items");
                long newest = since;
                if (items != null) {
                    for (int i = items.length() - 1; i >= 0; i--) {
                        JSONObject o = items.getJSONObject(i);
                        long t = o.optLong("time", 0);
                        if (t > newest) newest = t;
                        if (t <= since) continue;
                        notifyItem(o.optString("title", "随行"), o.optString("body", ""));
                    }
                }
                if (newest > since) sp.edit().putLong("ntfyLastTime", newest).apply();
                // 这 60 秒里每 10 秒问一次"电脑有没有让我抓点东西"，
                // 后台也能及时应答，不用等到下一轮通知拉取
                for (int i = 0; i < 6 && !stopping; i++) {
                    sleep(10000);
                    pollDebugCommand(sp);
                }
            } catch (Throwable t) {
                Log.w(TAG, "拉取通知失败，稍后再试: " + t);
                sleep(30000);
            }
        }
    }

    /**
     * 后台也要能应答电脑那边的调试要求（这就是"程序后台可活动"那一条）。
     *
     *  - log / clear：后台直接做，不需要界面；
     *  - shot：截图必须有界面，所以先记一个"待抓"标记，等 App 回到前台立刻补上。
     *
     * 指令取走即清（电脑那边是一次性的），所以前台页面和这里谁先拿到谁处理，
     * "待抓"标记保证即使后台先拿到也不会漏。
     */
    private void pollDebugCommand(SharedPreferences sp) {
        try {
            String base = sp.getString("serverBase", "").trim();
            String token = sp.getString("serverToken", "").trim();
            if (base.isEmpty()) return;
            String url = base.replaceAll("/+$", "") + "/api/phone/command?k=" + token;
            HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
            conn.setConnectTimeout(10000);
            conn.setReadTimeout(15000);
            conn.connect();
            StringBuilder sb = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream(), "UTF-8"))) {
                String line;
                while ((line = r.readLine()) != null) sb.append(line);
            } finally {
                conn.disconnect();
            }
            String cmd = new JSONObject(sb.toString()).optString("cmd", "");
            if (cmd.isEmpty()) return;
            switch (cmd) {
                case "log":
                    DebugLog.sendLog(this);
                    break;
                case "clear":
                    DebugLog.clear();
                    break;
                case "shot":
                    sp.edit().putBoolean("pendingShot", true).apply();
                    DebugLog.add("shot", "电脑要看截图，等界面回到前台就抓");
                    break;
                default:
                    break;
            }
        } catch (Throwable t) {
            Log.w(TAG, "取调试指令失败: " + t);
        }
    }

    private void notifyItem(String title, String msg) {
        if (msg.isEmpty() && title.isEmpty()) return;

        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm == null) return;
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Notification.Builder b = new Notification.Builder(this, CH_PUSH)
                .setSmallIcon(R.drawable.ic_notify)
                .setContentTitle(title.isEmpty() ? "随行" : title)
                .setContentText(msg)
                .setAutoCancel(true)
                .setContentIntent(pi);
        if (!msg.isEmpty()) b.setStyle(new Notification.BigTextStyle().bigText(msg));
        nm.notify(pushSeq++, b.build());
    }

    private void sleep(long ms) {
        try { Thread.sleep(ms); } catch (InterruptedException ignored) { }
    }

    public static void start(Context c) {
        Intent i = new Intent(c, NtfyService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) c.startForegroundService(i);
        else c.startService(i);
    }

    public static void stop(Context c) {
        c.stopService(new Intent(c, NtfyService.class));
    }
}
