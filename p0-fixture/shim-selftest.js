'use strict';

/**
 * shim-selftest.js —— `lx.utils` 的已知向量测试
 *
 * 为什么单独测 utils：
 *   真实音源（尤其 kw/wy/tx 的旧版）大量依赖 aesEncrypt / rsaEncrypt / md5 做签名。
 *   这部分一旦实现错了，表现为"接口一直返回空"，极难排查。
 *   这里用**固定向量**钉死行为，避免"实现和测试互相印证"的假绿。
 *
 * 运行：node shim-selftest.js
 */

const crypto = require('node:crypto');
const { createUtils } = require('./lx-runtime');

const utils = createUtils();

// 网易云常量（公开值，来自 Binaryify/NeteaseCloudMusicApi 与 lx 的 wy/utils/crypto.js）
const PRESET_KEY = Buffer.from('0CoJUm6Qyw8W8jud');
const IV = Buffer.from('0102030405060708');
const EAPI_KEY = Buffer.from('e82ckenh8dichen8');
const PUBLIC_KEY = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDgtQn2JZ34ZC28NWYpAUd98iZ3
7BUrX/aKzmFbt7clFSs6sXqHauqKWqdtLkF2KexO40H1YTX8z2lSgBBOAxLsvakl
V8k4cBFK9snQXE9/DDaFt6Rr7iVZMldczhC0JNgTz+SHXT6CBHuX3e9SdB1Ua44o
ncaTWz7OBGLbCiK45wIDAQAB
-----END PUBLIC KEY-----`;

const cases = [];
const test = (name, fn) => cases.push({ name, fn });

function eq(actual, expected, label) {
  if (actual !== expected) throw new Error(`${label || 'value'}: expected ${expected}, got ${actual}`);
}

// --- 1. md5 -----------------------------------------------------------------
test('md5 已知向量', () => {
  eq(utils.crypto.md5('hello'), '5d41402abc4b2a76b9719d911017c592', 'md5(hello)');
  eq(utils.crypto.md5(''), 'd41d8cd98f00b204e9800998ecf8427e', 'md5("")');
});

// --- 2. AES -----------------------------------------------------------------
test('AES-128-CBC 已知向量（weapi 第一层）', () => {
  const out = utils.crypto.aesEncrypt(Buffer.from('test'), 'aes-128-cbc', PRESET_KEY, IV);
  eq(out.toString('hex'), '082852acd9c539e414295ed7417a1d22', 'cbc(test)');
});

test('AES-128-ECB 已知向量（eapi / linuxapi）', () => {
  const out = utils.crypto.aesEncrypt(Buffer.from('test'), 'aes-128-ecb', EAPI_KEY, null);
  eq(out.toString('hex'), 'b477dd46e47c5547517ac93db4a5a784', 'ecb(test)');
});

test('AES 支持 Uint8Array 输入（WebView 里脚本常用）', () => {
  const bytes = new Uint8Array(Buffer.from('test'));
  const out = utils.crypto.aesEncrypt(bytes, 'aes-128-cbc', PRESET_KEY, IV);
  eq(out.toString('hex'), '082852acd9c539e414295ed7417a1d22', 'cbc(uint8)');
});

// --- 3. RSA no padding ------------------------------------------------------
test('RSA_NO_PADDING：输出 128 字节且确定性', () => {
  const a = utils.crypto.rsaEncrypt(Buffer.from(PRESET_KEY), PUBLIC_KEY);
  const b = utils.crypto.rsaEncrypt(Buffer.from(PRESET_KEY), PUBLIC_KEY);
  eq(a.length, 128, 'length');
  eq(a.toString('hex'), b.toString('hex'), 'deterministic');
});

test('RSA_NO_PADDING：短输入左侧补零到 128 字节', () => {
  const short = utils.crypto.rsaEncrypt(Buffer.from([1, 2, 3]), PUBLIC_KEY);
  const manual = utils.crypto.rsaEncrypt(Buffer.concat([Buffer.alloc(125), Buffer.from([1, 2, 3])]), PUBLIC_KEY);
  eq(short.toString('hex'), manual.toString('hex'), 'left padding');
});

// --- 4. buffer --------------------------------------------------------------
test('buffer.from / bufToString', () => {
  const buf = utils.buffer.from('测试', 'utf8');
  eq(buf.toString('hex'), Buffer.from('测试', 'utf8').toString('hex'), 'from utf8');
  eq(utils.buffer.bufToString(buf, 'base64'), Buffer.from('测试', 'utf8').toString('base64'), 'bufToString');
});

// --- 5. zlib ----------------------------------------------------------------
test('zlib deflate / inflate 往返', async () => {
  const src = Buffer.from('x'.repeat(4096));
  const packed = await utils.zlib.deflate(src);
  if (!Buffer.isBuffer(packed)) throw new Error('deflate 没返回 Buffer');
  const back = await utils.zlib.inflate(packed);
  eq(back.toString(), src.toString(), 'roundtrip');
});

// --- 6. weapi 组合（端到端）------------------------------------------------
test('weapi 组合：可被标准 AES 解开，encSecKey 256 hex', () => {
  const secretKey = Buffer.from('abcdefghijklmnop');
  const text = JSON.stringify({ s: '晴天', type: 1, limit: 10 });

  const inner = utils.crypto.aesEncrypt(Buffer.from(text), 'aes-128-cbc', PRESET_KEY, IV).toString('base64');
  const params = utils.crypto.aesEncrypt(Buffer.from(inner), 'aes-128-cbc', secretKey, IV).toString('base64');
  const encSecKey = utils.crypto
    .rsaEncrypt(Buffer.from(secretKey).reverse(), PUBLIC_KEY)
    .toString('hex');

  // 用 Node 标准库反向解开，证明加密结果合法
  const d1 = crypto.createDecipheriv('aes-128-cbc', secretKey, IV);
  const b64 = Buffer.concat([d1.update(Buffer.from(params, 'base64')), d1.final()]).toString();
  const d2 = crypto.createDecipheriv('aes-128-cbc', PRESET_KEY, IV);
  const plain = Buffer.concat([d2.update(Buffer.from(b64, 'base64')), d2.final()]).toString();

  eq(plain, text, '解密回原文');
  eq(encSecKey.length, 256, 'encSecKey 十六进制长度');
});

// --- 7. 随机数 --------------------------------------------------------------
test('randomBytes 长度与随机性', () => {
  const a = utils.crypto.randomBytes(16);
  const b = utils.crypto.randomBytes(16);
  eq(a.length, 16, 'length');
  if (a.toString('hex') === b.toString('hex')) throw new Error('randomBytes 疑似不随机');
});

// --- runner -----------------------------------------------------------------
(async () => {
  let failed = 0;
  for (const c of cases) {
    try {
      await c.fn();
      console.log(`  \x1b[32m✓\x1b[0m ${c.name}`);
    } catch (e) {
      failed++;
      console.log(`  \x1b[31m✗\x1b[0m ${c.name}`);
      console.log(`      \x1b[31m${e.message}\x1b[0m`);
    }
  }
  console.log(`\n${cases.length - failed}/${cases.length} 通过`);
  process.exit(failed ? 1 : 0);
})();
