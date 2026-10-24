package com.codexmobile;

import android.app.Activity;
import android.graphics.Color;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;

/**
 * 内置浏览器页：点「服务」里的卡片时用它打开，
 * 这样不用跳出 App，返回键就能回来。
 */
public class WebActivity extends Activity {

    public static final String EXTRA_URL = "url";
    public static final String EXTRA_TITLE = "title";

    private WebView web;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        String url = getIntent().getStringExtra(EXTRA_URL);
        String title = getIntent().getStringExtra(EXTRA_TITLE);
        if (url == null) { finish(); return; }

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(Color.parseColor("#101114"));

        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(Color.parseColor("#17181C"));
        bar.setPadding(dp(6), dp(6), dp(12), dp(6));

        TextView back = new TextView(this);
        back.setText("← 返回");
        back.setTextColor(Color.parseColor("#69C497"));
        back.setTextSize(15f);
        back.setPadding(dp(8), dp(8), dp(8), dp(8));
        back.setOnClickListener(v -> goBackOrFinish());
        bar.addView(back);

        TextView t = new TextView(this);
        t.setText(title == null ? url : title);
        t.setTextColor(Color.parseColor("#E6E7E9"));
        t.setTextSize(14.5f);
        t.setSingleLine(true);
        t.setEllipsize(android.text.TextUtils.TruncateAt.END);
        bar.addView(t, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        root.addView(bar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        s.setUseWideViewPort(true);
        s.setLoadWithOverviewMode(true);
        s.setBuiltInZoomControls(true);
        s.setDisplayZoomControls(false);
        web.setWebViewClient(new WebViewClient());
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public void onReceivedTitle(WebView view, String pageTitle) {
                if (pageTitle != null && !pageTitle.isEmpty()) t.setText(pageTitle);
            }
        });
        web.setBackgroundColor(Color.parseColor("#101114"));
        root.addView(web, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        setContentView(root);

        // targetSdk 35 起是 edge-to-edge，原生这条标题栏要自己避开状态栏
        final LinearLayout barRef = bar;
        final int barLeft = bar.getPaddingLeft();
        final int barRight = bar.getPaddingRight();
        final int barBottom = bar.getPaddingBottom();
        getWindow().getDecorView().setOnApplyWindowInsetsListener((v, insets) -> {
            int top;
            if (android.os.Build.VERSION.SDK_INT >= 30) {
                top = insets.getInsets(WindowInsets.Type.systemBars()).top;
            } else {
                top = insets.getSystemWindowInsetTop();
            }
            barRef.setPadding(barLeft, dp(6) + top, barRight, barBottom);
            return insets;
        });
        web.loadUrl(url);
    }

    private int dp(int v) { return Math.round(v * getResources().getDisplayMetrics().density); }

    @Override
    public void onBackPressed() {
        goBackOrFinish();
    }

    private void goBackOrFinish() {
        if (web != null && web.canGoBack()) web.goBack();
        else finish();
    }
}
