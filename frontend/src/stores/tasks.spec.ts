import { describe, expect, it } from 'vitest'
import { taskInTab, type TaskRunView } from './tasks'

function task(kind: TaskRunView['kind'], state: TaskRunView['state']): TaskRunView {
  return { kind, state } as TaskRunView
}

describe('taskInTab', () => {
  it('shows a canceled cache session under done, not failed', () => {
    const canceled = task('cache', 'canceled')
    expect(taskInTab(canceled, 'done')).toBe(true)
    expect(taskInTab(canceled, 'failed')).toBe(false)
  })

  it('keeps a canceled request in the failed tab', () => {
    const canceled = task('request', 'canceled')
    expect(taskInTab(canceled, 'failed')).toBe(true)
    expect(taskInTab(canceled, 'done')).toBe(false)
  })

  it('keeps real cache failures in the failed tab', () => {
    const failed = task('cache', 'failed')
    expect(taskInTab(failed, 'failed')).toBe(true)
    expect(taskInTab(failed, 'done')).toBe(false)
  })

  it('keeps done and active rows where they belong', () => {
    expect(taskInTab(task('cache', 'done'), 'done')).toBe(true)
    expect(taskInTab(task('cache', 'active'), 'active')).toBe(true)
    expect(taskInTab(task('cache', 'done'), 'failed')).toBe(false)
  })
})
