'use strict';

/**
 * mock-server.js —— 零依赖的本地 mock API，模拟 ikun 音源的服务端行为。
 *
 * 单独运行：
 *   node mock-server.js           # 监听 18787
 *   node mock-server.js 18000     # 指定端口
 *
 * 作为库使用：
 *   const mock = createMockServer();      // 随机端口
 *   mock.port, mock.setScenario('busy'), mock.stats(), mock.close()
 *
 * 场景（可用 HTTP 动态切换：GET /__scenario?set=busy）：
 *   ok | deny(403) | busy(429) | fail(500) | slow | redirect
 *   badurl | longurl | html | http500 | noapikey
 */

const http = require('node:http');

const API_KEY = 'local-test-key';

const SCENARIOS = ['ok', 'deny', 'busy', 'fail', 'slow', 'redirect', 'badurl', 'longurl', 'html', 'http500', 'noapikey'];

function createMockServer({ port = 0, host = '127.0.0.1' } = {}) {
  /** 每个场景下 /music/url 的行为 */
  const state = {
    scenario: 'ok',
    hits: [],
    lastMusicRequest: null, // {method, headers, body, at}
    updateAlert: { updateMsg: 'v2 修复了若干问题', updateUrl: 'https://example.com/update' },
  };

  const server = http.createServer((req, res) => {
    const url = new URL(req.url, `http://${host}:${port}`);
    state.hits.push({ method: req.method, path: url.pathname, at: Date.now() });

    if (url.pathname === '/__scenario') {
      const set = url.searchParams.get('set');
      if (set && SCENARIOS.includes(set)) {
        state.scenario = set;
        return json(res, 200, { code: 200, scenario: set });
      }
      return json(res, 200, { code: 200, scenario: state.scenario, available: SCENARIOS });
    }

    if (url.pathname === '/__stats') {
      return json(res, 200, {
        scenario: state.scenario,
        hits: state.hits.length,
        lastMusicRequest: state.lastMusicRequest,
        recent: state.hits.slice(-20),
      });
    }

    if (url.pathname === '/__reset') {
      state.hits = [];
      state.lastMusicRequest = null;
      state.scenario = 'ok';
      return json(res, 200, { code: 200 });
    }

    // 脚本更新检查（对应 ikun 的 checkUpdate）
    if (url.pathname === '/script/lxmusic') {
      return json(res, 200, { code: 200, data: state.updateAlert });
    }

    if (url.pathname.startsWith('/audio/')) {
      return serveAudio(req, res, url.pathname);
    }

    if (url.pathname === '/music/url' || url.pathname === '/music/url-real') {
      return handleMusicUrl(req, res, url, state, { port: server.address().port, host });
    }

    return json(res, 404, { code: 404, message: 'not found' });
  });

  const listen = () =>
    new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(port, host, () => {
        server.off('error', reject);
        resolve(server.address().port);
      });
    });

  return {
    server,
    get port() {
      return server.address() ? server.address().port : port;
    },
    get base() {
      return `http://${host}:${this.port}`;
    },
    listen,
    close: () => new Promise((r) => server.close(r)),
    setScenario: (s) => {
      if (!SCENARIOS.includes(s)) throw new Error(`unknown scenario: ${s}`);
      state.scenario = s;
    },
    get scenario() {
      return state.scenario;
    },
    stats: () => ({
      scenario: state.scenario,
      hits: state.hits.slice(),
      lastMusicRequest: state.lastMusicRequest,
      updateAlert: state.updateAlert,
    }),
    reset: () => {
      state.hits = [];
      state.lastMusicRequest = null;
      state.scenario = 'ok';
    },
  };
}

function handleMusicUrl(req, res, url, state, { port, host }) {
  if (url.pathname === '/music/url-real') {
    return json(res, 200, { code: 200, url: `http://${host}:${port}/audio/redirected.mp3` });
  }

  readBody(req, (err, raw) => {
    if (err) return json(res, 400, { code: 400, message: 'bad body' });

    let parsed = null;
    try {
      parsed = JSON.parse(raw.toString() || '{}');
    } catch (_) {
      parsed = null;
    }

    state.lastMusicRequest = {
      method: req.method,
      headers: req.headers,
      body: parsed,
      rawBody: raw.toString().slice(0, 500),
      at: Date.now(),
    };

    if (state.scenario === 'http500') {
      return json(res, 500, { code: 500, message: '服务器内部错误（HTTP 500）' });
    }

    if (state.scenario === 'noapikey' || req.headers['x-api-key'] !== API_KEY) {
      return json(res, 200, { code: 403, message: '鉴权失败' });
    }

    if (state.scenario === 'html') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      return res.end('<html><body>502 Bad Gateway</body></html>');
    }

    if (state.scenario === 'redirect') {
      res.writeHead(302, { Location: '/music/url-real' });
      return res.end();
    }

    const after = (fn) => fn();

    switch (state.scenario) {
      case 'deny':
        return after(() => json(res, 200, { code: 403, message: '鉴权失败' }));
      case 'busy':
        return after(() => json(res, 200, { code: 429, message: '请求过速' }));
      case 'fail':
        return after(() => json(res, 200, { code: 500, message: '后端炸了' }));
      case 'badurl':
        return after(() => json(res, 200, { code: 200, url: 'not-a-valid-url' }));
      case 'longurl':
        return after(() => json(res, 200, { code: 200, url: `https://example.com/${'x'.repeat(3000)}` }));
      case 'slow':
        return setTimeout(() => json(res, 200, { code: 200, url: `http://${host}:${port}/audio/slow.mp3` }), 1500);
      default:
        return json(res, 200, {
          code: 200,
          source: parsed && parsed.source,
          quality: parsed && parsed.quality,
          url: `http://${host}:${port}/audio/${(parsed && parsed.source) || 'unknown'}.mp3`,
        });
    }
  });
}

function serveAudio(req, res, pathname) {
  const total = 64 * 1024;
  const range = req.headers.range;

  if (range) {
    const m = /bytes=(\d*)-(\d*)/.exec(range);
    if (m) {
      const start = m[1] ? parseInt(m[1], 10) : 0;
      const end = m[2] ? parseInt(m[2], 10) : total - 1;
      const len = Math.max(0, end - start + 1);
      res.writeHead(206, {
        'Content-Type': 'audio/mpeg',
        'Accept-Ranges': 'bytes',
        'Content-Range': `bytes ${start}-${end}/${total}`,
        'Content-Length': len,
      });
      return res.end(Buffer.alloc(len, 0x55));
    }
  }

  res.writeHead(200, {
    'Content-Type': 'audio/mpeg',
    'Accept-Ranges': 'bytes',
    'Content-Length': total,
    'Content-Disposition': `inline; filename="${pathname.split('/').pop()}"`,
  });
  res.end(Buffer.alloc(total, 0x55));
}

function readBody(req, cb) {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => cb(null, Buffer.concat(chunks)));
  req.on('error', (e) => cb(e));
}

function json(res, status, obj) {
  const body = Buffer.from(JSON.stringify(obj));
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': body.length,
  });
  res.end(body);
}

module.exports = { createMockServer, SCENARIOS, API_KEY };

if (require.main === module) {
  const port = Number(process.argv[2] || process.env.MOCK_PORT || 18787);
  const mock = createMockServer({ port });
  mock
    .listen()
    .then((p) => {
      console.log(`mock server on http://127.0.0.1:${p}`);
      console.log(`  GET  /__scenario?set=busy     切换场景（${SCENARIOS.join(' | ')}）`);
      console.log(`  GET  /__stats                 查看命中记录`);
      console.log(`  POST /music/url               模拟音源接口`);
      console.log(`  GET  /script/lxmusic          模拟更新检查`);
      console.log(`  GET  /audio/test.mp3          64KB 假音频（支持 Range）`);
    })
    .catch((e) => {
      console.error('failed to listen:', e.message);
      process.exit(1);
    });
}
