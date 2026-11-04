/*
 * @name 本地 Mock 音源（P0 夹具）
 * @description 与 ikun 音源 v26 同构，请求指向本地 mock server
 * @version v0
 * @author airmedy-p0
 *
 * 说明：
 *   这个文件不是"好用的音源"，而是**协议夹具**。它刻意保留了真实音源里的这些特征，
 *   用来验证宿主（Airmedy 的 lx 兼容层）能不能照样跑：
 *     1. 解构 globalThis.lx（request/on/send/utils/env/version）
 *     2. POST 对象 body（宿主必须自动 JSON 化）+ 自定义请求头
 *     3. needle 专属选项 follow_max
 *     4. 业务错误码 200/403/429/500 的语义
 *     5. 脚本加载时异步 checkUpdate → lx.send(updateAlert)
 *     6. **声明非标准 source（git）与超纲音质（hires/atmos/master）** —— 宽容模式测试点
 *     7. 不声明 local 源（与真实音源一致）
 *     8. 脚本里夹带无害的怪异调用（console.groupEnd）
 */

const DEV_ENABLE = false
const UPDATE_ENABLE = true

// harness 会注入 __MOCK_BASE__；在真实 lx 客户端里跑时用固定端口
const API_URL = globalThis.__MOCK_BASE__ ?? 'http://127.0.0.1:18787'
const API_KEY = 'local-test-key'
const SCRIPT_MD5 = 'fixture-p0'

const MUSIC_QUALITY = JSON.parse('{"git":["128k","320k","flac"],"kg":["128k","320k","flac","flac24bit","hires","atmos","master"],"kw":["128k","320k","flac","flac24bit","hires"],"tx":["128k","320k","flac","flac24bit","hires","atmos","atmos_plus","master"],"wy":["128k","320k","flac","flac24bit","hires","atmos","master"]}');

const MUSIC_SOURCE = Object.keys(MUSIC_QUALITY);
const { EVENT_NAMES, request, on, send, utils, env, version } = globalThis.lx;

const httpFetch = (url, options = { method: 'GET' }) => {
  return new Promise((resolve, reject) => {
    request(url, options, (err, resp) => {
      if (err) return reject(err);
      resolve(resp);
    });
  });
};

const handleGetMusicUrl = async (source, musicInfo, quality) => {
  // 与真实音源一致：强依赖平台 ID
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
  if (env !== 'mobile') console.groupEnd(); // 真实脚本里就有这行，宿主不能因此判失败

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
    const resp = await httpFetch(
      `${API_URL}/script/lxmusic?checkUpdate=${SCRIPT_MD5}&key=${API_KEY}`,
      { method: 'GET', headers: { 'Content-Type': 'application/json' } },
    );
    const { body } = resp;
    if (!body || body.code !== 200) return console.log('checkUpdate failed');
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
