/*
 * @name 公开接口解析（PoC）
 * @description 用公开接口把网易云 songmid 换成直链；仅免费曲可用，VIP 会明确失败
 * @version v0
 * @author airmedy-poc
 *
 * 这是一个**合规的参考实现**：不登录、不携带任何账号 cookie，
 * 只用 music.163.com 的公开接口。它的存在是为了证明：
 *
 *   搜索拿到的 ID → 自定义源协议(musicUrl) → 真实可播放直链 → 下载
 *
 * 这条链路能跑通。VIP 曲目会得到 code=-110 / url=null，脚本如实抛错，
 * 宿主据此可以提示"该曲目需要会员或更换音源"。
 */

const { EVENT_NAMES, request, on, send, env, version } = globalThis.lx

const API = 'https://music.163.com/api/song/enhance/player/url'
const BR = { '128k': 128000, '320k': 320000, flac: 999000 }

const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36'

const httpFetch = (url, options = { method: 'GET' }) =>
  new Promise((resolve, reject) => {
    request(url, options, (err, resp) => (err ? reject(err) : resolve(resp)))
  })

const getMusicUrl = async (musicInfo, quality) => {
  const id = musicInfo.songmid ?? musicInfo.id ?? musicInfo.hash
  if (!id) throw new Error('缺少 songmid，无法解析')

  const br = BR[quality] ?? 320000
  const resp = await httpFetch(
    `${API}?ids=[${id}]&br=${br}`,
    {
      method: 'GET',
      headers: { 'User-Agent': UA, Referer: 'https://music.163.com/' },
      timeout: 15000,
    },
  )

  const body = resp.body
  if (!body || typeof body !== 'object') throw new Error('接口返回异常（非 JSON）')

  const item = (body.data || [])[0]
  if (!item) throw new Error('接口未返回曲目数据')
  if (item.code === -110) throw new Error('无可用直链：需要会员或受版权限制')
  if (!item.url) throw new Error(`无可用直链（code=${item.code}）`)

  return item.url
}

on(EVENT_NAMES.request, ({ action, info }) => {
  switch (action) {
    case 'musicUrl':
      return getMusicUrl(info.musicInfo, info.type)
    default:
      return Promise.reject('action not support')
  }
})

send(EVENT_NAMES.inited, {
  status: true,
  openDevTools: false,
  sources: {
    wy: {
      name: '网易云（公开接口）',
      type: 'music',
      actions: ['musicUrl'],
      qualitys: ['128k', '320k', 'flac'],
    },
  },
})
