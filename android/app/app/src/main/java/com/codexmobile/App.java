package com.codexmobile;

import android.app.Application;
import android.content.SharedPreferences;

import java.io.PrintWriter;
import java.io.StringWriter;

/**
 * 兜底的崩溃记录器。
 *
 * 手机上出问题最难办的是拿不到日志。这里把上一次崩溃的堆栈存下来，
 * 下次打开 App 时直接显示在屏幕上，截图就能定位。
 */
public class App extends Application {

    public static final String PREFS = "codex";
    public static final String KEY_CRASH = "lastCrash";
    public static final String KEY_CRASH_AT = "lastCrashAt";

    @Override
    public void onCreate() {
        super.onCreate();
        final Thread.UncaughtExceptionHandler previous = Thread.getDefaultUncaughtExceptionHandler();
        Thread.setDefaultUncaughtExceptionHandler((thread, error) -> {
            try {
                StringWriter sw = new StringWriter();
                error.printStackTrace(new PrintWriter(sw));
                String trace = sw.toString();
                if (trace.length() > 6000) {
                    trace = trace.substring(0, 6000);
                }
                SharedPreferences p = getSharedPreferences(PREFS, MODE_PRIVATE);
                p.edit()
                        .putString(KEY_CRASH, trace)
                        .putLong(KEY_CRASH_AT, System.currentTimeMillis())
                        .commit();
            } catch (Throwable ignored) {
                // 记录崩溃时自己再崩就不好了
            }
            if (previous != null) {
                previous.uncaughtException(thread, error);
            }
        });
    }
}
