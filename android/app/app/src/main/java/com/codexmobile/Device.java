package com.codexmobile;

import android.content.Context;
import android.os.Build;
import android.util.DisplayMetrics;
import android.view.WindowManager;
import android.webkit.WebSettings;
import android.webkit.WebView;

import org.json.JSONObject;

/**
 * 这台手机的基本信息：出问题时最常要问的那几项。
 * 机型/系统/WebView 版本/屏幕尺寸/密度/App 版本。
 */
public final class Device {

    private Device() {}

    public static String info(Context ctx) {
        JSONObject o = new JSONObject();
        try {
            o.put("app", BuildConfig.VERSION_NAME + "（" + BuildConfig.VERSION_CODE + "）");
            o.put("model", Build.MANUFACTURER + " " + Build.MODEL);
            o.put("android", Build.VERSION.RELEASE + "（API " + Build.VERSION.SDK_INT + "）");
            o.put("webview", webViewVersion());
            o.put("screen", screen(ctx));
        } catch (Throwable ignored) {}
        return o.toString();
    }

    /** 一句话版本，塞进文件名和界面上用。 */
    public static String shortInfo(Context ctx) {
        return Build.MODEL + "-android" + Build.VERSION.RELEASE;
    }

    private static String webViewVersion() {
        try {
            if (Build.VERSION.SDK_INT >= 26) {
                return WebView.getCurrentWebViewPackage() == null
                        ? "未知" : WebView.getCurrentWebViewPackage().versionName;
            }
        } catch (Throwable ignored) {}
        return "未知";
    }

    private static String screen(Context ctx) {
        try {
            WindowManager wm = (WindowManager) ctx.getSystemService(Context.WINDOW_SERVICE);
            if (wm == null) return "";
            DisplayMetrics m = new DisplayMetrics();
            wm.getDefaultDisplay().getRealMetrics(m);
            return m.widthPixels + "x" + m.heightPixels + " @" + m.density;
        } catch (Throwable t) {
            return "";
        }
    }

    /** 顺手指一下当前 WebView 的设置，调样式时有用。 */
    public static String webSettings(WebView web) {
        try {
            WebSettings s = web.getSettings();
            return "js=" + s.getJavaScriptEnabled() + " dom=" + s.getDomStorageEnabled()
                    + " wide=" + s.getUseWideViewPort() + " ua=" + (s.getUserAgentString() == null ? "" : "有");
        } catch (Throwable t) {
            return "";
        }
    }
}
