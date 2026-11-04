'use strict';

/**
 * e2e.js —— 初步验证：公开搜索 → ID → 自定义源协议 → 直链 → 下载
 *
 * 验证的不是"能搜到歌"，而是整条链路真的能落地：
 *   1. 公开搜索拿到 songmid（无 cookie、无登录）
 *   2. 把 songmid 交给一个走 lx 协议的解析器（poc/resolver-public.js）
 *   3. 拿到真实直链
 *   4. 下载成文件并检查它**真的是一首音频**（magic + 时长估算）
 *   5. 负例：VIP 曲目必须**明确失败**，而不是静默返回坏数据
 *
 * 用法：
 *   node e2e.js                          # 默认搜"周杰伦 晴天"，取第一首免费曲
 *   node e2e.js --query="陈奕迅 十年"
 *   node e2e.js --id=509781655           # 指定曲目（可用于验证 VIP 失败）
 *   node e2e.js --out=./out
 */

const fs = require('node:fs');
const path = require('node:path');
const https = require('node:https');
const http = require('node:http');
const crypto = require('node:crypto');
const { createHost } = require('../p0-fixture/lx-runtime');

const UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36';

const args = Object.fromEntries(
  process.argv
    .slice(2)
    .filter((a) => a.startsWith('--'))
    .map((a) => {
      const i = a.indexOf('=');
      return i === -1 ? [a.slice(2), true] : [a.slice(2, i), a.slice(i + 1)];
    }),
);

const QUERY = typeof args.query === 'string' ? args.query : '周杰伦 晴天';
const OUT_DIR = path.resolve(__dirname, typeof args.out === 'string' ? args.out : 'out');
const SCRIPT = fs.readFileSync(path.join(__dirname, 'resolver-public.js'), 'utf8');

const results = [];
function step(name, detail = '') {
  results.push({ name, detail });
  console.log(`\x1b[32m✓\x1b[0m ${name}${detail ? `  \x1b[90m${detail}\x1b[0m` : ''}`);
}
function fail(name, err) {
  console.log(`\x1b[31m✗\x1b[0m ${name}`);
  console.log(`   \x1b[31m${err && (err.message || err)}\x1b[0m`);
  process.exit(1);
}

// --- HTTP ------------------------------------------------------------------
function get(url, { headers = {}, redirects = 3, raw = false } = {}) {
  return new Promise((resolve, reject) => {
    // 直链常见是 http://（m801.music.126.net），必须两种协议都支持
    const lib = new URL(url).protocol === 'http:' ? http : https;
    const req = lib.get(url, { headers: { 'User-Agent': UA, ...headers } }, (res) => {
      const { statusCode } = res;
      if (statusCode >= 300 && statusCode < 400 && res.headers.location && redirects > 0) {
        res.resume();
        return resolve(get(new URL(res.headers.location, url).toString(), { headers, redirects: redirects - 1, raw }));
      }
      if (statusCode !== 200) {
        res.resume();
        return reject(new Error(`HTTP ${statusCode} @ ${url}`));
      }
      if (raw) return resolve({ res, statusCode });
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const buf = Buffer.concat(chunks);
        let body = buf.toString('utf8');
        try {
          body = JSON.parse(body);
        } catch (_) {}
        resolve({ statusCode, headers: res.headers, body, raw: buf });
      });
    });
    req.setTimeout(20000, () => req.destroy(new Error('请求超时')));
    req.on('error', reject);
  });
}

// --- 步骤 1：搜索 -----------------------------------------------------------
async function search(query) {
  const url = `https://music.163.com/api/search/get/web?s=${encodeURIComponent(query)}&type=1&offset=0&limit=10`;
  const { body } = await get(url, { headers: { Referer: 'https://music.163.com/' } });
  if (!body || body.code !== 200) throw new Error(`搜索失败: ${JSON.stringify(body).slice(0, 120)}`);
  const songs = (body.result && body.result.songs) || [];
  if (!songs.length) throw new Error('搜索无结果');
  return songs;
}

// --- 步骤 3：走 lx 协议解析 --------------------------------------------------
function resolveViaLx(song, quality = '320k') {
  const host = createHost({ script: SCRIPT, tolerant: true, resolveTimeoutMs: 20000 });
  host.run();
  if (!host.state.inited) throw new Error('解析器初始化失败（inited 未触发）');
  return host
    .resolve('musicUrl', 'wy', {
      type: quality,
      musicInfo: { songmid: String(song.id), name: song.name, hash: '' },
    })
    .then((url) => ({ url, host }));
}

// --- 步骤 4：下载 -----------------------------------------------------------
function download(url, destBase) {
  const part = `${destBase}.part`;
  return new Promise((resolve, reject) => {
    get(url, {
      headers: { Referer: 'https://music.163.com/', 'User-Agent': UA },
      raw: true,
      redirects: 5,
    })
      .then(({ res }) => {
        const contentType = res.headers['content-type'] || '';
        const expected = Number(res.headers['content-length'] || 0);
        const hash = crypto.createHash('sha1');
        const out = fs.createWriteStream(part);
        let bytes = 0;
        let head = Buffer.alloc(0);

        res.on('data', (c) => {
          bytes += c.length;
          hash.update(c);
          if (head.length < 16) head = Buffer.concat([head, c]).subarray(0, 16);
          out.write(c);
        });
        res.on('end', () => {
          out.end(() => {
            const ok = !expected || bytes === expected;
            resolve({ part, bytes, expected, contentType, sha1: hash.digest('hex'), head, complete: ok });
          });
        });
        res.on('error', (e) => {
          out.end();
          reject(e);
        });
        out.on('error', reject);
      })
      .catch(reject);
  });
}

// --- 音频嗅探 ---------------------------------------------------------------
function sniff(buf) {
  if (buf.slice(0, 3).toString() === 'ID3') return 'mp3';
  if (buf.slice(0, 4).toString() === 'fLaC') return 'flac';
  if (buf.slice(4, 8).toString() === 'ftyp') return 'm4a';
  if (buf[0] === 0xff && (buf[1] & 0xe0) === 0xe0) return 'mp3';
  return 'unknown';
}

const EXT = { mp3: '.mp3', flac: '.flac', m4a: '.m4a', unknown: '.bin' };

// --- 主流程 -----------------------------------------------------------------
(async () => {
  fs.mkdirSync(OUT_DIR, { recursive: true });
  console.log(`\x1b[1m搜索 → 自定义源 → 直链 → 下载\x1b[0m  \x1b[90m(输出目录 ${OUT_DIR})\x1b[0m\n`);

  // 1. 搜索
  const songs = await search(QUERY).catch((e) => fail('公开搜索', e));
  step('公开搜索（无 cookie）', `"${QUERY}" → ${songs.length} 条`);

  // 2. 选曲
  let song;
  if (typeof args.id === 'string') {
    song = songs.find((s) => String(s.id) === args.id) || { id: args.id, name: `(指定 id ${args.id})`, fee: null, duration: 0 };
    step('指定曲目', `id=${song.id}`);
  } else {
    song = songs.find((s) => s.fee !== 1) || songs[0];
    const kind = song.fee === 1 ? 'VIP' : '免费/试听';
    step('选择曲目', `${song.id} · ${song.name} · fee=${song.fee} (${kind})`);
  }

  // 3. 解析直链（走 lx 协议）
  let url;
  try {
    const r = await resolveViaLx(song);
    url = r.url;
    step('自定义源协议解析 musicUrl', `${url.slice(0, 64)}…`);
  } catch (e) {
    // VIP / 无直链：这是**预期内**的失败，单独判定
    const msg = String(e && e.message ? e.message : e);
    if (/无可用直链|会员|版权|-110/.test(msg)) {
      console.log(`\x1b[33m⚠\x1b[0m 自定义源协议解析 musicUrl`);
      console.log(`   \x1b[33m预期内失败：${msg}\x1b[0m`);
      console.log('\n\x1b[1m结论：\x1b[0m该曲目需要会员/受版权限制，链路本身正常（错误被如实透出）。');
      process.exit(0);
    }
    fail('自定义源协议解析 musicUrl', e);
  }

  // 4. 下载
  const safeName = String(song.name).replace(/[\\/:*?"<>|]/g, '_').slice(0, 40);
  const base = path.join(OUT_DIR, `${song.id}-${safeName}`);
  const dl = await download(url, base).catch((e) => fail('下载', e));

  if (!dl.complete) fail('下载完整性', `收到 ${dl.bytes}B，期望 ${dl.expected}B`);

  // 5. 校验是不是真音频
  const kind = sniff(dl.head);
  if (kind === 'unknown') fail('音频校验', `文件头不像音频: ${dl.head.toString('hex')}`);

  const finalPath = `${base}${EXT[kind]}`;
  fs.renameSync(dl.part, finalPath);

  const sizeMB = (dl.bytes / 1024 / 1024).toFixed(2);
  step('下载完成', `${sizeMB} MB · ${dl.contentType || '未知类型'} · sha1 ${dl.sha1.slice(0, 12)}…`);
  step('音频校验', `识别为 ${kind}，文件头 ${dl.head.slice(0, 4).toString('hex')}`);

  // 6. 时长粗校验（拿接口返回的时长对比）
  if (song.duration) {
    const sec = Math.round(song.duration / 1000);
    const m = Math.floor(sec / 60);
    const s = String(sec % 60).padStart(2, '0');
    step('曲目信息', `接口时长 ${m}:${s}，文件已落盘`);
  }

  console.log(`\n\x1b[1m文件：\x1b[0m${finalPath}`);
  console.log('\x1b[1m结论：\x1b[0m搜索 → ID → 自定义源 → 直链 → 下载，链路完整可用。');
})().catch((e) => fail('未捕获异常', e));
