package com.codexmobile;

import android.webkit.JavascriptInterface;

/**
 * 网页和手机之间的一小座桥。
 *
 * 网页是整套界面的主体，但有几件事只有原生做得到：
 * 拿到本机版本号、开一个新页面、下载并安装更新。
 */
public class JsBridge {

    private final MainActivity activity;

    public JsBridge(MainActivity activity) {
        this.activity = activity;
    }

    /** 给「我的」页顶部显示的一行信息。 */
    @JavascriptInterface
    public String info() {
        return "随行 v" + versionName() + "（" + versionCode() + "）";
    }

    @JavascriptInterface
    public int versionCode() {
        return BuildConfig.VERSION_CODE;
    }

    @JavascriptInterface
    public String versionName() {
        return BuildConfig.VERSION_NAME;
    }

    /** 用 App 内置的浏览器页打开，不跳出去。 */
    @JavascriptInterface
    public void openUrl(String url, String title) {
        activity.runOnUiThread(() -> activity.openInApp(url, title));
    }

    /** 任务跑完时发一条系统通知。 */
    @JavascriptInterface
    public void notify(String title, String body) {
        activity.runOnUiThread(() -> activity.postDoneNotification(title, body));
    }

    /** 触感反馈：发送、拍板这些关键动作用。milliseconds 10~30 合适，太长会吵。 */
    @JavascriptInterface
    public void haptic(int milliseconds) {
        activity.runOnUiThread(() -> activity.vibrate(milliseconds));
    }

    /** 让用户确认通知权限和提醒样式。 */
    @JavascriptInterface
    public void testNotify() {
        activity.runOnUiThread(activity::testNotification);
    }

    /** 拉起系统语音识别；结果经 window.__voiceResult(text) 回给网页。 */
    @JavascriptInterface
    public void voice() {
        activity.runOnUiThread(activity::startVoiceInput);
    }

    /**
     * 取走"分享到随行"攒下的文本（并清空）。没有分享返回空串。
     * 冷启动时分享先到、页面后到，所以网页每次 ready 都该问一次。
     */
    @JavascriptInterface
    public String lastShare() {
        android.content.SharedPreferences prefs =
                activity.getSharedPreferences(App.PREFS, android.content.Context.MODE_PRIVATE);
        String text = prefs.getString("pendingShare", "");
        if (!text.isEmpty()) {
            prefs.edit().remove("pendingShare").apply();
        }
        return text;
    }

    /** 下载并弹出安装。 */
    @JavascriptInterface
    public void installUpdate(String url) {
        activity.runOnUiThread(() -> Updater.downloadAndInstall(activity, url));
    }

    /** 是否正在接收服务器通知。 */
    @JavascriptInterface
    public boolean ntfyRunning() {
        return NtfyService.running;
    }

    /** 开/关服务器通知订阅。url 是 ntfy 频道地址。 */
    @JavascriptInterface
    public void setNtfy(String url, boolean on) {
        activity.runOnUiThread(() -> activity.configureNtfy(url, on));
    }

    /** 重新扫码配对。 */
    @JavascriptInterface
    public void repair() {
        activity.runOnUiThread(activity::startScan);
    }

    /**
     * 跳到系统的"自启动管理"。
     *
     * 国内厂商在系统层还拦了一道自启动白名单，只能用户手动开；这一层没有
     * 统一 API，原生那边挨个试各家设置的类名。网页「我的」里给个入口。
     */
    @JavascriptInterface
    public void openAutostart() {
        activity.runOnUiThread(activity::openAutostartSettings);
    }

    /**
     * 把一张图存到手机里。url 是电脑那边的图片接口地址（带口令）。
     * App 里交给系统下载器，落到公共「下载」目录，不需要额外权限。
     */
    @JavascriptInterface
    public void download(String url) {
        activity.runOnUiThread(() -> activity.downloadFile(url));
    }

    /**
     * 网页每次拿到 /api/state 都把电脑的局域网入口交过来存一份。
     *
     * 电脑的局域网地址是路由器 DHCP 发的，换网络、续租都会变，所以是"每次刷新"，
     * 不是装一次记一辈子。App 下次启动或回到前台时先拿这份地址探一下，
     * 通就直接连局域网（快一个数量级），不通就回公网入口。
     */
    @JavascriptInterface
    public void setLanUrls(String json) {
        try {
            org.json.JSONArray arr = new org.json.JSONArray(json == null ? "[]" : json);
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < arr.length(); i++) {
                String u = arr.optString(i, "").trim();
                if (u.isEmpty()) continue;
                if (sb.length() > 0) sb.append('\n');
                sb.append(u);
            }
            String out = sb.toString();
            String old = activity.getSharedPreferences(App.PREFS, android.content.Context.MODE_PRIVATE)
                    .getString("lanUrls", "");
            if (out.equals(old)) return;
            activity.getSharedPreferences(App.PREFS, android.content.Context.MODE_PRIVATE)
                    .edit().putString("lanUrls", out).apply();
            DebugLog.add("net", "电脑的局域网入口已更新：" + out.replace('\n', ' '));
        } catch (Throwable t) {
            DebugLog.add("net", "存局域网入口失败：" + t);
        }
    }

    // ---------------- 调试 ----------------

    /** 网页里 console.log / 报错都往这儿送，和原生日志汇成一份。 */
    @JavascriptInterface
    public void log(String tag, String message) {
        DebugLog.add(tag == null || tag.isEmpty() ? "js" : tag, message);
    }

    /** 看本机攒下的日志（界面上"查看日志"用）。 */
    @JavascriptInterface
    public String debugLog() {
        return DebugLog.dump();
    }

    /** 清空本机日志。 */
    @JavascriptInterface
    public void clearLog() {
        DebugLog.clear();
    }

    /** 把日志发回电脑，返回一句人话。 */
    @JavascriptInterface
    public String sendLog() {
        String r = DebugLog.sendLog(activity);
        DebugLog.add("log", "手动发日志：" + r);
        return r;
    }

    /**
     * 抓一张当前界面的截图并发回电脑。
     * 截图必须在 UI 线程做，而网桥调用不在 UI 线程，所以这里等它一下（最多 15 秒）。
     */
    @JavascriptInterface
    public String screenshot(String name) {
        final String[] out = new String[1];
        final java.util.concurrent.CountDownLatch latch = new java.util.concurrent.CountDownLatch(1);
        activity.runOnUiThread(() -> {
            out[0] = activity.captureAndSendScreen(name);
            latch.countDown();
        });
        try {
            latch.await(15, java.util.concurrent.TimeUnit.SECONDS);
        } catch (InterruptedException ignored) {
        }
        return out[0] == null ? "抓图失败" : out[0];
    }

    /** 机型、系统、WebView 版本、屏幕——出问题时先要这些。 */
    @JavascriptInterface
    public String deviceInfo() {
        return Device.info(activity);
    }
}
