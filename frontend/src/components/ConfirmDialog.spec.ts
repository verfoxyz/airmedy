import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ConfirmDialog from './ConfirmDialog.vue'

// The Cancel/Confirm handling order is a contract, not an implementation
// detail: a parent that clears its pending target on update:open must still see
// that target when it handles confirm. Getting this backwards silently skipped
// the deletion entirely (lx source removal), and the page test could not see it
// because it stubbed this component out.
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

function mountDialog(props: Record<string, unknown> = {}) {
  return mount(ConfirmDialog, {
    props: { open: true, title: 'title', message: 'message', ...props },
    global: {
      stubs: {
        // The Modal primitive teleports to body; the dialog's own logic does not
        // depend on it, so render the slots inline.
        Modal: {
          props: ['open', 'title'],
          template: '<div v-if="open"><slot /><slot name="footer" /></div>',
        },
      },
    },
  })
}

function buttonLabels(wrapper: ReturnType<typeof mountDialog>): string[] {
  return wrapper.findAll('button').map(button => button.text())
}

describe('ConfirmDialog', () => {
  it('emits confirm before update:open', async () => {
    const wrapper = mountDialog()
    const order: string[] = []
    wrapper.vm.$.vnode.props!['onConfirm'] = () => order.push('confirm')
    wrapper.vm.$.vnode.props!['onUpdate:open'] = () => order.push('update:open')

    await wrapper.find('button:last-child').trigger('click')

    expect(order).toEqual(['confirm', 'update:open'])
  })

  it('emits cancel before update:open', async () => {
    const wrapper = mountDialog()
    const order: string[] = []
    wrapper.vm.$.vnode.props!['onCancel'] = () => order.push('cancel')
    wrapper.vm.$.vnode.props!['onUpdate:open'] = () => order.push('update:open')

    await wrapper.find('button:first-child').trigger('click')

    expect(order).toEqual(['cancel', 'update:open'])
  })

  it('keeps the confirm payload handoff working when the parent clears on close', async () => {
    // The exact shape LxSourcesSettings uses: a pending target that update:open
    // clears. The confirm handler must still observe it.
    let pending: string | null = 'source-1'
    const seen: (string | null)[] = []

    const wrapper = mount(ConfirmDialog, {
      props: {
        open: true,
        title: 't',
        message: 'm',
        'onUpdate:open': () => {
          pending = null
        },
        onConfirm: () => {
          seen.push(pending)
        },
      },
      global: {
        stubs: {
          Modal: {
            props: ['open', 'title'],
            template: '<div v-if="open"><slot /><slot name="footer" /></div>',
          },
        },
      },
    })

    await wrapper.find('button:last-child').trigger('click')

    expect(seen).toEqual(['source-1'])
    expect(pending).toBeNull()
  })

  it('does not emit anything while closed', () => {
    const wrapper = mountDialog({ open: false })
    expect(wrapper.findAll('button')).toHaveLength(0)
  })

  it('uses the provided labels over the defaults', () => {
    const wrapper = mountDialog({ confirmLabel: 'Remove', cancelLabel: 'Keep' })
    expect(buttonLabels(wrapper)).toEqual(['Keep', 'Remove'])
  })
})
