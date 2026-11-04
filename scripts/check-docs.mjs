#!/usr/bin/env node
// check-docs.mjs — keep catalog/ navigable and honest.
//
// Checks:
//   1. every catalog/<area>/README.md is listed in catalog/README.md
//   2. every relative markdown link resolves, including its #anchor
//   3. a file longer than TOC_THRESHOLD lines carries a <!-- toc --> block
//
// Usage:
//   node scripts/check-docs.mjs          # check only
//   node scripts/check-docs.mjs --fix    # also (re)generate the TOC blocks

import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const CATALOG = join(ROOT, 'catalog')
const INDEX = join(CATALOG, 'README.md')
const TOC_THRESHOLD = 400
const SPLIT_WARN = 800
const TOC_START = '<!-- toc -->'
const TOC_END = '<!-- /toc -->'
const fix = process.argv.includes('--fix')

const errors = []
const warnings = []

/** Read markdown with LF newlines; remember whether the file used CRLF. */
function readMarkdown(file) {
  const raw = readFileSync(file, 'utf8')
  return {
    markdown: raw.split('\r\n').join('\n'),
    crlf: raw.includes('\r\n'),
  }
}

function writeMarkdown(file, markdown, crlf) {
  const out = crlf ? markdown.split('\n').join('\r\n') : markdown
  writeFileSync(file, out)
}

function walk(dir, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) walk(full, out)
    else if (entry.name.endsWith('.md')) out.push(full)
  }
  return out
}

/** GitHub-style heading slug. Good enough for catalog/ (English headings). */
function slug(heading) {
  return heading
    .replace(/`[^`]*`/g, m => m.slice(1, -1))
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s-]/gu, '')
    .trim()
    .replace(/\s+/g, '-')
}

function headings(markdown) {
  const out = []
  let fenced = false
  for (const line of markdown.split('\n')) {
    if (line.trimStart().startsWith('```')) fenced = !fenced
    if (fenced) continue
    const m = /^(#{1,6})\s+(.*)$/.exec(line)
    if (m) out.push({ level: m[1].length, text: m[2].trim() })
  }
  return out
}

// ---------------------------------------------------------------- 1. index coverage
const indexText = readMarkdown(INDEX).markdown
const areas = readdirSync(CATALOG, { withFileTypes: true })
  .filter(e => e.isDirectory())
  .map(e => e.name)
  .filter(name => {
    try {
      return statSync(join(CATALOG, name, 'README.md')).isFile()
    } catch {
      return false
    }
  })
  .sort()

for (const area of areas) {
  if (!indexText.includes(`](${area}/README.md)`)) {
    errors.push(`catalog/README.md does not list catalog/${area}/README.md`)
  }
}

// ---------------------------------------------------------------- 3. TOC blocks
function tocBody(markdown) {
  const lines = headings(markdown).filter(h => h.level === 2 || h.level === 3)
  if (lines.length < 3) return ''
  return lines
    .map(h => {
      const indent = h.level === 3 ? '  ' : ''
      return `${indent}- [${h.text}](#${slug(h.text)})`
    })
    .join('\n')
}

function applyToc(markdown) {
  const start = markdown.indexOf(TOC_START)
  const end = markdown.indexOf(TOC_END)
  const body = tocBody(markdown)
  if (!body) return null

  if (start !== -1 && end > start) {
    return `${markdown.slice(0, start)}${TOC_START}\n${body}\n${TOC_END}${markdown.slice(end + TOC_END.length)}`
  }

  const h1 = /^#\s+.*$/m.exec(markdown)
  if (!h1) return null
  const at = h1.index + h1[0].length
  const rest = markdown.slice(at).replace(/^\n+/, '')
  return `${markdown.slice(0, at)}\n\n${TOC_START}\n${body}\n${TOC_END}\n\n${rest}`
}

const files = walk(CATALOG)
const lineCounts = []

for (const file of files) {
  const { markdown, crlf } = readMarkdown(file)
  const lines = markdown.split('\n').length
  lineCounts.push({ file, lines })

  if (lines > TOC_THRESHOLD && !(markdown.includes(TOC_START) && markdown.includes(TOC_END))) {
    const next = fix ? applyToc(markdown) : null
    if (next) {
      writeMarkdown(file, next, crlf)
      console.log(`toc: added to ${relative(ROOT, file)}`)
      continue
    }
    errors.push(`${relative(ROOT, file)} has ${lines} lines and no ${TOC_START} block`)
  }
}

// ---------------------------------------------------------------- 2. links + anchors
const anchorCache = new Map()
function anchorsOf(file) {
  if (!anchorCache.has(file)) {
    const set = new Set(headings(readMarkdown(file).markdown).map(h => slug(h.text)))
    anchorCache.set(file, set)
  }
  return anchorCache.get(file)
}

const LINK = /\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g

for (const file of walk(CATALOG)) {
  const markdown = readMarkdown(file).markdown
  const ownDir = dirname(file)
  for (const match of markdown.matchAll(LINK)) {
    const target = match[1]
    if (/^(https?:|mailto:)/.test(target)) continue

    const [rawPath, anchor] = target.split('#')
    if (rawPath === '') {
      if (anchor && !anchorsOf(file).has(anchor)) {
        errors.push(`${relative(ROOT, file)}: broken local anchor #${anchor}`)
      }
      continue
    }

    const resolved = resolve(ownDir, rawPath)
    let stat
    try {
      stat = statSync(resolved)
    } catch {
      errors.push(`${relative(ROOT, file)}: missing link target ${target}`)
      continue
    }
    if (stat.isDirectory() || !resolved.endsWith('.md')) continue
    if (anchor && !anchorsOf(resolved).has(anchor)) {
      errors.push(`${relative(ROOT, file)}: broken anchor #${anchor} in ${target}`)
    }
  }
}

// ---------------------------------------------------------------- report
lineCounts.sort((a, b) => b.lines - a.lines)
for (const { file, lines } of lineCounts.slice(0, 3)) {
  if (lines > SPLIT_WARN) {
    warnings.push(`${relative(ROOT, file)} is ${lines} lines; consider splitting it by concern`)
  }
}

for (const warning of warnings) console.warn(`warn: ${warning}`)
for (const error of errors) console.error(`error: ${error}`)

if (errors.length) {
  console.error(`\n${errors.length} problem(s) in catalog/.`)
  process.exit(1)
}

console.log(`catalog/: ${areas.length} areas, ${files.length} files — ok`)
