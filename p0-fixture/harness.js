'use strict';

/**
 * harness.js —— P0 验收脚本
 *
 * 它做的事：
 *   1. 起本地 mock server
 *   2. 用 lx-runtime（参考实现）把 lx-source-fixture.js 跑起来
 *   3. 跑一组固定场景，逐条判定 PASS / FAIL
 *
 * 运行：
 *   node harness.js            # 退出码 0 = 全绿
 *   node harness.js --verbose  # 打印每次请求的 mock 侧记录
 *
 * 之后把同一份 scenario 表搬进 Airmedy 的 Go/WebView 宿主，两边必须同时全绿。
 */

const fs = require('node:fs');
const path = require('node:path');
const { createMockServer } = require('./mock-server');
const { createHost, validateResult, filterSources } = require('./lx-runtime');

const VERBOSE = process.argv.includes('--verbose');
const SCRIPT_PATH = path.join(__dirname, 'lx-source-fixture.js');
const SCRIPT = fs.readFileSync(SCRIPT_PATH, 'utf8');

// --- 迷你测试框架 ----------------------------------------------------------
const results = [];
let currentGroup = '';

function group(name) {
  currentGroup = name;
  console.log(`\n\x1b[1m${name}\x1b[0m`);
}

async function test(name, fn) {
  const started = Date.now();
  try {
    await fn();
    const ms = Date.now() - started;
    results.push({ group: currentGroup, name, ok: true });
    console.log(`  \x1b[32m✓\x1b[0m ${name} \x1b[90m(${ms}ms)\x1b[0m`);
  } catch (e) {
    const ms = Date.now() - started;
    const msg = typeof e === 'string' ? e : (e && e.message) || String(e);
    results.push({ group: currentGroup, name, ok: false, error: msg });
    console.log(`  \x1b[31m✗\x1b[0m ${name} \x1b[90m(${ms}ms)\x1b[0m`);
    console.log(`      \x1b[31m${msg}\x1b[0m`);
  }
}

function ok(cond, msg) {
  if (!cond) throw new Error(msg || 'assertion failed');
}

function eq(actual, expected, label) {
  const a = JSON.stringify(actual);
  const b = JSON.stringify(expected);
  if (a !== b) throw new Error(`${label || 'value'}: expected ${b}, got ${a}`);
}

async function rejects(promise, pattern, label) {
  try {
    const v = await promise;
    throw new Error(`${label || 'promise'}: expected rejection, resolved with ${JSON.stringify(v)}`);
  } catch (e) {
    if (e && e.__isAssertion) throw e;
    const msg = typeof e === 'string' ? e : (e && e.message) || String(e);
    if (e && e.message && e.message.startsWith(`${label}: expected rejection`)) throw e;
    if (pattern && !pattern.test(msg)) {
      throw new Error(`${label || 'promise'}: expected /${pattern.source}/, got "${msg}"`);
    }
    return msg;
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- 主体 ------------------------------------------------------------------
async function main() {
  const mock = createMockServer();
  const port = await mock.listen();
  const base = `http://127.0.0.1:${port}`;
  console.log(`\x1b[90mmock server: ${base}\x1b[0m`);

  const newHost = (opts = {}) => {
    const host = createHost({
      script: SCRIPT,
      globals: { __MOCK_BASE__: base },
      resolveTimeoutMs: 8000,
      ...opts,
    });
    host.run();
    return host;
  };

  // 宽容模式宿主（默认）+ 严格模式宿主
  const host = newHost({ tolerant: true });
  const strictHost = newHost({ tolerant: false });

  const track = { songmid: '123456', hash: 'HASH123', name: '晴天', singer: '周杰伦', interval: '04:29' };

  // ---------------------------------------------------------------- 初始化
  group('A. 初始化握手');
  await test('宽容模式：inited 被接收，sources 解析成功', () => {
    ok(host.state.inited, 'inited 未触发');
    ok(Object.keys(host.state.sources).length >= 5, `sources 数量异常: ${Object.keys(host.state.sources)}`);
  });

  await test('宽容模式：保留非标准 source `git`（魔改客户端生态）', () => {
    ok(host.state.sources.git, 'git 源被丢弃了 —— 宽容模式没生效');
    eq(host.state.sources.git.actions, ['musicUrl'], 'git.actions');
  });

  await test('宽容模式：保留超纲音质 hires/atmos/master', () => {
    const kg = host.state.sources.kg;
    ok(kg.qualitys.includes('hires'), `kg.qualitys 丢了 hires: ${kg.qualitys}`);
    ok(kg.qualitys.includes('master'), `kg.qualitys 丢了 master: ${kg.qualitys}`);
  });

  await test('严格模式：`git` 被丢弃（对齐官方 lx 行为）', () => {
    ok(strictHost.state.inited, 'strict inited 未触发');
    ok(!strictHost.state.sources.git, 'git 不该出现在严格模式里');
  });

  await test('严格模式：音质被裁到官方 4 档', () => {
    eq(strictHost.state.sources.kg.qualitys, ['128k', '320k', 'flac', 'flac24bit'], 'kg.qualitys');
    eq(strictHost.state.sources.wy.qualitys, ['128k', '320k', 'flac', 'flac24bit'], 'wy.qualitys');
  });

  await test('脚本未声明 local 源（与真实音源一致）', () => {
    ok(!host.state.sources.local, 'fixture 不该声明 local');
  });

  // ------------------------------------------------------------ updateAlert
  group('B. updateAlert（更新提示）');
  await sleep(150);
  await test('脚本加载时异步 checkUpdate → 收到 updateAlert', () => {
    ok(host.state.updateAlerts.length >= 1, '没有收到 updateAlert');
    const a = host.state.updateAlerts[0];
    ok(typeof a.log === 'string' && a.log.length > 0, 'log 为空');
    ok(/^https?:\/\//.test(a.updateUrl || ''), `updateUrl 不合法: ${a.updateUrl}`);
  });

  await test('updateAlert 只能调用一次（第二次必须拒绝）', async () => {
    await rejects(host.lx.send('updateAlert', { log: 'again' }), /only be called once/, 'updateAlert#2');
  });

  // ------------------------------------------------------------- musicUrl
  group('C. musicUrl（正常路径）');

  await test('ok：返回 http URL', async () => {
    mock.setScenario('ok');
    const url = await host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track });
    ok(/^http:\/\/127\.0\.0\.1:\d+\/audio\/wy\.mp3$/.test(url), `URL 异常: ${url}`);
  });

  await test('请求体/请求头符合真实音源的约定', async () => {
    const req = mock.stats().lastMusicRequest;
    ok(req, 'mock 没收到请求');
    eq(req.method, 'POST', 'method');
    // 真实脚本用 `musicInfo.hash ?? musicInfo.songmid`，hash 优先
    eq(req.body, { source: 'wy', musicId: 'HASH123', quality: '320k' }, 'body');
    eq(req.headers['x-api-key'], 'local-test-key', 'x-api-key');
    ok(
      /^lx-music-desktop\/2\.0\.0$/.test(req.headers['user-agent'] || ''),
      `user-agent 异常: ${req.headers['user-agent']}`,
    );
    ok(
      /application\/json/.test(req.headers['content-type'] || ''),
      `content-type 异常: ${req.headers['content-type']}`,
    );
  });

  await test('ID 选择：hash 优先，缺失时回退 songmid', async () => {
    mock.setScenario('ok');
    await host.resolve('musicUrl', 'wy', { type: '128k', musicInfo: { songmid: '999', name: '无 hash 的歌' } });
    eq(mock.stats().lastMusicRequest.body.musicId, '999', 'songmid 回退');
  });

  await test('对象 body 被自动 JSON 化（不是 [object Object]）', async () => {
    const req = mock.stats().lastMusicRequest;
    ok(req.rawBody.includes('"musicId"'), `rawBody 异常: ${req.rawBody}`);
  });

  await test('follow_max：302 重定向被跟随', async () => {
    mock.setScenario('redirect');
    const url = await host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track });
    ok(url.includes('/audio/redirected.mp3'), `重定向后 URL 异常: ${url}`);
  });

  // ------------------------------------------------------------- 错误语义
  group('D. musicUrl（错误语义）');

  await test('403 → 「鉴权失败」', async () => {
    mock.setScenario('deny');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /鉴权失败/, '403');
  });

  await test('429 → 「请求过速」（应可重试）', async () => {
    mock.setScenario('busy');
    const msg = await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /请求过速/, '429');
    ok(msg.includes('请求过速'), '错误信息被破坏');
  });

  await test('500 → 透出后端 message', async () => {
    mock.setScenario('fail');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /获取URL失败.*后端炸了/, '500-code');
  });

  await test('HTTP 500 → 仍按业务 code 处理，不崩', async () => {
    mock.setScenario('http500');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /获取URL失败/, 'http500');
  });

  await test('非 JSON 响应 → 脚本报 unknow error，宿主不崩', async () => {
    mock.setScenario('html');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /unknow error/, 'non-json');
  });

  await test('不支持的动作 → 脚本拒绝 action not support', async () => {
    mock.setScenario('ok');
    await rejects(host.resolve('lyric', 'wy', { musicInfo: track }), /action not support/, 'lyric');
  });

  // ------------------------------------------------------------- 结果校验
  group('E. 结果校验（宿主兜底）');

  await test('非 http(s) 的 URL 必须被宿主拒绝', async () => {
    mock.setScenario('badurl');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /must be http/, 'badurl');
  });

  await test('超长 URL（>2048）必须被宿主拒绝', async () => {
    mock.setScenario('longurl');
    await rejects(host.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /url too long/, 'longurl');
  });

  await test('validateResult 直接调用也要拒绝非法值', () => {
    let threw = false;
    try {
      validateResult('musicUrl', 12345);
    } catch (e) {
      threw = /must be a string/.test(e.message);
    }
    ok(threw, 'validateResult 没拒绝数字');
  });

  // ------------------------------------------------------------- 超时/中断
  group('F. 超时与中断');

  await test('请求超时（脚本未传 timeout 时用宿主默认值）', async () => {
    mock.setScenario('slow');
    const slowHost = newHost({ requestTimeoutMs: 300 });
    await rejects(slowHost.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /timeout/, 'req-timeout');
  });

  await test('外层超时（handler 迟迟不返回）', async () => {
    mock.setScenario('slow');
    const outerHost = newHost({ requestTimeoutMs: 5000, resolveTimeoutMs: 200 });
    await rejects(outerHost.resolve('musicUrl', 'wy', { type: '320k', musicInfo: track }), /timed out after 200ms/, 'outer-timeout');
  });

  await test('cancel() 能中断在途请求', async () => {
    mock.setScenario('slow');
    const err = await new Promise((resolve) => {
      const cancel = host.lx.request(
        `${base}/music/url`,
        { method: 'POST', headers: { 'X-Api-Key': 'local-test-key' }, body: { source: 'wy' }, timeout: 5000 },
        (e) => resolve(e),
      );
      setTimeout(cancel, 30);
    });
    ok(err, 'cancel 之后回调没有收到错误');
    eq(err.code, 'ABORTED', 'err.code');
  });

  await test('未注册 request handler 时给出明确错误', async () => {
    const bare = createHost({ script: '/* noop */', globals: { __MOCK_BASE__: base } });
    bare.run();
    await rejects(bare.resolve('musicUrl', 'wy', { musicInfo: track }), /Request event is not defined/, 'no-handler');
  });

  // ------------------------------------------------------------------- 汇总
  mock.reset();
  const failed = results.filter((r) => !r.ok);
  console.log(`\n${'─'.repeat(56)}`);
  console.log(
    `\x1b[1m结果：\x1b[0m ${results.length - failed.length}/${results.length} 通过` +
      (failed.length ? `，\x1b[31m${failed.length} 失败\x1b[0m` : ' \x1b[32m全部通过\x1b[0m'),
  );
  if (VERBOSE) {
    console.log('\n\x1b[90mmock 记录：\x1b[0m');
    console.log(JSON.stringify(mock.stats(), null, 2).slice(0, 4000));
  }
  if (failed.length) {
    console.log('\n\x1b[31m失败项：\x1b[0m');
    for (const f of failed) console.log(`  - [${f.group}] ${f.name}: ${f.error}`);
  }

  await mock.close();
  process.exit(failed.length ? 1 : 0);
}

main().catch(async (e) => {
  console.error('\x1b[31mharness crashed:\x1b[0m', e);
  process.exit(1);
});
