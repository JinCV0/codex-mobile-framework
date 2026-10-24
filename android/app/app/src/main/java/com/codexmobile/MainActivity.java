package com.codexmobile;

import android.Manifest;
import android.app.Activity;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.drawable.GradientDrawable;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.speech.RecognizerIntent;
import android.util.Base64;
import android.text.InputType;
import android.text.method.ScrollingMovementMethod;
import android.view.Gravity;
import android.view.WindowInsets;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebChromeClient;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.io.ByteArrayOutputStream;
import java.util.List;
import java.util.Locale;

import org.json.JSONObject;

import com.google.zxing.integration.android.IntentIntegrator;
import com.google.zxing.integration.android.IntentResult;

/**
 * 手机端 —— 一个纯粹的浏览器壳。
 *
 * 设计取舍：手机里**不跑任何网络组件**。
 * 电脑端的公网地址（Funnel 提供的固定 HTTPS 域名）本身就是可达的，
 * 手机只管用 WebView 打开它。这样 APK 从 40MB 降到 2MB 出头，
 * 也没有原生库、没有前台服务、没有权限纠缠，稳定性高得多。
 *
 * 配对：扫电脑上的二维码 → 拿到完整网址 → 直接加载。
 */
public class MainActivity extends Activity {

    private static final int SCREEN_WELCOME = 0;
    private static final int SCREEN_MAIN = 1;
    private static final int SCREEN_CRASH = 2;
    private static final int SCREEN_INPUT = 3;
    // 网页里点"选择文件"时原生这边的请求码
    private static final int REQUEST_FILE_CHOOSER = 0x4f12;
    // 网页里点话筒时走系统语音识别
    private static final int REQUEST_VOICE = 0x4f13;
    // 网页交给我们、等我们回话的文件选择回调
    private android.webkit.ValueCallback<Uri[]> filePathCallback;

    private SharedPreferences prefs;
    // 网络切换监听器；lastNetDiagAt 用于给常规诊断节流（见 netDiagThrottled）
    private ConnectivityManager.NetworkCallback netCallback;
    private long lastNetDiagAt = 0;
    private FrameLayout root;
    private WebView web;
    private View welcomeScreen;
    private View crashScreen;
    private View inputScreen;
    private View topBar;
    private TextView statusView;
    private LinearLayout loadingOverlay;
    private boolean pageReady = false;
    // 升级后要先咬掉旧外壳缓存（Service Worker + Cache Storage），
    // 等这一页加载完再动手：那两个 API 得在真正的页面里才有效。
    private boolean wipeShellOnLoad = false;

    private String loadedUrl = "";

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        prefs = getSharedPreferences(App.PREFS, MODE_PRIVATE);
        DebugLog.init(this);
        // 开机先记一份现场，之后每次网络切换、主页面加载失败都会再补一份
        NetDiag.log(this, "启动");
        watchNetwork();
        // 上次崩过：把堆栈补进日志，并顺手发回电脑一份（只发一次）
        final String lastCrash = prefs.getString(App.KEY_CRASH, "");
        if (lastCrash != null && !lastCrash.isEmpty()) {
            DebugLog.add("crash", lastCrash);
            if (!prefs.getBoolean("crashSent", false)) {
                prefs.edit().putBoolean("crashSent", true).apply();
                new Thread(() -> DebugLog.upload(this, "log", "崩溃日志",
                        DebugLog.dump(), Device.info(this))).start();
            }
        }

        root = new FrameLayout(this);
        root.setBackgroundColor(Color.parseColor("#101114"));
        setContentView(root);

        // targetSdk 35 起系统强制 edge-to-edge，网页会顶到状态栏下面；
        // 而安卓 WebView 不提供 env(safe-area-inset-*)，所以由原生把
        // 状态栏高度量出来交给 CSS。
        getWindow().getDecorView().setOnApplyWindowInsetsListener((v, insets) -> {
            int top, bottom;
            int ime = 0;
            if (Build.VERSION.SDK_INT >= 30) {
                android.graphics.Insets bars = insets.getInsets(WindowInsets.Type.systemBars());
                top = bars.top;
                bottom = bars.bottom;
                ime = insets.getInsets(WindowInsets.Type.ime()).bottom;
            } else {
                top = insets.getSystemWindowInsetTop();
                bottom = insets.getSystemWindowInsetBottom();
            }
            applyInsets(top, bottom);
            applyIme(ime);
            return insets;
        });

        buildWebView();
        buildTopBar();
        buildWelcomeScreen();
        buildInputScreen();
        buildCrashScreen();

        createNotificationChannel();
        if (!crashTrace().isEmpty()) {
            showScreen(SCREEN_CRASH);
        } else if (hasTarget()) {
            openTarget();
            // 配对成功后第一次回到主界面就把通知权限要了：
            // 审批推送全靠系统通知，等到"测试通知"才申请，前面该收的都收不到。
            maybeAskNotificationPermission();
        } else {
            showScreen(SCREEN_WELCOME);
        }
    }

    /** Android 13+ 要通知权限。只问一次，拒绝过就不再烦人。 */
    private void maybeAskNotificationPermission() {
        if (Build.VERSION.SDK_INT < 33) return;
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) return;
        if (prefs.getBoolean("notifPermAsked", false)) return;
        prefs.edit().putBoolean("notifPermAsked", true).apply();
        requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 2);
    }

    // ------------------------------------------------------------ 配对

    public void startScan() {
        try {
            IntentIntegrator integrator = new IntentIntegrator(this);
            integrator.setDesiredBarcodeFormats(IntentIntegrator.QR_CODE);
            integrator.setPrompt("把电脑屏幕上的二维码放进取景框");
            integrator.setBeepEnabled(false);
            integrator.setOrientationLocked(true);
            integrator.setBarcodeImageEnabled(false);
            integrator.initiateScan();
        } catch (Throwable t) {
            // 扫码这一路不可用时，别把整个 App 拖下水，退回手动输入。
            prefs.edit().putString(App.KEY_CRASH,
                    "扫码启动失败：" + t + "\n" + android.util.Log.getStackTraceString(t)).apply();
            toast("扫码启动失败，请改用手动输入网址");
            showScreen(SCREEN_INPUT);
        }
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        // 网页里的"选择图片/文件"回来了。必须**无论成功取消都回调一次**，
        // 否则网页那边会一直等，输入框里的附件选不了第二次。
        if (requestCode == REQUEST_FILE_CHOOSER) {
            Uri[] uris = null;
            if (resultCode == RESULT_OK && data != null) {
                if (data.getClipData() != null) {
                    int n = data.getClipData().getItemCount();
                    uris = new Uri[n];
                    for (int i = 0; i < n; i++) {
                        uris[i] = data.getClipData().getItemAt(i).getUri();
                    }
                } else if (data.getData() != null) {
                    uris = new Uri[]{ data.getData() };
                }
            }
            if (filePathCallback != null) {
                filePathCallback.onReceiveValue(uris);
                filePathCallback = null;
            }
            return;
        }
        if (requestCode == REQUEST_VOICE) {
            String text = "";
            if (resultCode == RESULT_OK && data != null) {
                List<String> rs = data.getStringArrayListExtra(RecognizerIntent.EXTRA_RESULTS);
                if (rs != null && !rs.isEmpty()) text = rs.get(0);
            }
            deliverVoiceResult(text);
            return;
        }
        IntentResult result = IntentIntegrator.parseActivityResult(requestCode, resultCode, data);
        if (result == null) {
            super.onActivityResult(requestCode, resultCode, data);
            return;
        }
        String contents = result.getContents();
        if (contents == null) {
            showScreen(hasTarget() ? SCREEN_MAIN : SCREEN_WELCOME);
            return;
        }
        if (applyPairing(contents)) {
            toast("配对成功");
            openTarget();
        } else {
            toast("这不是「随行」的配对码");
            showScreen(hasTarget() ? SCREEN_MAIN : SCREEN_WELCOME);
        }
    }

    /**
     * 电脑端生成的配对码：codexmobile://pair?u=<完整网址>&n=<机器名>
     * 同时兼容早期只带域名/端口的格式。
     */
    private boolean applyPairing(String raw) {
        Uri uri;
        try {
            uri = Uri.parse(raw.trim());
        } catch (Throwable t) {
            return false;
        }
        if (!"codexmobile".equalsIgnoreCase(uri.getScheme())) return false;
        if (!"pair".equalsIgnoreCase(uri.getHost())) return false;

        String url = uri.getQueryParameter("u");
        if (url == null || url.trim().isEmpty()) {
            // 兼容旧格式：自己拼一个
            String host = uri.getQueryParameter("h");
            if (host == null || host.trim().isEmpty()) return false;
            String token = uri.getQueryParameter("k") == null ? "" : uri.getQueryParameter("k");
            String port = uri.getQueryParameter("p") == null ? "443" : uri.getQueryParameter("p");
            String scheme = "443".equals(port) ? "https" : "http";
            url = scheme + "://" + host.trim() + "/?k=" + token;
        }
        String name = uri.getQueryParameter("n") == null ? "" : uri.getQueryParameter("n");

        prefs.edit()
                .putString("url", url.trim())
                .putString("machine", name.trim())
                .apply();
        return true;
    }

    private boolean hasTarget() {
        return !prefs.getString("url", "").trim().isEmpty();
    }

    private void openTarget() {
        String url = prefs.getString("url", "").trim();
        if (url.isEmpty()) {
            showScreen(SCREEN_WELCOME);
            return;
        }
        // 记住服务器地址和口令，后台拉通知的服务要用
        // （这里务必记公网地址：它在哪儿都能用，局域网地址换个网络就废了）
        try {
            Uri u = Uri.parse(url);
            String base = u.getScheme() + "://" + u.getHost();
            String k = u.getQueryParameter("k");
            SharedPreferences.Editor ed = prefs.edit().putString("serverBase", base);
            if (k != null) ed.putString("serverToken", k);
            ed.apply();
        } catch (Throwable ignored) { }
        showScreen(SCREEN_MAIN);
        // 先按公网地址记上：onResume 靠 loadedUrl 非空来判断"已经加载过了"，
        // 这里留空的话，启动后紧跟着的 onResume 会再触发一次 openTarget。
        loadedUrl = url;
        // 选地址可能要探一次局域网（最长 1.2 秒），别卡着主线程；
        // 这段空窗期正好盖在 loading 蒙层下面。
        new Thread(() -> {
            String pick = chooseUrl();
            final String target = pick.isEmpty() ? url : pick;
            runOnUiThread(() -> {
                loadedUrl = target;
                web.loadUrl(target);
            });
        }, "pickurl").start();
    }

    /** 存下来的局域网入口（换行分隔）。 */
    private String[] savedLanUrls() {
        String raw = prefs.getString("lanUrls", "");
        if (raw == null || raw.trim().isEmpty()) return new String[0];
        java.util.List<String> out = new java.util.ArrayList<>();
        for (String part : raw.split("\n")) {
            String t = part.trim();
            if (!t.isEmpty()) out.add(t);
        }
        return out.toArray(new String[0]);
    }

    /**
     * 决定这次加载哪个地址。在后台线程调用。
     *
     * 同一个 WiFi 下直连电脑的局域网地址，比绕公网隧道（手机 → Funnel 入口 →
     * Tailscale 骨干 → DERP → 电脑）快一个数量级，也不受 WiFi 出口链路和运营商
     * IPv6 的影响。地址是 DHCP 发的会变，所以每次现探 —— 探不通就老老实实回
     * 公网入口，绝不把用户卡在一条死路上。
     *
     * 只在 WiFi 下试：蜂窝网络里局域网地址必然不通，白等那 1.2 秒。
     */
    private String chooseUrl() {
        String fallback = prefs.getString("url", "").trim();
        if (fallback.isEmpty()) return "";
        String[] lans = savedLanUrls();
        if (lans.length == 0) return fallback;
        if (!NetDiag.onWifi(this)) return fallback;
        for (String u : lans) {
            if (NetDiag.reachable(u, 1200)) {
                DebugLog.add("net", "局域网可达，直连 " + u.replaceAll("[?&]k=.*$", ""));
                return u;
            }
        }
        DebugLog.add("net", "局域网入口都不通（试了 " + lans.length + " 个），走公网入口");
        return fallback;
    }

    /**
     * 网络变了就看看当前地址还合不合适。在后台线程调用。
     *
     * 从 WiFi 走到蜂窝：局域网地址必然断，得切回公网；
     * 从蜂窝回到 WiFi：可以切回局域网，快很多。
     */
    private void maybeSwitchTarget() {
        if (loadedUrl.isEmpty()) return;
        final String pick = chooseUrl();
        if (pick.isEmpty() || pick.equals(loadedUrl)) return;
        runOnUiThread(() -> {
            if (pick.equals(loadedUrl)) return;
            DebugLog.add("net", "网络变化，换个入口重连");
            loadedUrl = pick;
            web.loadUrl(pick);
        });
    }

    // ------------------------------------------------------------ 语音输入

    /** 网页点了话筒：拉起系统自带的语音识别，识别完经 __voiceResult 回给网页。 */
    public void startVoiceInput() {
        try {
            Intent intent = new Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH);
            intent.putExtra(RecognizerIntent.EXTRA_LANGUAGE_MODEL,
                    RecognizerIntent.LANGUAGE_MODEL_FREE_FORM);
            intent.putExtra(RecognizerIntent.EXTRA_LANGUAGE, Locale.getDefault());
            intent.putExtra(RecognizerIntent.EXTRA_MAX_RESULTS, 1);
            intent.putExtra(RecognizerIntent.EXTRA_PARTIAL_RESULTS, false);
            startActivityForResult(intent, REQUEST_VOICE);
        } catch (Throwable t) {
            toast("这台手机没有可用的语音识别");
        }
    }

    /** 把识别结果交回网页；识别取消时也要回一次（空串），网页好把话筒按钮复位。 */
    private void deliverVoiceResult(String text) {
        final String js = "window.__voiceResult && window.__voiceResult("
                + JSONObject.quote(text == null ? "" : text) + ")";
        runOnUiThread(() -> {
            if (web != null) web.evaluateJavascript(js, null);
        });
    }

    // ------------------------------------------------------------ 分享入口

    /**
     * 别的 App「分享到 Codex 随行」：纯文本先进来，网页加载好了再取走。
     * 网页通过 JsBridge.lastShare() 取并清空，取走前一直攒在 prefs 里，
     * 冷启动（网页还没影）也不会丢。
     */
    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        stashShare(intent);
    }

    private void stashShare(Intent intent) {
        if (intent == null) return;
        String text = null;
        if (Intent.ACTION_SEND.equals(intent.getAction())) {
            String extra = intent.getStringExtra(Intent.EXTRA_TEXT);
            if (extra != null && !extra.trim().isEmpty()) text = extra.trim();
        }
        if (text == null) return;
        prefs.edit().putString("pendingShare", text).apply();
        DebugLog.add("app", "收到分享 " + text.length() + " 字");
        deliverShare();
    }

    /** 网页在的时候直接把分享内容推过去。 */
    private void deliverShare() {
        if (!pageReady) return;
        String text = prefs.getString("pendingShare", "");
        if (text.isEmpty()) return;
        prefs.edit().remove("pendingShare").apply();
        final String js = "window.__shareReceived && window.__shareReceived(" + JSONObject.quote(text) + ")";
        runOnUiThread(() -> {
            if (web != null) web.evaluateJavascript(js, null);
        });
    }

    // ------------------------------------------------------------ 界面

    private void buildWebView() {
        // 换了新版本就把 WebView 的缓存清一次。
        //
        // 2026-10-05 的教训：界面是网页，而网页注册了 Service Worker 把外壳
        // 缓在本地。服务端换了新界面，手机却一直从缓存里拿老页面 —— 用户反复
        // 重启 App 都没用（旧的外壳缓存还在，连"自动更新"那段代码都是旧的）。
        // 这是 WebView 应用的标准做法：升级后清一次缓存，一劳永逸。
        int lastCleared = prefs.getInt("cacheClearedFor", 0);
        if (lastCleared != BuildConfig.VERSION_CODE) {
            try {
                WebView tmp = new WebView(this);
                tmp.clearCache(true);
                tmp.destroy();
                android.webkit.CookieManager.getInstance().flush();
            } catch (Throwable ignored) { }
            prefs.edit().putInt("cacheClearedFor", BuildConfig.VERSION_CODE).apply();
            DebugLog.add("app", "升级到 " + BuildConfig.VERSION_CODE + "，已清 WebView 缓存");
            // 上面那一下清不到 Service Worker 缓存的外壳：2026-10-10 就是被它
            // 钉在老界面上（电脑上早改好了，手机十一个小时没换）。页面加载完
            // 再把 SW 注册和缓存一起清掉，然后重新加载一次。
            wipeShellOnLoad = true;
        }
        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setDatabaseEnabled(true);
        // 关掉网页那套缩放：双指放大、双击放大、右下角缩放按钮都不要，
        // 用起来才像手机 App 而不是浏览器（用户反馈"网页味"）。
        s.setSupportZoom(false);
        s.setBuiltInZoomControls(false);
        s.setDisplayZoomControls(false);
        s.setTextZoom(100);
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        s.setCacheMode(WebSettings.LOAD_DEFAULT);
        web.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                DebugLog.add("web", "页面加载完成");
                if (wipeShellOnLoad) {
                    wipeShellOnLoad = false;
                    wipeShellAndReload();
                    return;
                }
                // 页面加载完再注一次内边距，避免首次注入时页面还没准备好
                applyInsets(topInsetPx, bottomInsetPx);
                applyIme(imePx);
                pageReady = true;
                // 页面刚加载好：攒着的分享内容（如果有）现在可以交给网页了
                deliverShare();
                if (loadingOverlay != null) loadingOverlay.setVisibility(View.GONE);
                if (topBar.getVisibility() == View.VISIBLE && !hasProblem()) {
                    topBar.setVisibility(View.GONE);
                }
            }

            @Override
            public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                String desc = error == null ? "" : String.valueOf(error.getDescription());
                // 切后台/切网络时系统会把在途请求掐断，这种不算故障，别刷日志
                boolean aborted = desc.contains("ERR_CONNECTION_ABORTED") || desc.contains("ERR_FAILED");
                if (!aborted) {
                    DebugLog.add("net", "加载失败 " + desc + " " + (request == null ? "" : request.getUrl()));
                }
                // 只关心主页面加载失败：一般是电脑关机了，或者 Funnel 链路不通
                if (request != null && request.isForMainFrame()) {
                    // 主页面打不开正是最需要现场的时刻：网络类型、DNS 解析、各地址连通性一起记下来
                    netDiagNow("主页加载失败 " + desc);
                    // 走公网入口打不开时，如果存过局域网地址就再试一把。WiFi 下公网不通、
                    // 局域网明明能连，这种情况不该让用户自己去处理。
                    new Thread(MainActivity.this::maybeSwitchTarget, "switchurl").start();
                    topBar.setVisibility(View.VISIBLE);
                    statusView.setTextColor(Color.parseColor("#E8756B"));
                    statusView.setText("连不上电脑（" + error.getDescription() + "）· 点这里重试");
                    statusView.setOnClickListener(v -> openTarget());
                }
            }
        });
        // 网页与原生之间的桥：版本号、内置浏览器、下载安装更新
        web.addJavascriptInterface(new JsBridge(this), "CodexApp");
        // 兜底：页面里万一还有 alert，别让它变成"点了没反应"
        web.setWebChromeClient(new WebChromeClient() {
            // 网页里 <input type="file"> 被点到时，必须由原生把系统选择器打开，
            // 否则点上去毫无反应 —— 这是 WebView 应用的经典坑，跟权限无关
            // （相册/文件走系统选择器，本来就不需要额外权限）。
            @Override
            public boolean onShowFileChooser(WebView view,
                                             android.webkit.ValueCallback<Uri[]> callback,
                                             FileChooserParams params) {
                if (filePathCallback != null) {
                    filePathCallback.onReceiveValue(null);
                    filePathCallback = null;
                }
                filePathCallback = callback;
                try {
                    Intent intent = new Intent(Intent.ACTION_GET_CONTENT);
                    intent.addCategory(Intent.CATEGORY_OPENABLE);
                    String type = "*/*";
                    String[] accepts = params == null ? null : params.getAcceptTypes();
                    if (accepts != null && accepts.length > 0 && accepts[0] != null && !accepts[0].isEmpty()) {
                        String first = accepts[0].split(",")[0].trim();
                        if (!first.isEmpty()) type = first;
                    }
                    intent.setType(type);
                    intent.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                    startActivityForResult(Intent.createChooser(intent, "选择图片或文件"), REQUEST_FILE_CHOOSER);
                    return true;
                } catch (Throwable t) {
                    DebugLog.add("app", "打开文件选择器失败：" + t);
                    toast("打不开文件选择器");
                    if (filePathCallback != null) {
                        filePathCallback.onReceiveValue(null);
                        filePathCallback = null;
                    }
                    return false;
                }
            }

            @Override
            public boolean onJsAlert(WebView view, String url, String message, android.webkit.JsResult result) {
                result.confirm();
                toast(message);
                return true;
            }

            @Override
            public boolean onConsoleMessage(android.webkit.ConsoleMessage msg) {
                // 网页里的 console.log / 报错都进本机日志，和原生那条汇在一起
                if (msg != null) {
                    String lv = msg.messageLevel() == android.webkit.ConsoleMessage.MessageLevel.ERROR ? "js-err" : "js";
                    DebugLog.add(lv, msg.message() + "  (" + msg.sourceId() + ":" + msg.lineNumber() + ")");
                }
                return true;
            }
        });
        web.setBackgroundColor(Color.parseColor("#101114"));
        root.addView(web, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        // 首屏加载提示：别让人对着黑屏等
        loadingOverlay = new LinearLayout(this);
        loadingOverlay.setOrientation(LinearLayout.VERTICAL);
        loadingOverlay.setGravity(Gravity.CENTER);
        loadingOverlay.setBackgroundColor(Color.parseColor("#101114"));
        TextView lt = new TextView(this);
        lt.setText("正在连接你的电脑");
        lt.setTextColor(Color.parseColor("#E6E7E9"));
        lt.setTextSize(15f);
        lt.setGravity(Gravity.CENTER);
        loadingOverlay.addView(lt);
        TextView ls = new TextView(this);
        ls.setText("第一次打开会慢几秒");
        ls.setTextColor(Color.parseColor("#6B6D75"));
        ls.setTextSize(12.5f);
        ls.setGravity(Gravity.CENTER);
        ls.setPadding(0, dp(8), 0, 0);
        loadingOverlay.addView(ls);
        root.addView(loadingOverlay, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    private String label() {
        String m = prefs.getString("machine", "");
        return m.isEmpty() ? "已连接" : "已连接 · " + m;
    }

    /** 顶部那条状态条只在有问题时显示，正常情况下不占地方。 */
    private boolean hasProblem() {
        return statusView != null
                && (statusView.getCurrentTextColor() == Color.parseColor("#E8756B")
                 || statusView.getCurrentTextColor() == Color.parseColor("#D9A441"));
    }

    /** 在 App 内打开一个页面（服务卡片走这里，不跳出去）。 */
    public void openInApp(String url, String title) {
        DebugLog.add("app", "在 App 内打开：" + title);
        try {
            Intent i = new Intent(this, WebActivity.class);
            i.putExtra(WebActivity.EXTRA_URL, url);
            if (title != null) i.putExtra(WebActivity.EXTRA_TITLE, title);
            startActivity(i);
        } catch (Throwable t) {
            try { startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url))); }
            catch (Throwable ignored) { toast("打不开这个地址"); }
        }
    }

    private void buildTopBar() {
        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(Color.parseColor("#17181C"));
        bar.setPadding(dp(12), dp(6), dp(8), dp(6));

        statusView = new TextView(this);
        statusView.setTextColor(Color.parseColor("#9A9CA3"));
        statusView.setTextSize(12f);
        statusView.setText("准备中…");
        bar.addView(statusView, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        TextView repo = new TextView(this);
        repo.setText("重新配对");
        repo.setTextColor(Color.parseColor("#69C497"));
        repo.setTextSize(12f);
        repo.setPadding(dp(10), dp(6), dp(6), dp(6));
        repo.setOnClickListener(v -> startScan());
        bar.addView(repo);

        topBar = bar;
        FrameLayout.LayoutParams lp = new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.gravity = Gravity.TOP;
        root.addView(bar, lp);
    }

    private void buildWelcomeScreen() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setGravity(Gravity.CENTER);
        box.setPadding(dp(28), dp(40), dp(28), dp(28));
        box.setBackgroundColor(Color.parseColor("#101114"));

        TextView title = new TextView(this);
        title.setText("随行");
        title.setTextColor(Color.parseColor("#E6E7E9"));
        title.setTextSize(24f);
        title.setGravity(Gravity.CENTER);
        box.addView(title);

        TextView sub = new TextView(this);
        sub.setText("扫一下电脑上的二维码就能开始\n不用输地址，不用记口令");
        sub.setTextColor(Color.parseColor("#9A9CA3"));
        sub.setTextSize(14f);
        sub.setGravity(Gravity.CENTER);
        sub.setPadding(0, dp(14), 0, dp(36));
        box.addView(sub);

        Button scan = new Button(this);
        scan.setText("扫电脑上的码");
        scan.setTextSize(17f);
        scan.setTextColor(Color.parseColor("#101114"));
        scan.setAllCaps(false);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(Color.parseColor("#69C497"));
        bg.setCornerRadius(dp(14));
        scan.setBackground(bg);
        scan.setLayoutParams(new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(54)));
        scan.setOnClickListener(v -> startScan());
        box.addView(scan);

        TextView hint = new TextView(this);
        hint.setText("二维码在哪：电脑右下角托盘图标 →「显示二维码」\n再双击一次启动脚本也会弹出来");
        hint.setTextColor(Color.parseColor("#6B6D75"));
        hint.setTextSize(12.5f);
        hint.setGravity(Gravity.CENTER);
        hint.setPadding(0, dp(22), 0, 0);
        box.addView(hint);

        TextView manual = new TextView(this);
        manual.setText("手动输入网址");
        manual.setTextColor(Color.parseColor("#69C497"));
        manual.setTextSize(14f);
        manual.setGravity(Gravity.CENTER);
        manual.setPadding(0, dp(30), 0, dp(10));
        manual.setOnClickListener(v -> showScreen(SCREEN_INPUT));
        box.addView(manual);

        welcomeScreen = box;
        root.addView(box, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    private void buildInputScreen() {
        ScrollView scroll = new ScrollView(this);
        scroll.setBackgroundColor(Color.parseColor("#101114"));

        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(22), dp(44), dp(22), dp(22));
        scroll.addView(box);

        TextView t = new TextView(this);
        t.setText("手动输入网址");
        t.setTextColor(Color.parseColor("#E6E7E9"));
        t.setTextSize(16f);
        box.addView(t);

        TextView h = new TextView(this);
        h.setText("电脑端托盘菜单 →「显示二维码」，页面上最下面那一行就是。");
        h.setTextColor(Color.parseColor("#9A9CA3"));
        h.setTextSize(13f);
        h.setPadding(0, dp(8), 0, dp(12));
        box.addView(h);

        EditText input = new EditText(this);
        input.setText(prefs.getString("url", ""));
        input.setInputType(InputType.TYPE_TEXT_VARIATION_URI | InputType.TYPE_CLASS_TEXT);
        input.setHint("https://codex-mobile.xxxx.ts.net/?k=...");
        input.setTextColor(Color.parseColor("#E6E7E9"));
        input.setHintTextColor(Color.parseColor("#6B6D75"));
        box.addView(input);

        Button save = new Button(this);
        save.setText("打开");
        save.setAllCaps(false);
        save.setTextColor(Color.parseColor("#101114"));
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(Color.parseColor("#69C497"));
        bg.setCornerRadius(dp(12));
        save.setBackground(bg);
        LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(50));
        blp.topMargin = dp(20);
        save.setLayoutParams(blp);
        save.setOnClickListener(v -> {
            String url = input.getText().toString().trim();
            if (url.isEmpty()) { toast("请填写网址"); return; }
            if (!url.startsWith("http")) url = "https://" + url;
            prefs.edit().putString("url", url).remove("machine").apply();
            openTarget();
        });
        box.addView(save);

        TextView back = new TextView(this);
        back.setText("← 返回扫码");
        back.setTextColor(Color.parseColor("#69C497"));
        back.setTextSize(14f);
        back.setGravity(Gravity.CENTER);
        back.setPadding(0, dp(20), 0, 0);
        back.setOnClickListener(v -> showScreen(hasTarget() ? SCREEN_MAIN : SCREEN_WELCOME));
        box.addView(back);

        inputScreen = scroll;
        root.addView(scroll, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    private String crashTrace() {
        String t = prefs.getString(App.KEY_CRASH, "");
        return t == null ? "" : t;
    }

    private void buildCrashScreen() {
        ScrollView scroll = new ScrollView(this);
        scroll.setBackgroundColor(Color.parseColor("#101114"));

        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(20), dp(44), dp(20), dp(20));
        scroll.addView(box);

        TextView title = new TextView(this);
        title.setText("上次运行崩了");
        title.setTextColor(Color.parseColor("#E8756B"));
        title.setTextSize(18f);
        box.addView(title);

        TextView sub = new TextView(this);
        sub.setText("下面是崩溃堆栈，截图发我就能定位。");
        sub.setTextColor(Color.parseColor("#9A9CA3"));
        sub.setTextSize(13f);
        sub.setPadding(0, dp(8), 0, dp(12));
        box.addView(sub);

        TextView trace = new TextView(this);
        trace.setText(crashTrace());
        trace.setTextColor(Color.parseColor("#E6E7E9"));
        trace.setTextSize(10.5f);
        trace.setTypeface(android.graphics.Typeface.MONOSPACE);
        trace.setMovementMethod(new ScrollingMovementMethod());
        trace.setTextIsSelectable(true);
        box.addView(trace);

        Button clear = new Button(this);
        clear.setText("继续使用");
        clear.setAllCaps(false);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(Color.parseColor("#69C497"));
        bg.setCornerRadius(dp(12));
        clear.setBackground(bg);
        clear.setTextColor(Color.parseColor("#101114"));
        LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(48));
        blp.topMargin = dp(18);
        clear.setLayoutParams(blp);
        clear.setOnClickListener(v -> {
            prefs.edit().remove(App.KEY_CRASH).apply();
            if (hasTarget()) openTarget();
            else showScreen(SCREEN_WELCOME);
        });
        box.addView(clear);

        crashScreen = scroll;
        root.addView(scroll, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    private int topInsetPx = 0;
    private int bottomInsetPx = 0;

    /**
     * 把系统栏高度交给 CSS。安卓 WebView 不实现 env(safe-area-inset-*)，
     * 上边不注入会被状态栏压住，下边不注入底部导航会被手势条压住。
     */
    private void applyInsets(int topPx, int bottomPx) {
        topInsetPx = topPx;
        bottomInsetPx = bottomPx;
        if (topBar != null) {
            topBar.setPadding(topBar.getPaddingLeft(), topPx + dp(6),
                    topBar.getPaddingRight(), topBar.getPaddingBottom());
        }
        float d = getResources().getDisplayMetrics().density;
        String sat = Math.round(topPx / d) + "px";
        String sab = Math.round(bottomPx / d) + "px";
        if (web != null) {
            web.evaluateJavascript(
                    "document.documentElement.style.setProperty('--sat','" + sat + "');" +
                    "document.documentElement.style.setProperty('--sab','" + sab + "')", null);
        }
    }

    private int imePx = 0;
    private int baseHeightPx = 0;

    /**
     * 键盘。
     *
     * targetSdk 35 + 安卓 15 起是强制 edge-to-edge，adjustResize 不再缩短窗口，
     * 系统改成把整窗往上平移去露出输入框 —— 结果顶部那条标题（返回键、对话名）
     * 会被顶出屏幕。这里的做法是自己按键盘高度把 WebView 从底部收起来：
     * 输入框跟着落到键盘上方，系统就不需要再平移了。
     *
     * 老一些的安卓仍然会自己缩窗口，检测到这种情况就不重复收，免得缩两次。
     */
    private void applyIme(int px) {
        imePx = px;
        float d = getResources().getDisplayMetrics().density;
        if (web != null) {
            web.evaluateJavascript("window.__ime=" + Math.round(px / d) + ";"
                    + "window.dispatchEvent(new Event('resize'));", null);
        }
        if (root == null) return;
        root.post(() -> {
            int h = root.getHeight();
            if (px <= 0) {
                // 布局还没跑完时高度是 0，别当成基准
                if (h > 0) baseHeightPx = h;
                setWebBottomMargin(0);
                return;
            }
            if (baseHeightPx > 0 && h <= baseHeightPx - px / 2) {
                setWebBottomMargin(0);
            } else {
                setWebBottomMargin(px);
            }
        });
    }

    private void setWebBottomMargin(int px) {
        if (web == null) return;
        ViewGroup.LayoutParams lp = web.getLayoutParams();
        if (!(lp instanceof FrameLayout.LayoutParams)) return;
        FrameLayout.LayoutParams flp = (FrameLayout.LayoutParams) lp;
        if (flp.bottomMargin == px) return;
        flp.bottomMargin = px;
        web.setLayoutParams(flp);
    }

    private void showScreen(int which) {
        welcomeScreen.setVisibility(which == SCREEN_WELCOME ? View.VISIBLE : View.GONE);
        inputScreen.setVisibility(which == SCREEN_INPUT ? View.VISIBLE : View.GONE);
        crashScreen.setVisibility(which == SCREEN_CRASH ? View.VISIBLE : View.GONE);
        web.setVisibility(which == SCREEN_MAIN ? View.VISIBLE : View.GONE);
        topBar.setVisibility(which == SCREEN_MAIN ? View.VISIBLE : View.GONE);
        if (which == SCREEN_MAIN) {
            statusView.setTextColor(Color.parseColor("#9A9CA3"));
            statusView.setText(label());
            statusView.setOnClickListener(null);
        }
    }

    private void toast(String s) {
        Toast.makeText(this, s, Toast.LENGTH_SHORT).show();
    }

    // ------------------------------------------------------------ 系统通知
    //
    // "任务完成后通知我"就是走这里：网页在一轮跑完时调 JsBridge.notify()，
    // 原生弹一条系统通知。不需要任何第三方推送服务。

    private static final String CH_DONE = "codex-done";
    private static final int NOTIFY_DONE_ID = 2001;

    private void createNotificationChannel() {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm == null) return;
        NotificationChannel ch = new NotificationChannel(CH_DONE, "任务完成", NotificationManager.IMPORTANCE_HIGH);
        ch.setShowBadge(true);
        nm.createNotificationChannel(ch);
    }

    public void postDoneNotification(String title, String body) {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm == null) return;
        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP);
        // 带上正在看的地址：点通知直接回到这条对话，不用先看到首页再点进去
        if (loadedUrl != null && !loadedUrl.isEmpty()) {
            open.setData(Uri.parse(loadedUrl));
        }
        PendingIntent pi = PendingIntent.getActivity(
                this, 0, open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Notification.Builder b = new Notification.Builder(this, CH_DONE)
                .setSmallIcon(R.drawable.ic_notify)
                .setContentTitle(title == null || title.isEmpty() ? "Codex 任务完成" : title)
                .setAutoCancel(true)
                .setContentIntent(pi);
        if (body != null && !body.isEmpty()) {
            b.setStyle(new Notification.BigTextStyle().bigText(body)).setContentText(body);
        }
        nm.notify(NOTIFY_DONE_ID, b.build());
    }

    /** 触感反馈。老系统没有 VibrationEffect，分开处理。 */
    public void vibrate(int ms) {
        if (ms <= 0) ms = 15;
        if (ms > 100) ms = 100;
        android.os.Vibrator v = (android.os.Vibrator) getSystemService(VIBRATOR_SERVICE);
        if (v == null || !v.hasVibrator()) return;
        if (Build.VERSION.SDK_INT >= 26) {
            v.vibrate(android.os.VibrationEffect.createOneShot(ms, android.os.VibrationEffect.DEFAULT_AMPLITUDE));
        } else {
            v.vibrate(ms);
        }
    }

    public void testNotification() {
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 2);
            toast("请先允许通知，然后再试一次");
            return;
        }
        postDoneNotification("Codex 任务完成", "这是一条测试通知，看到就说明通道正常");
    }

    /** 打开/关闭 ntfy 频道订阅。 */
    public void configureNtfy(String url, boolean on) {
        if (url != null && !url.trim().isEmpty()) {
            prefs.edit().putString("ntfyUrl", url.trim()).apply();
        }
        // 记下来：回到前台时按这个决定要不要把后台服务补起来
        prefs.edit().putBoolean("ntfyOn", on).apply();
        if (on) {
            if (Build.VERSION.SDK_INT >= 33
                    && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
                requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 2);
            }
            NtfyService.start(this);
            toast("开始接收服务器通知");
        } else {
            NtfyService.stop(this);
            toast("已停止接收");
        }
    }

    @Override
    public void onBackPressed() {
        // 返回键交给网页决定：先关弹层，再退对话，再回「对话」页。
        // 网页说"我处理不了"才真的退出，免得一下就回到桌面。
        if (web == null || web.getVisibility() != View.VISIBLE || !pageReady) {
            super.onBackPressed();
            return;
        }
        web.evaluateJavascript(
                "(function(){try{return (window.__back&&window.__back())?'1':'0'}catch(e){return '0'}})()",
                value -> {
                    if (value == null || !value.contains("1")) finish();
                });
    }

    @Override
    protected void onResume() {
        super.onResume();
        DebugLog.add("app", "回到前台");
        netDiagThrottled("回前台", 120000);
        ensureNtfyService();
        // 电脑那边要过截图（后台服务收到的），回到前台立刻补上
        if (prefs.getBoolean("pendingShot", false)) {
            prefs.edit().putBoolean("pendingShot", false).apply();
            web.postDelayed(() -> captureAndSendScreen("电脑要求"), 1500);
        }
        if (web.getVisibility() == View.VISIBLE && loadedUrl.isEmpty()) openTarget();
    }

    /**
     * 回到前台时把后台服务补起来。
     *
     * 以前只有"用户手动拨那个开关"才会起服务，于是系统一旦把它收掉
     * （前台服务超时、厂商省电清理、重启手机），通知就静悄悄地永远不来了 ——
     * 用户只能自己再去拨一次开关，还未必知道要这么做。
     * 这里每次回前台都检查一遍：只要还开着，就补起来。
     */
    private void ensureNtfyService() {
        try {
            if (!prefs.getBoolean("ntfyOn", false)) return;
            if (NtfyService.running) return;
            DebugLog.add("svc", "后台服务不在，重新拉起");
            NtfyService.start(this);
        } catch (Throwable t) {
            DebugLog.add("svc", "重新拉起后台服务失败：" + t);
        }
    }

    /**
     * 跳到系统的"自启动管理"。
     *
     * 为什么必须给个入口：光有 BOOT_COMPLETED 广播是不够的，国内厂商
     * （OPPO / vivo / 小米 / 华为 / 三星）在系统层又拦了一道自启动白名单，
     * 不手动加进去，开机广播根本收不到，NtfyService 也就永远起不来。
     * 而这一层没有统一 API，只能挨个试各家设置的类名，试不通就退到
     * 应用详情页让用户自己找。
     */
    public void openAutostartSettings() {
        String[][] targets = {
                // OPPO / realme（ColorOS）
                {"com.coloros.safecenter", "com.coloros.safecenter.permission.startup.StartupAppListActivity"},
                {"com.coloros.safecenter", "com.coloros.safecenter.startupapp.StartupAppListActivity"},
                {"com.oppo.safe", "com.oppo.safe.permission.startup.StartupAppListActivity"},
                // vivo / iQOO
                {"com.vivo.permissionmanager", "com.vivo.permissionmanager.activity.BgStartUpManagerActivity"},
                {"com.iqoo.secure", "com.iqoo.secure.ui.phoneoptimize.AddWhiteListActivity"},
                // 小米 / 红米
                {"com.miui.securitycenter", "com.miui.permcenter.autostart.AutoStartManagementActivity"},
                // 华为 / 荣耀
                {"com.huawei.systemmanager", "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity"},
                {"com.huawei.systemmanager", "com.huawei.systemmanager.optimize.process.ProtectActivity"},
                // 三星
                {"com.samsung.android.lool", "com.samsung.android.sm.ui.battery.BatteryActivity"},
        };
        for (String[] t : targets) {
            try {
                Intent i = new Intent();
                i.setClassName(t[0], t[1]);
                i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                startActivity(i);
                DebugLog.add("svc", "打开了自启动管理：" + t[0]);
                return;
            } catch (Throwable ignored) {
                // 这台机器上没有这个组件，试下一个
            }
        }
        try {
            Intent i = new Intent(android.provider.Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                    Uri.parse("package:" + getPackageName()));
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            startActivity(i);
            DebugLog.add("svc", "没找到自启动管理，退到应用详情页");
            toast("在系统的“自启动”里把随行打开");
        } catch (Throwable t) {
            DebugLog.add("svc", "打开系统设置失败：" + t);
            toast("没找到系统设置入口");
        }
    }

    /**
     * 用系统下载器存一张图。
     *
     * 为什么不自己写循环写文件：DownloadManager 自带断点续传、通知栏进度、
     * 失败重试，而且落到公共「下载」目录在 Android 10+ 的分区存储下
     * 不需要任何权限。文件名推不出来（路径藏在 query 里），自己拼一个。
     */
    public void downloadFile(String url) {
        try {
            String stamp = new java.text.SimpleDateFormat("yyyyMMdd-HHmmss", Locale.US)
                    .format(new java.util.Date());
            String name = "codex-" + stamp + guessImageExt(url);
            android.app.DownloadManager.Request req =
                    new android.app.DownloadManager.Request(Uri.parse(url));
            req.setNotificationVisibility(
                    android.app.DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
            req.setDestinationInExternalPublicDir(
                    android.os.Environment.DIRECTORY_DOWNLOADS, name);
            android.app.DownloadManager dm =
                    (android.app.DownloadManager) getSystemService(DOWNLOAD_SERVICE);
            if (dm == null) {
                toast("这台手机没有可用的下载器");
                return;
            }
            dm.enqueue(req);
            DebugLog.add("app", "开始下载：" + name);
            toast("已开始下载，去「下载」目录看");
        } catch (Throwable t) {
            DebugLog.add("app", "下载失败：" + t);
            toast("下载失败：" + t.getMessage());
        }
    }

    /** 从接口地址里猜个后缀。缩略图服务端统一输出 jpeg，所以兜底给 .jpg。 */
    private String guessImageExt(String url) {
        String u = url == null ? "" : url.toLowerCase();
        if (u.contains(".png")) return ".png";
        if (u.contains(".webp")) return ".webp";
        if (u.contains(".gif")) return ".gif";
        return ".jpg";
    }

    /**
     * 把旧的"外壳缓存"清干净再重载一次。
     *
     * WebView.clearCache() 清的是 HTTP 缓存，Service Worker 存放在 Cache Storage
     * 里的外壳它管不着 —— 而"缓存优先"的 SW 会让手机一直看着老界面
     * （2026-10-10 实测：电脑上改好十一个小时，手机上还是旧的，连修好的逻辑一起没生效）。
     * 这里在页面里直接执行脚本：注销 SW 注册、删掉所有缓存，然后重新加载。
     *
     * 清不掉也能用：脚本是尽力而为，重载那一步照常发生（最差就是仍走旧缓存）。
     */
    private void wipeShellAndReload() {
        final String js = "(function(){try{"
                + "if(navigator.serviceWorker&&navigator.serviceWorker.getRegistrations){"
                + "navigator.serviceWorker.getRegistrations().then(function(rs){"
                + "rs.forEach(function(r){try{r.unregister();}catch(e){}});});}"
                + "if(window.caches){caches.keys().then(function(ks){"
                + "ks.forEach(function(k){try{caches.delete(k);}catch(e){}});});}"
                + "}catch(e){}})()";
        try {
            web.evaluateJavascript(js, value -> web.postDelayed(() -> {
                if (!loadedUrl.isEmpty()) {
                    DebugLog.add("app", "已清掉 SW 外壳缓存，重新加载界面");
                    web.loadUrl(loadedUrl);
                }
            }, 700));
        } catch (Throwable t) {
            DebugLog.add("app", "清外壳缓存失败：" + t);
            if (!loadedUrl.isEmpty()) web.loadUrl(loadedUrl);
        }
    }

    @Override
    protected void onPause() {
        super.onPause();
        DebugLog.add("app", "退到后台");
    }

    @Override
    protected void onDestroy() {
        if (netCallback != null) {
            try {
                ConnectivityManager cm = (ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
                if (cm != null) cm.unregisterNetworkCallback(netCallback);
            } catch (Throwable ignored) {
            }
            netCallback = null;
        }
        super.onDestroy();
    }

    /**
     * 盯住网络切换。
     *
     * "WiFi 连不上、切蜂窝就好了"这类毛病，关键现场就在切网的那一瞬间：切之前挂在
     * 哪张网、切之后又是哪张。这里两边都留痕，事后能直接对比。
     */
    private void watchNetwork() {
        try {
            ConnectivityManager cm = (ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
            if (cm == null) return;
            netCallback = new ConnectivityManager.NetworkCallback() {
                @Override
                public void onAvailable(Network network) {
                    netDiagThrottled("网络上线", 20000);
                    // 换网之后当前地址可能已经不通了（比如 WiFi 走掉了），
                    // 或者能换到更快的局域网直连，这里重新选一次
                    new Thread(MainActivity.this::maybeSwitchTarget, "switchurl").start();
                }

                @Override
                public void onLost(Network network) {
                    // 断网时不再跑 TCP 探测（没意义也拖时间），记一份摘要就够
                    DebugLog.add("net", "网络下线 " + NetDiag.snapshot(MainActivity.this));
                }
            };
            cm.registerDefaultNetworkCallback(netCallback);
        } catch (Throwable t) {
            DebugLog.add("net", "注册网络监听失败：" + t);
        }
    }

    /** 常规事件用这个：同样的话最多每 minGapMs 记一次，免得切个后台就刷一屏。 */
    private void netDiagThrottled(String reason, long minGapMs) {
        long now = System.currentTimeMillis();
        if (now - lastNetDiagAt < minGapMs) return;
        lastNetDiagAt = now;
        NetDiag.log(this, reason);
    }

    /** 出坏事时用这个：只防 3 秒内的连击，不和常规节流抢时间窗。 */
    private void netDiagNow(String reason) {
        long now = System.currentTimeMillis();
        if (now - lastNetDiagAt < 3000) return;
        lastNetDiagAt = now;
        NetDiag.log(this, reason);
    }

    /**
     * 抓一张当前界面的截图，发回电脑。必须在 UI 线程调用。
     * 返回一句人话，网页直接显示。
     */
    public String captureAndSendScreen(String name) {
        try {
            int w = web.getWidth(), h = web.getHeight();
            if (w <= 0 || h <= 0) return "界面还没画出来";
            // 控制体积：最长边不超过 1200
            float scale = Math.min(1f, 1200f / Math.max(w, h));
            Bitmap bmp = Bitmap.createBitmap(Math.round(w * scale), Math.round(h * scale), Bitmap.Config.ARGB_8888);
            Canvas canvas = new Canvas(bmp);
            canvas.scale(scale, scale);
            web.draw(canvas);
            ByteArrayOutputStream buf = new ByteArrayOutputStream();
            bmp.compress(Bitmap.CompressFormat.PNG, 100, buf);
            bmp.recycle();
            String b64 = Base64.encodeToString(buf.toByteArray(), Base64.NO_WRAP);
            DebugLog.add("shot", "抓到截图 " + (b64.length() / 1024) + "KB，开始上传");
            final String label = (name == null || name.isEmpty()) ? Device.shortInfo(this) : name;
            new Thread(() -> {
                String r = DebugLog.upload(this, "shot", label, b64, Device.info(this));
                DebugLog.add("shot", "上传结果：" + r);
            }).start();
            return "已抓图，正在传到电脑";
        } catch (Throwable t) {
            DebugLog.add("shot", "抓图失败：" + t);
            return "抓图失败：" + t;
        }
    }
}
