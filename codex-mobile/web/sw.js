/* 手机端"开机就有画面"的关键：把界面外壳缓在本地。

   以前每次启动都要现去电脑上拉 index.html（异地网络 + TLS 握手，
   动辄好几秒），所以每次都看到"正在连接"。现在：

     - 外壳（index.html、图表库、图标）在安装时就缓存好；
     - 打开页面时先拿缓存立刻渲染，后台再去拉新的（下次启动就是新的）；
     - /api/* 和 /proxy/* 永远走网络，绝不缓存数据。

   改外壳版本号会让旧的缓存自动清掉（activate 里删）。
*/
// 换版本号会强制清掉旧缓存。2026-10-10 从 v7 升到 v8：把导航改回
// "缓存优先、后台校验"。2026-10-05 当时之所以改成网络优先，是因为缓存优先
// 会让手机一直看着老界面；但那两个兜底是后来才有的 —— 外壳真变了页面会自己
// 换一次（shell-updated / controllerchange），/api/status 的 build 变了也会
// 触发刷新。而网络优先的代价已经实测到了：公网 TTFB 1.4~1.8 秒，
// 链路一抖就是十几二十秒的白等（手机日志里有一次 23.8 秒）。
const VERSION = 'codex-shell-v10';
const SHELL = [
  './index.html',
  './chart.umd.min.js',
  './app.webmanifest',
  './icon-192.png',
  './icon-512.png',
  './icon.png',
  './tray.png',
];

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(VERSION);
    // 逐个加，某个文件拿不到也别把安装搞挂
    await Promise.all(SHELL.map((u) => cache.add(new Request(u, { cache: 'reload' })).catch(() => {})));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter((k) => k !== VERSION).map((k) => caches.delete(k)));
    await self.clients.claim();
  })());
});

// 页面发现"外壳版本和电脑上的对不上"时会叫我们：立刻把新外壳取下来。
// 缓存优先的代价就是页面自己 reload 拿到的是旧缓存，所以这一步必须由
// Service Worker 去做，做完再通知页面刷新（见 index.html 的 forceShellRefresh）。
self.addEventListener('message', (event) => {
  const data = event.data || {};
  if (data.type !== 'force-shell-refresh') return;
  event.waitUntil((async () => {
    try {
      const res = await fetch('./index.html', { cache: 'reload' });
      if (res && res.ok) {
        const cache = await caches.open(VERSION);
        await cache.put('./index.html', res.clone());
      }
    } catch (_) {}
    try {
      const clients = await self.clients.matchAll({ type: 'window' });
      for (const c of clients) c.postMessage({ type: 'shell-refreshed' });
    } catch (_) {}
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  let url;
  try { url = new URL(req.url); } catch (_) { return; }
  if (url.origin !== self.location.origin) return;                       // 第三方不管
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/proxy/')) return; // 数据永远走网络

  // 打开页面：优先问网络，但只等 1.5 秒 —— 网络快就用新的，
  // 网络慢/断了才回落到缓存把界面撑起来（后台继续把新外壳存下来）。
  //
  // 以前是"先给缓存、后台更新"，看似秒开，代价是**换界面时手机可能一直看旧的**：
  // 后台那次更新可能失败，而且页面里"一个会话只自动换一次"的闸早就用掉了。
  if (req.mode === 'navigate') {
    event.respondWith((async () => {
      const cache = await caches.open(VERSION);
      const cached = await cache.match('./index.html');
      const netFetch = fetch(req).then(async (res) => {
        if (res && res.ok) {
          try { await cache.put('./index.html', res.clone()); } catch (_) {}
        }
        return res;
      }).catch(() => null);

      if (cached) {
        // 手上有界面就立刻给出去 —— "打开就能用"靠的就是这一条。
        // 新外壳在后台取回来；真变了才通知页面自己换一次。
        event.waitUntil((async () => {
          const res = await netFetch;
          if (!res || !res.ok) return;
          try {
            const body = await res.clone().text();
            const old = await cached.clone().text();
            if (old && old !== body) notifyShellUpdated();
          } catch (_) {}
        })());
        return cached;
      }

      const res = await netFetch;
      if (res) return res;
      return new Response('离线：连不上你的电脑。', {
        status: 503, headers: { 'Content-Type': 'text/plain; charset=utf-8' },
      });
    })());
    return;
  }

  // 静态资源：缓存优先 + 后台更新（?k= 和版本参数用 ignoreSearch 忽略掉）
  event.respondWith((async () => {
    const cache = await caches.open(VERSION);
    const cached = await cache.match(req, { ignoreSearch: true });
    const fresh = fetch(req).then((res) => {
      if (res && res.ok) cache.put(req, res.clone()).catch(() => {});
      return res;
    }).catch(() => null);
    if (cached) {
      event.waitUntil(fresh);
      return cached;
    }
    const res = await fresh;
    return res || new Response('', { status: 503 });
  })());
});

async function notifyShellUpdated() {
  try {
    const clients = await self.clients.matchAll({ type: 'window' });
    for (const c of clients) c.postMessage({ type: 'shell-updated' });
  } catch (_) {}
}
