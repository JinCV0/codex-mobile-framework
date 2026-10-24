package com.codexmobile;

import android.app.DownloadManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.database.Cursor;
import android.net.Uri;
import android.os.Build;
import android.os.Environment;
import android.widget.Toast;

/**
 * 应用内更新：从电脑上把新版本 APK 下下来，然后交回系统安装。
 *
 * 刻意不用任何第三方更新库——总共就下载 + 发一个安装 Intent 两件事。
 */
public final class Updater {

    private static final String FILE_NAME = "CodexMobile-update.apk";
    private static long downloadId = -1;
    private static BroadcastReceiver receiver;

    private Updater() { }

    public static void downloadAndInstall(Context ctx, String url) {
        try {
            DownloadManager dm = (DownloadManager) ctx.getSystemService(Context.DOWNLOAD_SERVICE);
            if (dm == null) { Toast.makeText(ctx, "系统下载器不可用", Toast.LENGTH_SHORT).show(); return; }

            DownloadManager.Request req = new DownloadManager.Request(Uri.parse(url));
                req.setTitle("随行 新版本");
            req.setDescription("下载完成后会自动弹出安装");
            req.setMimeType("application/vnd.android.package-archive");
            req.setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
            req.setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, FILE_NAME);
            downloadId = dm.enqueue(req);
            Toast.makeText(ctx, "开始下载…", Toast.LENGTH_SHORT).show();
            register(ctx);
        } catch (Throwable t) {
            Toast.makeText(ctx, "下载失败：" + t.getMessage(), Toast.LENGTH_LONG).show();
        }
    }

    private static void register(final Context ctx) {
        unregister(ctx);
        receiver = new BroadcastReceiver() {
            @Override
            public void onReceive(Context context, Intent intent) {
                long id = intent.getLongExtra(DownloadManager.EXTRA_DOWNLOAD_ID, -1);
                if (id != downloadId) return;
                unregister(context);
                install(context);
            }
        };
        IntentFilter filter = new IntentFilter(DownloadManager.ACTION_DOWNLOAD_COMPLETE);
        if (Build.VERSION.SDK_INT >= 33) {
            ctx.registerReceiver(receiver, filter, Context.RECEIVER_NOT_EXPORTED);
        } else {
            ctx.registerReceiver(receiver, filter);
        }
    }

    private static void unregister(Context ctx) {
        if (receiver == null) return;
        try { ctx.unregisterReceiver(receiver); } catch (Throwable ignored) { }
        receiver = null;
    }

    private static void install(Context ctx) {
        try {
            DownloadManager dm = (DownloadManager) ctx.getSystemService(Context.DOWNLOAD_SERVICE);
            if (dm == null) return;

            // 下载失败就别让用户空欢喜
            Cursor c = dm.query(new DownloadManager.Query().setFilterById(downloadId));
            if (c != null) {
                try {
                    if (c.moveToFirst()) {
                        int status = c.getInt(c.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS));
                        if (status != DownloadManager.STATUS_SUCCESSFUL) {
                            Toast.makeText(ctx, "下载没有完成，稍后再试", Toast.LENGTH_LONG).show();
                            return;
                        }
                    }
                } finally {
                    c.close();
                }
            }

            Uri uri = dm.getUriForDownloadedFile(downloadId);
            if (uri == null) {
                Toast.makeText(ctx, "找不到下载好的文件", Toast.LENGTH_LONG).show();
                return;
            }
            Intent i = new Intent(Intent.ACTION_VIEW);
            i.setDataAndType(uri, "application/vnd.android.package-archive");
            i.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION | Intent.FLAG_ACTIVITY_NEW_TASK);
            ctx.startActivity(i);
        } catch (Throwable t) {
            Toast.makeText(ctx, "调起安装失败：" + t.getMessage(), Toast.LENGTH_LONG).show();
        }
    }
}
