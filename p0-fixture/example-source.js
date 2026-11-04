/*!
 * @name ikun 音源（示例自定义源）
 * @description 与 ikun-music-source.js v26 同构的示例源，接口指向可配置的自建服务端
 * @version v26-demo
 * @author airmedy
 */

/*
 * 这是一份**示例自定义源**，不是可以直接播放的音源。
 *
 * 它放在插件目录根部，由 wrapper.js 装进 vm 沙箱运行；插件本身没有子插件层级。
 *
 * 它刻意保留了真实 ikun 音源的结构（前端脚本只是壳，解析在服务端）：
 *   1. 解构 globalThis.lx（request / on / send / utils / env / version）
 *   2. POST 对象 body（宿主必须自动 JSON 化）+ 自定义请求头
 *   3. needle 专属选项 follow_max
 *   4. 业务错误码 200 / 403 / 429 / 500 的语义
 *   5. 加载时异步 checkUpdate → lx.send(updateAlert)
 *   6. 声明非标准 source（git）与超纲音质（hires/atmos/master）
 *   7. 不声明 local 源
 *
 * 与原版的差异只有三处，都是有意的：
 *   - API_URL 走 LX_DEMO_API_URL，默认是本机 mock（p0-fixture/mock-server.js）
 *   - API_KEY 是假值
 *   - 版本注释标注 demo，避免被误当成真实付费源
 *
 * 想用自己的服务端：把 API_URL 换成你的地址即可（接口契约照原版：
 * POST /music/url {source, musicId, quality} → {code, url}）。
 */

const DEV_ENABLE = false;
const UPDATE_ENABLE = true;
const API_URL = globalThis.LX_DEMO_API_URL || 'http://127.0.0.1:18787';
const API_KEY = 'local-test-key';
const SCRIPT_MD5 = 'demo-fixture';

const MUSIC_QUALITY = JSON.parse(
  '{"git":["128k","320k","flac"],"kg":["128k","320k","flac","flac24bit","hires","atmos","master"],"kw":["128k","320k","flac","flac24bit","hires"],"tx":["128k","320k","flac","flac24bit","hires","atmos","atmos_plus","master"],"wy":["128k","320k","flac","flac24bit","hires","atmos","master"]}',
);

const MUSIC_SOURCE = Object.keys(MUSIC_QUALITY);
const { EVENT_NAMES, request, on, send, env, version } = globalThis.lx;

const httpFetch = (url, options = { method: 'GET' }) => {
  return new Promise((resolve, reject) => {
    request(url, options, (err, resp) => {
      if (err) return reject(err);
      resolve(resp);
    });
  });
};

const handleGetMusicUrl = async (source, musicInfo, quality) => {
  // 与原版一致：强依赖平台 ID，hash 优先、songmid 回退。
  const songId = musicInfo.hash ?? musicInfo.songmid;
  const resp = await httpFetch(`${API_URL}/music/url`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'User-Agent': env ? `lx-music-${env}/${version}` : `lx-music-request/${version}`,
      'X-Api-Key': API_KEY,
    },
    body: {
      source: source,
      musicId: songId,
      quality: quality,
    },
    follow_max: 5,
  });

  const { body } = resp;
  if (!body || isNaN(Number(body.code))) throw new Error('unknow error');
  if (env !== 'mobile') console.groupEnd();

  switch (Number(body.code)) {
    case 200:
      return body.url;
    case 403:
      throw new Error('鉴权失败');
    case 429:
      throw new Error('请求过速');
    case 500:
      throw new Error(`获取URL失败, ${body.message ?? '未知错误'}`);
    default:
      throw new Error(body.message ?? '未知错误');
  }
};

const checkUpdate = async () => {
  try {
    const resp = await httpFetch(`${API_URL}/script/lxmusic?checkUpdate=${SCRIPT_MD5}&key=${API_KEY}`, {
      method: 'GET',
      headers: { 'Content-Type': 'application/json' },
    });
    const { body } = resp;
    if (!body || body.code !== 200) return;
    if (body.data == null) return;
    await send(EVENT_NAMES.updateAlert, {
      log: body.data.updateMsg,
      updateUrl: body.data.updateUrl,
    });
  } catch (err) {
    console.log('checkUpdate error:', err && err.message);
  }
};

const musicSources = {};
MUSIC_SOURCE.forEach((item) => {
  musicSources[item] = {
    name: item,
    type: 'music',
    actions: ['musicUrl'],
    qualitys: MUSIC_QUALITY[item],
  };
});

on(EVENT_NAMES.request, ({ action, source, info }) => {
  switch (action) {
    case 'musicUrl':
      return handleGetMusicUrl(source, info.musicInfo, info.type)
        .then((data) => Promise.resolve(data))
        .catch((err) => Promise.reject(err));
    default:
      return Promise.reject('action not support');
  }
});

if (UPDATE_ENABLE) checkUpdate();

send(EVENT_NAMES.inited, {
  status: true,
  openDevTools: DEV_ENABLE,
  sources: musicSources,
});
