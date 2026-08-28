import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  evaluateFilters,
  isDocsFile,
  matchesPattern,
  resolveComparisonRanges,
  resolveRange,
} from '../../tools/ci/detect-changes.mjs'

test('docs-only changes do not select full CI', () => {
  assert.deepEqual(evaluateFilters(['docs/README.md'], ['full', 'docs']), {
    full: false,
    docs: true,
  })
})

test('live generation rollout records select full CI canary', () => {
  assert.deepEqual(
    evaluateFilters(
      ['runbooks/GENERATION-CLEAN-BOUNDARY-ROLLOUT-2026-08-05.md'],
      ['full', 'docs']
    ),
    {
      full: true,
      docs: true,
    }
  )
})

test('runtime changes select full CI', () => {
  assert.deepEqual(
    evaluateFilters(['new-runtime-surface/config.toml'], ['full', 'docs']),
    {
      full: true,
      docs: false,
    }
  )
})

test('workflow changes select full CI by default', () => {
  assert.deepEqual(evaluateFilters(['.github/workflows/ci.yml'], ['full']), {
    full: true,
  })
})

test('pull request synchronize uses current and full PR ranges', () => {
  const payload = {
    action: 'synchronize',
    before: '1111111111111111111111111111111111111111',
    pull_request: {
      base: { sha: '2222222222222222222222222222222222222222' },
      head: { sha: '3333333333333333333333333333333333333333' },
    },
  }

  assert.deepEqual(resolveRange('pull_request', payload, ''), {
    base: '1111111111111111111111111111111111111111',
    fallbackBase: '2222222222222222222222222222222222222222',
    head: '3333333333333333333333333333333333333333',
    mode: 'incremental',
  })
  assert.deepEqual(resolveComparisonRanges('pull_request', payload, ''), {
    current: {
      base: '1111111111111111111111111111111111111111',
      fallbackBase: '2222222222222222222222222222222222222222',
      head: '3333333333333333333333333333333333333333',
      mode: 'incremental',
    },
    pullRequest: {
      base: '2222222222222222222222222222222222222222',
      head: '3333333333333333333333333333333333333333',
      mode: 'pull_request',
    },
  })
})

test('path matcher handles recursive and extension patterns', () => {
  assert.equal(matchesPattern('docs/specs/SPEC.md', 'docs/**'), true)
  assert.equal(
    matchesPattern('templates/workflows/CI.md', 'templates/**/*.md'),
    true
  )
  assert.equal(matchesPattern('services/api/main.go', 'services/**'), true)
  assert.equal(matchesPattern('README.md', '**/*.md'), true)
  assert.equal(isDocsFile('src/app/page.tsx'), false)
})
