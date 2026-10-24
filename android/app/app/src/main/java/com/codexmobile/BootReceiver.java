package com.codexmobile;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;

/**
 * 开机后把后台服务补起来。
 *
 * 为什么需要它：NtfyService 一断，手机就收不到任何通知、也收不到电脑发来的
 * 远程指令，而用户未必知道要去「我的」里把开关关掉再打开。开机广播是最可靠的
 * 补起时机 —— 手机重启后开机自启，服务就在了。
 *
 * 注意国内厂商还会额外拦一道：OPPO / vivo / 小米 / 华为这些系统有"自启动管理"，
 * 不把本 App 加进白名单，这个广播根本收不到。那一步只能引导用户去系统设置里
 * 手动开，入口见 MainActivity.openAutostartSettings。
 */
public class BootReceiver extends BroadcastReceiver {

    @Override
    public void onReceive(Context ctx, Intent intent) {
        try {
            String action = intent == null ? "" : String.valueOf(intent.getAction());
            if (!Intent.ACTION_BOOT_COMPLETED.equals(action)
                    && !"android.intent.action.QUICKBOOT_POWERON".equals(action)
                    && !"com.htc.intent.action.QUICKBOOT_POWERON".equals(action)) {
                return;
            }
            SharedPreferences p = ctx.getSharedPreferences(App.PREFS, Context.MODE_PRIVATE);
            // 用户没开通知订阅就别自作主张把服务拉起来
            if (!p.getBoolean("ntfyOn", false)) return;
            DebugLog.add("svc", "收到开机广播，把后台服务补起来");
            NtfyService.start(ctx);
        } catch (Throwable t) {
            DebugLog.add("svc", "开机广播处理失败：" + t);
        }
    }
}
