'use strict';

/**
 * lx-runtime.js —— `globalThis.lx` 的**参考实现（golden reference）**
 *
 * 目标：把 lx-music-desktop 的脚本宿主语义，用最少的依赖、可测试的方式复刻出来。
 * 将来 Airmedy 的 Go + WebView 宿主必须跑通同一套场景（见 harness.js / README.md）。
 *
 * 语义来源（逐条对照）：
 *   lx-music-desktop/src/main/modules/userApi/renderer/preload.js
 *   - request(url, {method,timeout,headers,body,form,formData}, cb) → 返回 cancel()
 *   - send(inited|updateAlert) / on('request')
 *   - utils.crypto / buffer / zlib
 *   - version '2.0.0' / env 'desktop'
 *   - musicUrl、pic：必须是 http(s) 且 ≤2048
 *   - lyric：{lyric,tlyric,rlyric,lxlyric} 带长度上限
 *
 * 唯一有意的偏离：`filterSources()` 支持 tolerant 模式（宽松白名单）。
 * 原因见方案文档 §2.5：生态里有魔改版脚本会声明 `git` 与 hires/atmos 等超纲音质。
 */

const http = require('node:http');
const https = require('node:https');
const zlib = require('node:zlib');
const crypto = require('node:crypto');
const { URL } = require('node:url');

const EVENT_NAMES = Object.freeze({
  request: 'request',
  inited: 'inited',
  updateAlert: 'updateAlert',
});

const API_VERSION = '2.0.0';

// --- lx 官方白名单 ---------------------------------------------------------
const OFFICIAL_SOURCES = ['kw', 'kg', 'tx', 'wy', 'mg', 'local'];

const OFFICIAL_QUALITYS = {
  kw: ['128k', '320k', 'flac', 'flac24bit'],
  kg: ['128k', '320k', 'flac', 'flac24bit'],
  tx: ['128k', '320k', 'flac', 'flac24bit'],
  wy: ['128k', '320k', 'flac', 'flac24bit'],
  mg: ['128k', '320k', 'flac', 'flac24bit'],
  local: [],
};

const OFFICIAL_ACTIONS = {
  kw: ['musicUrl'],
  kg: ['musicUrl'],
  tx: ['musicUrl'],
  wy: ['musicUrl'],
  mg: ['musicUrl'],
  local: ['musicUrl', 'lyric', 'pic'],
};

const ALL_ACTIONS = ['musicUrl', 'lyric', 'pic'];

const LIMITS = Object.freeze({
  url: 2048,
  lyric: 51200,
  lyricSub: 5120,
  lxlyric: 8192,
  updateLog: 1024,
});

// --- 请求实现 --------------------------------------------------------------

/**
 * 对应 lx 的 `lx.request`。
 * @returns {() => void} cancel
 */
function createRequestImpl({ defaultTimeoutMs = 20000, maxTimeoutMs = 60000 } = {}) {
  return function request(url, options = {}, callback = () => {}) {
    const {
      method = 'get',
      timeout,
      headers = {},
      body,
      form,
      formData,
      follow_max = 10,
    } = options || {};

    let settled = false;
    let current = null;
    const timers = new Set();

    const done = (err, resp, parsed) => {
      if (settled) return;
      settled = true;
      for (const t of timers) clearTimeout(t);
      timers.clear();
      try {
        callback.call(null, err, resp, parsed);
      } catch (e) {
        // 回调本身抛错不应该炸宿主
        console.error('[lx-runtime] callback threw:', e && e.message);
      }
    };

    const cancel = () => {
      if (settled) return;
      if (current) current.destroy();
      done(Object.assign(new Error('aborted'), { code: 'ABORTED' }), null, null);
    };

    const effectiveTimeout = (() => {
      const t = Number(timeout);
      if (!Number.isFinite(t) || t <= 0) return Math.min(defaultTimeoutMs, maxTimeoutMs);
      return Math.min(t, maxTimeoutMs);
    })();

    const finish = (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const raw = Buffer.concat(chunks);
        let parsed = raw.toString();
        try {
          parsed = JSON.parse(parsed);
        } catch (_) {
          /* 保持字符串 */
        }
        done(
          null,
          {
            statusCode: res.statusCode,
            statusMessage: res.statusMessage,
            headers: res.headers,
            bytes: raw.length,
            raw,
            body: parsed,
          },
          parsed,
        );
      });
    };

    const go = (target, hopsLeft) => {
      let parsedUrl;
      try {
        parsedUrl = new URL(target);
      } catch (_) {
        return done(Object.assign(new Error(`invalid url: ${target}`), { code: 'EINVALIDURL' }), null, null);
      }

      const isHttps = parsedUrl.protocol === 'https:';
      const lib = isHttps ? https : http;

      let payload = null;
      const finalHeaders = Object.assign({}, headers);

      if (formData) {
        return done(
          Object.assign(new Error('formData is not supported by the reference implementation'), {
            code: 'ENOTSUP',
          }),
          null,
          null,
        );
      } else if (form) {
        payload = Object.keys(form)
          .map((k) => `${encodeURIComponent(k)}=${encodeURIComponent(form[k])}`)
          .join('&');
        if (!hasHeader(finalHeaders, 'content-type')) {
          finalHeaders['Content-Type'] = 'application/x-www-form-urlencoded';
        }
      } else if (body !== undefined && body !== null) {
        if (typeof body === 'string' || Buffer.isBuffer(body)) {
          payload = body;
        } else {
          // needle 的默认行为：对象 → JSON + application/json
          payload = JSON.stringify(body);
          if (!hasHeader(finalHeaders, 'content-type')) {
            finalHeaders['Content-Type'] = 'application/json';
          }
        }
      }

      if (payload != null) {
        finalHeaders['Content-Length'] = Buffer.byteLength(payload);
      }

      const req = lib.request(
        {
          protocol: parsedUrl.protocol,
          hostname: parsedUrl.hostname,
          port: parsedUrl.port,
          path: `${parsedUrl.pathname}${parsedUrl.search}`,
          method: String(method).toUpperCase(),
          headers: finalHeaders,
        },
        (res) => {
          const status = res.statusCode || 0;
          const location = res.headers.location;
          if (status >= 300 && status < 400 && location && hopsLeft > 0) {
            res.resume(); // 丢弃响应体
            current = null;
            const nextUrl = new URL(location, target).toString();
            return go(nextUrl, hopsLeft - 1);
          }
          finish(res);
        },
      );

      current = req;
      req.on('error', (err) => done(err, null, null));

      const timer = setTimeout(() => {
        req.destroy();
        done(Object.assign(new Error(`request timeout after ${effectiveTimeout}ms`), { code: 'ETIMEDOUT' }), null, null);
      }, effectiveTimeout);
      timers.add(timer);

      if (payload != null) req.write(payload);
      req.end();
    };

    try {
      go(String(url), Number.isFinite(Number(follow_max)) ? Number(follow_max) : 10);
    } catch (e) {
      done(e, null, null);
    }

    return cancel;
  };
}

function hasHeader(headers, name) {
  return Object.keys(headers).some((k) => k.toLowerCase() === name);
}

// --- utils -----------------------------------------------------------------

function createUtils() {
  return {
    crypto: {
      aesEncrypt(buffer, mode, key, iv) {
        const cipher = crypto.createCipheriv(mode, key, iv || null);
        return Buffer.concat([cipher.update(toBuffer(buffer)), cipher.final()]);
      },
      rsaEncrypt(buffer, key) {
        const buf = toBuffer(buffer);
        const padded = Buffer.concat([Buffer.alloc(Math.max(0, 128 - buf.length)), buf]);
        return crypto.publicEncrypt({ key, padding: crypto.constants.RSA_NO_PADDING }, padded);
      },
      randomBytes(size) {
        return crypto.randomBytes(size);
      },
      md5(str) {
        return crypto.createHash('md5').update(str).digest('hex');
      },
    },
    buffer: {
      from(...args) {
        return Buffer.from(...args);
      },
      bufToString(buf, format) {
        return Buffer.from(buf, 'binary').toString(format);
      },
    },
    zlib: {
      inflate(buf) {
        return new Promise((resolve, reject) => {
          zlib.inflate(toBuffer(buf), (err, out) => (err ? reject(err) : resolve(out)));
        });
      },
      deflate(data) {
        return new Promise((resolve, reject) => {
          zlib.deflate(toBuffer(data), (err, out) => (err ? reject(err) : resolve(out)));
        });
      },
    },
  };
}

function toBuffer(v) {
  if (Buffer.isBuffer(v)) return v;
  if (v instanceof Uint8Array) return Buffer.from(v.buffer, v.byteOffset, v.byteLength);
  if (typeof v === 'string') return Buffer.from(v);
  return Buffer.from(String(v));
}

// --- 源过滤 ----------------------------------------------------------------

/**
 * 对应 preload.js 里 handleInit 的过滤逻辑。
 * @param {object} sources 脚本声明的 sources
 * @param {{tolerant?: boolean}} opts tolerant=true 时接受未知 source / 未知音质
 */
function filterSources(sources, { tolerant = true } = {}) {
  const out = {};
  if (!sources || typeof sources !== 'object') return out;

  const keys = tolerant ? Object.keys(sources) : OFFICIAL_SOURCES;

  for (const source of keys) {
    const decl = sources[source];
    if (!decl || decl.type !== 'music') continue;

    const declaredActions = Array.isArray(decl.actions) ? decl.actions : [];
    const declaredQualitys = Array.isArray(decl.qualitys) ? decl.qualitys : [];

    if (tolerant) {
      const actions = declaredActions.filter((a) => ALL_ACTIONS.includes(a));
      if (actions.length === 0) continue;
      out[source] = { type: 'music', actions, qualitys: declaredQualitys.slice() };
    } else {
      const allowedQualitys = OFFICIAL_QUALITYS[source];
      const allowedActions = OFFICIAL_ACTIONS[source];
      if (!allowedQualitys || !allowedActions) continue; // 例如 git
      out[source] = {
        type: 'music',
        actions: allowedActions.filter((a) => declaredActions.includes(a)),
        qualitys: allowedQualitys.filter((q) => declaredQualitys.includes(q)),
      };
    }
  }
  return out;
}

// --- 结果校验 --------------------------------------------------------------

/** 对应 preload.js 的 handleRequest 里的返回值校验。抛错 = 宿主判失败。 */
function validateResult(action, response) {
  switch (action) {
    case 'musicUrl':
      assertHttpUrl(response, 'musicUrl');
      return response;
    case 'pic':
      assertHttpUrl(response, 'pic');
      return response;
    case 'lyric': {
      if (typeof response !== 'object' || response === null) throw new Error('lyric: invalid response');
      if (typeof response.lyric !== 'string') throw new Error('lyric: `lyric` is required');
      if (response.lyric.length > LIMITS.lyric) throw new Error('lyric: too large');
      const pick = (v, max) => (typeof v === 'string' && v.length > 0 && v.length <= max ? v : null);
      return {
        lyric: response.lyric,
        tlyric: pick(response.tlyric, LIMITS.lyricSub),
        rlyric: pick(response.rlyric, LIMITS.lyricSub),
        lxlyric: pick(response.lxlyric, LIMITS.lxlyric),
      };
    }
    default:
      throw new Error(`unsupported action: ${action}`);
  }
}

function assertHttpUrl(value, field) {
  if (typeof value !== 'string') throw new Error(`${field}: result must be a string`);
  if (value.length > LIMITS.url) throw new Error(`${field}: url too long (${value.length} > ${LIMITS.url})`);
  if (!/^https?:/.test(value)) throw new Error(`${field}: result must be http(s)`);
}

// --- 宿主实例 --------------------------------------------------------------

/**
 * 创建一个 `lx` 对象，并把 fixture 脚本跑起来。
 * @param {{script: string, tolerant?: boolean, requestTimeoutMs?: number, resolveTimeoutMs?: number}} opts
 */
function createHost({
  script,
  tolerant = true,
  requestTimeoutMs = 20000,
  resolveTimeoutMs = 20000,
  onLog = () => {},
  globals = {},
} = {}) {
  const state = {
    inited: null,
    initedError: null,
    updateAlerts: [],
    sources: {},
    openDevTools: false,
    handler: null,
    updateAlertShown: false,
  };

  const request = createRequestImpl({ defaultTimeoutMs: requestTimeoutMs });

  const lx = {
    EVENT_NAMES,
    version: API_VERSION,
    env: 'desktop',
    currentScriptInfo: { name: 'fixture', description: '', version: 'v0', author: 'p0', homepage: '', rawScript: script },
    request,
    utils: createUtils(),
    send(eventName, data) {
      return new Promise((resolve, reject) => {
        switch (eventName) {
          case EVENT_NAMES.inited: {
            if (state.inited) return reject(new Error('Script is inited'));
            if (!data || typeof data !== 'object') return reject(new Error('Missing required parameter init info'));
            state.openDevTools = data.openDevTools === true;
            try {
              state.sources = filterSources(data.sources, { tolerant });
            } catch (e) {
              state.initedError = e.message;
              return reject(e);
            }
            state.inited = { raw: data.sources, filtered: state.sources };
            return resolve();
          }
          case EVENT_NAMES.updateAlert: {
            if (state.updateAlertShown) {
              return reject(new Error('The update alert can only be called once.'));
            }
            if (data.log === undefined || typeof data.log !== 'string') {
              return reject(new Error('log is required.'));
            }
            state.updateAlertShown = true;
            const entry = {
              log: data.log.length > LIMITS.updateLog ? `${data.log.slice(0, LIMITS.updateLog)}...` : data.log,
              updateUrl: typeof data.updateUrl === 'string' ? data.updateUrl : undefined,
            };
            state.updateAlerts.push(entry);
            onLog(`[updateAlert] ${entry.log} ${entry.updateUrl || ''}`.trim());
            return resolve();
          }
          default:
            return reject(new Error(`The event is not supported: ${eventName}`));
        }
      });
    },
    on(eventName, handler) {
      if (eventName !== EVENT_NAMES.request) {
        return Promise.reject(new Error(`The event is not supported: ${eventName}`));
      }
      state.handler = handler;
      return Promise.resolve();
    },
  };

  // 把宿主对象装进一个独立 realm，避免脚本污染外层全局
  const vm = require('node:vm');
  const sandbox = {
    console,
    lx,
    URL,
    Buffer,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    TextEncoder,
    TextDecoder,
    ...globals,
  };
  vm.createContext(sandbox);

  const run = () => {
    vm.runInContext(script, sandbox, { filename: 'lx-source-fixture.js', timeout: 5000 });
  };

  /**
   * 模拟主进程发起一次请求。
   * 与 lx 一致：handler 必须返回 Promise；外层还有一层超时（lx 是 20s 的 requestKey 超时）。
   */
  const resolve = (action, source, info) => {
    if (!state.handler) return Promise.reject(new Error('Request event is not defined'));
    let result;
    try {
      result = state.handler({ source, action, info });
    } catch (e) {
      return Promise.reject(e);
    }
    if (!result || typeof result.then !== 'function') {
      return Promise.reject(new Error('request handler must return a Promise'));
    }

    const timeout = new Promise((_, reject) => {
      const t = setTimeout(() => reject(new Error(`request timed out after ${resolveTimeoutMs}ms`)), resolveTimeoutMs);
      t.unref?.();
    });

    return Promise.race([Promise.resolve(result), timeout]).then((response) => validateResult(action, response));
  };

  return { lx, state, sandbox, run, resolve };
}

module.exports = {
  EVENT_NAMES,
  API_VERSION,
  LIMITS,
  OFFICIAL_SOURCES,
  filterSources,
  validateResult,
  createHost,
  createRequestImpl,
  createUtils,
};
