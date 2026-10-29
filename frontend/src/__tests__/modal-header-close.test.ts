/**
 * Tests for the dialog header close-X (modal.ts: modalCloseButtonHtml,
 * createModalCloseButton, createModalHeader and the delegated
 * `data-modal-dismiss` click handler that openModal installs).
 *
 * The X stands in for the dialog's own Cancel/Close button: with a target
 * id it clicks that button, so the dialog's own close path (and its
 * guards) runs; with no target it closes through closeModal, the Escape
 * path, which hides the dialog and runs onClose.
 */

import { openModal, closeModal, modalCloseButtonHtml, createModalCloseButton, createModalHeader } from '../modal';

/** Parse markup without an innerHTML write (the security hook flags those, even in tests). */
function fromHtml(html: string): HTMLElement {
  const el = new DOMParser().parseFromString(html, 'text/html').body.firstElementChild;
  if (!el) throw new Error('no element parsed');
  return document.adoptNode(el) as HTMLElement;
}

interface Dialog {
  modal: HTMLDivElement;
  trigger: HTMLButtonElement;
  header: HTMLDivElement;
  field: HTMLInputElement;
  footer: HTMLDivElement;
}

/** A framed dialog: trigger outside it, header (title + the given X), a field, a footer. */
function buildDialog(closeX: HTMLElement): Dialog {
  const trigger = document.createElement('button');
  trigger.id = 'trigger';
  trigger.textContent = 'Open';
  document.body.appendChild(trigger);

  const modal = document.createElement('div');
  modal.className = 'modal hidden';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  const content = document.createElement('div');
  content.className = 'modal-content';

  const header = document.createElement('div');
  header.className = 'modal-header';
  const h2 = document.createElement('h2');
  h2.textContent = 'Dialog';
  header.append(h2, closeX);

  const field = document.createElement('input');
  field.id = 'field';

  const footer = document.createElement('div');
  footer.className = 'modal-buttons';

  content.append(header, field, footer);
  modal.appendChild(content);
  document.body.appendChild(modal);
  return { modal, trigger, header, field, footer };
}

function addFooterButton(d: Dialog, id: string, text: string): HTMLButtonElement {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.id = id;
  btn.textContent = text;
  d.footer.appendChild(btn);
  return btn;
}

function closeXOf(d: Dialog): HTMLButtonElement {
  return d.header.querySelector<HTMLButtonElement>('button.modal-close')!;
}

afterEach(() => {
  document.body.replaceChildren();
});

describe('dialog header close-X', () => {
  describe('with a target button (modalCloseButtonHtml(id))', () => {
    test('marks the X with the target id and the "Close" label', () => {
      const x = fromHtml(modalCloseButtonHtml('cancel-btn'));
      expect(x.getAttribute('data-modal-dismiss')).toBe('cancel-btn');
      expect(x.getAttribute('aria-label')).toBe('Close');
      expect(x.getAttribute('type')).toBe('button');
    });

    test('clicking the X runs the target button\'s click handler, and only that', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn();
      cancel.addEventListener('click', onCancel);
      const onClose = jest.fn();
      openModal(d.modal, { onClose });

      closeXOf(d).click();

      expect(onCancel).toHaveBeenCalledTimes(1);
      // The X does not close the dialog by itself: closing is the target's job.
      expect(d.modal.classList.contains('hidden')).toBe(false);
      expect(onClose).not.toHaveBeenCalled();
    });

    test('when the target closes the dialog, the X closes it through the same path', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      cancel.addEventListener('click', () => closeModal(d.modal));
      const onClose = jest.fn();
      d.trigger.focus();
      openModal(d.modal, { onClose });

      closeXOf(d).click();

      expect(d.modal.classList.contains('hidden')).toBe(true);
      expect(onClose).toHaveBeenCalledTimes(1);
      expect(document.activeElement).toBe(d.trigger);
    });

    test('a disabled target (e.g. Cancel locked during a save) keeps the dialog open', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn(() => closeModal(d.modal));
      cancel.addEventListener('click', onCancel);
      const onClose = jest.fn();
      openModal(d.modal, { onClose });
      cancel.disabled = true;

      closeXOf(d).click();

      expect(onCancel).not.toHaveBeenCalled();
      expect(d.modal.classList.contains('hidden')).toBe(false);
      expect(onClose).not.toHaveBeenCalled();
    });

    test('the DOM-built X (createModalCloseButton(id)) behaves the same', () => {
      const d = buildDialog(createModalCloseButton('cancel-btn'));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn();
      cancel.addEventListener('click', onCancel);
      openModal(d.modal);

      closeXOf(d).click();

      expect(onCancel).toHaveBeenCalledTimes(1);
    });

    test('a click on the X icon (not the button itself) is still handled', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn();
      cancel.addEventListener('click', onCancel);
      openModal(d.modal);

      const icon = closeXOf(d).querySelector('svg')!;
      icon.dispatchEvent(new MouseEvent('click', { bubbles: true }));

      expect(onCancel).toHaveBeenCalledTimes(1);
    });
  });

  describe('without a target (modalCloseButtonHtml())', () => {
    test('clicking the X closes through closeModal: hidden, onClose, focus returned', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml()));
      expect(closeXOf(d).getAttribute('data-modal-dismiss')).toBe('');
      const onClose = jest.fn();
      d.trigger.focus();
      openModal(d.modal, { onClose });

      closeXOf(d).click();

      expect(d.modal.classList.contains('hidden')).toBe(true);
      expect(onClose).toHaveBeenCalledTimes(1);
      expect(document.activeElement).toBe(d.trigger);
    });

    test('a dialog that must be acknowledged (data-require-ack) is not closed by the X', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml()));
      d.modal.dataset.requireAck = 'true';
      const onClose = jest.fn();
      openModal(d.modal, { onClose });

      closeXOf(d).click();

      expect(d.modal.classList.contains('hidden')).toBe(false);
      expect(onClose).not.toHaveBeenCalled();
    });
  });

  describe('RI exchange header (createModalHeader with closeLabel "Close dialog")', () => {
    // riexchange.ts builds its header this way: no target, so the X takes the
    // closeModal path (which runs the dialog's onClose), and a distinct label
    // because a finished exchange relabels the footer Cancel to "Close".
    function buildRiExchangeDialog(): { d: Dialog; footerClose: HTMLButtonElement } {
      const h3 = document.createElement('h3');
      h3.textContent = 'RI Exchange';
      const header = createModalHeader(h3, { closeLabel: 'Close dialog' });
      const x = header.querySelector<HTMLButtonElement>('button.modal-close')!;
      const d = buildDialog(x);
      const footerClose = addFooterButton(d, 'ri-cancel', 'Close');
      footerClose.addEventListener('click', () => closeModal(d.modal));
      return { d, footerClose };
    }

    test('the X is labelled "Close dialog", distinct from the footer\'s "Close"', () => {
      const { d } = buildRiExchangeDialog();
      const named = Array.from(d.modal.querySelectorAll('button'))
        .map(b => b.getAttribute('aria-label') ?? b.textContent?.trim());
      expect(named).toEqual(['Close dialog', 'Close']);
      expect(closeXOf(d).getAttribute('data-modal-dismiss')).toBe('');
    });

    test('clicking the X closes the dialog and runs its onClose', () => {
      const { d } = buildRiExchangeDialog();
      const onClose = jest.fn();
      openModal(d.modal, { onClose });

      closeXOf(d).click();

      expect(d.modal.classList.contains('hidden')).toBe(true);
      expect(onClose).toHaveBeenCalledTimes(1);
    });
  });

  describe('createModalHeader', () => {
    test('closable: false (dialogs that must be acknowledged) renders no X', () => {
      const h2 = document.createElement('h2');
      const header = createModalHeader(h2, { closable: false });
      expect(header.className).toBe('modal-header');
      expect(header.querySelector('button')).toBeNull();
    });
  });

  describe('lifecycle', () => {
    test('initial focus skips the header X and lands on the first field', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml()));
      openModal(d.modal);
      expect(document.activeElement).toBe(d.field);
    });

    test('the click handler is removed on close: the X does nothing afterwards', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn();
      cancel.addEventListener('click', onCancel);
      const onClose = jest.fn();
      openModal(d.modal, { onClose });
      closeModal(d.modal);
      expect(onClose).toHaveBeenCalledTimes(1);

      d.modal.classList.remove('hidden');
      closeXOf(d).click();

      expect(onCancel).not.toHaveBeenCalled();
      expect(onClose).toHaveBeenCalledTimes(1);
    });

    test('re-opening does not stack click handlers', () => {
      const d = buildDialog(fromHtml(modalCloseButtonHtml('cancel-btn')));
      const cancel = addFooterButton(d, 'cancel-btn', 'Cancel');
      const onCancel = jest.fn();
      cancel.addEventListener('click', onCancel);
      openModal(d.modal);
      openModal(d.modal);

      closeXOf(d).click();

      expect(onCancel).toHaveBeenCalledTimes(1);
    });
  });
});
