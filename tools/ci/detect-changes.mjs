#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import { appendFileSync, readFileSync } from 'node:fs'
import process from 'node:process'

const ZERO_SHA = '0'.repeat(40)
const FULL_CHANGE_SENTINEL = '__full_change_detection__'
const LIVE_GENERATION_ROLLOUT =
  /^runbooks\/GENERATION-CLEAN-BOUNDARY-ROLLOUT-\d{4}-\d{2}-\d{2}\.md$/

export const FILTERS = {
  docs: [
    '**/*.md',
    '**/*.mdx',
    '**/*.mdc',
    'AGENTS.md',
    'CLAUDE.md',
    'CODEX.md',
    'MEMORY.md',
    'conversa.md',
    '.claude/**',
    '.cursor/**',
    '.jules/**',
    '.windsurf/**',
    'docs/**',
    'runbooks/**',
    'templates/**/*.md',
  ],
}

function splitLines(value) {
  return value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
}

function runGit(args) {
  return execFileSync('git', args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
}

function hasGitObject(ref) {
  try {
    execFileSync('git', ['cat-file', '-e', `${ref}^{commit}`], {
      stdio: 'ignore',
    })
    return true
  } catch {
    return false
  }
}

export function normalizePath(filePath) {
  return filePath.replace(/\\/g, '/').replace(/^\.\//, '')
}

export function matchesPattern(filePath, pattern) {
  const file = normalizePath(filePath)
  const normalizedPattern = normalizePath(pattern)

  if (normalizedPattern === '**') return true

  if (normalizedPattern.includes('/**/*.')) {
    const [prefix, suffix] = normalizedPattern.split('/**/*')
    return file.startsWith(`${prefix}/`) && file.endsWith(suffix)
  }

  if (normalizedPattern.startsWith('**/*.')) {
    const ext = normalizedPattern.slice('**/*'.length)
    return file.endsWith(ext)
  }

  if (normalizedPattern.endsWith('/**')) {
    const prefix = normalizedPattern.slice(0, -'/**'.length)
    return file === prefix || file.startsWith(`${prefix}/`)
  }

  return file === normalizedPattern
}

export function isDocsFile(filePath) {
  return FILTERS.docs.some((pattern) => matchesPattern(filePath, pattern))
}

function forcesFullCI(filePath) {
  return LIVE_GENERATION_ROLLOUT.test(normalizePath(filePath))
}

export function evaluateFilters(files, filterNames) {
  const normalizedFiles = files.map(normalizePath)
  const result = {}

  for (const filterName of filterNames) {
    if (filterName === 'full') {
      result[filterName] = normalizedFiles.some(
        (file) => !isDocsFile(file) || forcesFullCI(file)
      )
      continue
    }

    const patterns = FILTERS[filterName]
    if (!patterns) throw new Error(`Unknown change filter: ${filterName}`)

    result[filterName] = normalizedFiles.some((file) =>
      patterns.some((pattern) => matchesPattern(file, pattern))
    )
  }

  return result
}

export function resolveRange(eventName, payload, githubSha) {
  if (eventName === 'workflow_dispatch') {
    return { forceAll: true, mode: 'workflow_dispatch' }
  }

  if (eventName === 'pull_request') {
    const head = payload.pull_request?.head?.sha ?? githubSha
    if (
      payload.action === 'synchronize' &&
      payload.before &&
      payload.before !== ZERO_SHA
    ) {
      return {
        base: payload.before,
        fallbackBase: payload.pull_request?.base?.sha,
        head,
        mode: 'incremental',
      }
    }

    return {
      base: payload.pull_request?.base?.sha,
      head,
      mode: 'pull_request',
    }
  }

  if (eventName === 'push') {
    return {
      base: payload.before,
      head: payload.after ?? githubSha,
      mode:
        payload.before && payload.before !== ZERO_SHA
          ? 'incremental'
          : 'single_commit',
    }
  }

  return { base: undefined, head: githubSha, mode: 'single_commit' }
}

export function resolveComparisonRanges(eventName, payload, githubSha) {
  const current = resolveRange(eventName, payload, githubSha)
  if (eventName !== 'pull_request') return { current }

  const head = payload.pull_request?.head?.sha ?? githubSha
  const base = payload.pull_request?.base?.sha
  const pullRequest = { base, head, mode: 'pull_request' }
  return { current, pullRequest }
}

export function changedFilesForRange(range) {
  if (range.forceAll) return [FULL_CHANGE_SENTINEL]
  if (!range.head)
    throw new Error('Unable to resolve head SHA for change detection')

  if (!hasGitObject(range.head)) {
    throw new Error(`Head commit is not available in checkout: ${range.head}`)
  }

  if (range.base && range.base !== ZERO_SHA && !hasGitObject(range.base)) {
    if (!range.fallbackBase || !hasGitObject(range.fallbackBase)) {
      return [FULL_CHANGE_SENTINEL]
    }
    return splitLines(
      runGit([
        'diff',
        '--name-only',
        '--diff-filter=ACMR',
        `${range.fallbackBase}...${range.head}`,
      ])
    )
  }

  if (!range.base || range.base === ZERO_SHA) {
    return splitLines(
      runGit(['diff-tree', '--no-commit-id', '--name-only', '-r', range.head])
    )
  }

  const separator =
    range.mode === 'pull_request' ? `${range.base}...${range.head}` : range.base
  const args =
    range.mode === 'pull_request'
      ? ['diff', '--name-only', '--diff-filter=ACMR', separator]
      : ['diff', '--name-only', '--diff-filter=ACMR', separator, range.head]

  return splitLines(runGit(args))
}

function shouldEvaluateAllFilters(range, changedFiles) {
  return range.forceAll || changedFiles.includes(FULL_CHANGE_SENTINEL)
}

function readEventPayload() {
  const eventPath = process.env.GITHUB_EVENT_PATH
  if (!eventPath) return {}
  return JSON.parse(readFileSync(eventPath, 'utf8'))
}

function writeOutputs(outputs) {
  const lines = Object.entries(outputs).map(([key, value]) => `${key}=${value}`)
  process.stdout.write(`${lines.join('\n')}\n`)

  if (process.env.GITHUB_OUTPUT) {
    appendFileSync(process.env.GITHUB_OUTPUT, `${lines.join('\n')}\n`)
  }
}

async function main() {
  const filterNames = process.argv.slice(2)
  if (filterNames.length === 0) {
    throw new Error(
      `Usage: node tools/ci/detect-changes.mjs <full|${Object.keys(FILTERS).join('|')}>...`
    )
  }

  const eventName = process.env.GITHUB_EVENT_NAME ?? 'push'
  const payload = readEventPayload()
  const ranges = resolveComparisonRanges(
    eventName,
    payload,
    process.env.GITHUB_SHA
  )
  const changedFiles = changedFilesForRange(ranges.current)
  const currentForceAll = shouldEvaluateAllFilters(ranges.current, changedFiles)
  const evaluations = currentForceAll
    ? Object.fromEntries(filterNames.map((name) => [name, true]))
    : evaluateFilters(changedFiles, filterNames)
  const prChangedFiles = ranges.pullRequest
    ? changedFilesForRange(ranges.pullRequest)
    : changedFiles
  const prForceAll = ranges.pullRequest
    ? shouldEvaluateAllFilters(ranges.pullRequest, prChangedFiles)
    : currentForceAll
  const prEvaluations =
    currentForceAll || prForceAll
      ? Object.fromEntries(filterNames.map((name) => [name, true]))
      : evaluateFilters(prChangedFiles, filterNames)

  writeOutputs({
    ...Object.fromEntries(
      Object.entries(evaluations).map(([key, value]) => [
        key,
        value ? 'true' : 'false',
      ])
    ),
    ...Object.fromEntries(
      Object.entries(prEvaluations).map(([key, value]) => [
        `pr_${key}`,
        value ? 'true' : 'false',
      ])
    ),
    change_mode: ranges.current.mode,
    changed_count: String(changedFiles.length),
    pr_changed_count: String(prChangedFiles.length),
  })

  if (changedFiles.length > 0) {
    process.stdout.write('Changed files considered by CI:\n')
    for (const file of changedFiles) process.stdout.write(`- ${file}\n`)
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await main()
}
