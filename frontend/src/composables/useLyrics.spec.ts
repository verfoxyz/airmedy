import { describe, expect, it } from 'vitest'
import { ref } from 'vue'
import { useLyrics } from './useLyrics'

const parse = (content: string) => useLyrics(ref(content))

// The real-world shape this feature targets: an original block followed by a
// translation block that repeats every timestamp exactly. BOM and trailing
// whitespace are stripped by the local reader before the frontend sees it.
const DUPLICATE_TIMESTAMP_LRC = `[00:00.000]作词: s0rrow
[00:01.000]作曲: s0rrow
[00:02.000]编曲: s0rrow
[00:03.000]制作人: s0rrow
[00:16.270]Every day we talk a little less
[00:19.718]It looks like you are losing interest
[00:23.732]These feelings I have for you
[00:26.831]But you don't feel the same
[00:28.708]So I'll pack all my things
[00:30.774]And go run far away
[00:32.797]You are, you are very pretty
[00:35.787]I'm so very ugly
[00:37.676]Will you even love me, anymore (love me)
[00:40.975]You can, you can live without me
[00:43.892]That makes me unhappy
[00:46.052]I should get a piercing through my heart
[00:49.229]What do you even want me to be
[00:52.703]You never ever pay attention to me
[00:56.688]So I'll close my blinds in misery
[01:00.535]And I'll wait for you for a couple of weeks
[01:05.310]You are, you are very pretty
[01:08.511]I'm so very ugly
[01:10.406]Will you even love me, anymore (love me)
[01:13.598]You can, you can live without me
[01:16.579]That makes me unhappy
[01:18.755]I should get a piercing through my heart

[00:16.270]每天我们说的话都在逐渐减少
[00:19.718]看起来你正在慢慢失去兴趣
[00:23.732]这些我对你的感情
[00:26.831]可你却没有同样的感觉
[00:28.708]所以我会收拾好我的一切
[00:30.774]然后逃到很远的地方
[00:32.797]你啊 你真的很漂亮
[00:35.787]而我是那么的丑陋
[00:37.676]你会再爱我吗 哪怕一点点（爱我）
[00:40.975]你可以 你可以没有我的生活着
[00:43.892]那让我感到不开心
[00:46.052]我应该将我的心也一并刺穿
[00:49.229]你究竟想要我成为什么样子
[00:52.703]你从来从来都不愿多看我一眼
[00:56.688]所以我拉上窗帘沉进悲伤里
[01:00.535]然后我会一直等着你好几个星期
[01:05.310]你啊 你真的很漂亮
[01:08.511]而我是那么的丑陋
[01:10.406]你会再爱我吗 哪怕一点点（爱我）
[01:13.598]你可以 你可以没有我的生活着
[01:16.579]那让我感到不开心
[01:18.755]我应该将我的心也一并刺穿`

describe('duplicate timestamps', () => {
  it('merges an original block and a translation block into one line per timestamp', () => {
    const { syncedLines } = parse('[00:01]First\n[00:02]Second\n\n[00:01]第一条\n[00:02]第二条')
    expect(syncedLines.value).toEqual([
      { time: 1, text: 'First', secondary: '第一条' },
      { time: 2, text: 'Second', secondary: '第二条' },
    ])
  })

  it('uses only the second record as the translation when three share a timestamp', () => {
    const { syncedLines } = parse('[00:01]Original\n[00:01]Translation\n[00:01]Ignored')
    expect(syncedLines.value).toHaveLength(1)
    expect(syncedLines.value[0].text).toBe('Original')
    expect(syncedLines.value[0].secondary).toBe('Translation')
  })

  it('keeps the first non-empty record as the primary line', () => {
    const { syncedLines } = parse('[00:01]\n[00:01]Original\n[00:01]Translation')
    expect(syncedLines.value[0]).toMatchObject({ time: 1, text: 'Original', secondary: 'Translation' })
  })

  it('leaves lyrics without duplicates unchanged', () => {
    const { syncedLines } = parse('[00:01]First\n[00:02]Second')
    expect(syncedLines.value).toEqual([
      { time: 1, text: 'First', secondary: undefined },
      { time: 2, text: 'Second', secondary: undefined },
    ])
  })

  it('merges the real bilingual local LRC into 26 lines', () => {
    const { syncedLines, isSynced } = parse(DUPLICATE_TIMESTAMP_LRC)
    expect(isSynced.value).toBe(true)
    expect(syncedLines.value).toHaveLength(26)
    const first = syncedLines.value.find(line => line.time === 16.27)
    expect(first?.text).toBe('Every day we talk a little less')
    expect(first?.secondary).toBe('每天我们说的话都在逐渐减少')
  })
})

describe('Enhanced LRC', () => {
  it('uses the line timestamp for the first word and preserves Vietnamese and spaces', () => {
    const { syncedLines, plainLines } = parse('[00:01.62]Người <00:01.79>hỏi <00:02.05>anh\u00a0<00:02.30>rằng\n[00:02.58]')
    expect(syncedLines.value[0]).toEqual({
      time: 1.62, text: 'Người hỏi anh\u00a0rằng', secondary: undefined,
      words: [
        { text: 'Người ', start: 1.62, end: 1.79 },
        { text: 'hỏi ', start: 1.79, end: 2.05 },
        { text: 'anh\u00a0', start: 2.05, end: 2.3 },
        { text: 'rằng', start: 2.3, end: 2.58 },
      ],
    })
    expect(syncedLines.value[1].text).toBe('')
    expect(plainLines.value).toEqual([{ primary: 'Người hỏi anh\u00a0rằng', secondary: undefined }])
  })

  it('honors explicit end markers even beyond the one second fallback', () => {
    const { syncedLines } = parse('[00:01]<00:01.0>Hello <00:02.123>world<00:06.50>\n[00:10]Next')
    expect(syncedLines.value[0].words).toEqual([
      { text: 'Hello ', start: 1, end: 2.123 }, { text: 'world', start: 2.123, end: 6.5 },
    ])
    expect(syncedLines.value[1].words).toBeUndefined()
  })

  it('caps missing ends at one second, including the last line', () => {
    const { syncedLines } = parse('[00:01]one <00:02>two\n[00:20]three <00:21>four')
    expect(syncedLines.value.map(line => line.words?.at(-1)?.end)).toEqual([3, 22])
  })

  it('preserves spaces between an explicit word end and the next word start', () => {
    const { syncedLines } = parse('[00:01]<00:01>Hello<00:02> <00:03>world<00:04>')
    expect(syncedLines.value[0].words).toEqual([
      { text: 'Hello ', start: 1, end: 2 }, { text: 'world', start: 3, end: 4 },
    ])
  })

  it('cleans inline tags before bilingual and romanization consumers see the text', () => {
    const { syncedLines, plainLines } = parse('[00:01]你<00:02>好<00:03> ^ Xin <00:02>chào')
    expect(syncedLines.value[0].text).toBe('你好')
    expect(syncedLines.value[0].secondary).toBe('Xin chào')
    expect(syncedLines.value[0].words?.at(-1)?.end).toBe(3)
    expect(plainLines.value[0]).toEqual({ primary: '你好', secondary: 'Xin chào' })
  })

  it.each(['<00:00.5>', '<00:61>', '<00:bad>', '<00:02.1234>'])('falls back on invalid timing %s', marker => {
    const { syncedLines } = parse(`[00:01]one ${marker}two`)
    expect(syncedLines.value[0].words).toBeUndefined()
    expect(syncedLines.value[0].text).toBe('one two')
  })

  it('accepts equal timestamps and decodes entities without interpreting HTML', () => {
    const { syncedLines } = parse('[00:01]A &lt;00:01&gt;B &amp; C<00:02>')
    expect(syncedLines.value[0].words).toEqual([
      { text: 'A ', start: 1, end: 1 }, { text: 'B & C', start: 1, end: 2 },
    ])
  })

  it('retains ordinary LRC and plain lyrics, and reparses a changed track', () => {
    const content = ref('[00:01.00]First / Translation\n[00:02.00]Second')
    const lyrics = useLyrics(content)
    expect(lyrics.isSynced.value).toBe(true)
    expect(lyrics.syncedLines.value[0]).toEqual({ time: 1, text: 'First', secondary: 'Translation' })
    content.value = 'Plain lyrics'
    expect(lyrics.isSynced.value).toBe(false)
    expect(lyrics.syncedLines.value).toEqual([])
    expect(lyrics.plainLines.value).toEqual([{ primary: 'Plain lyrics', secondary: undefined }])
  })
})
