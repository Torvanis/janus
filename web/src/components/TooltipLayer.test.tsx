import { describe, expect, it } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { IconButton } from './IconButton';
import { TOOLTIP_GAP, TOOLTIP_MARGIN, TooltipLayer, placeTooltip } from './TooltipLayer';

const viewport = { width: 1600, height: 900 };
const size = { width: 40, height: 24 };
const at = (top: number, right = 1530) => ({ top, bottom: top + 30, left: right - 30, right });

describe('placeTooltip', () => {
  it('puts the tooltip above the button, right-aligned, when there is room', () => {
    const p = placeTooltip(at(435), size, viewport);
    expect(p).toEqual({ placement: 'top', top: 435 - TOOLTIP_GAP - 24, left: 1530 - 40 });
  });

  it('flips below when the button is too close to the top of the viewport', () => {
    const p = placeTooltip(at(10), size, viewport);
    expect(p.placement).toBe('bottom');
    expect(p.top).toBe(10 + 30 + TOOLTIP_GAP);
  });

  it('stays above when neither side fits (pinned to the top edge)', () => {
    const p = placeTooltip(at(10), size, { width: 1600, height: 50 });
    expect(p.placement).toBe('top');
    expect(p.top).toBe(TOOLTIP_MARGIN);
  });

  it('never leaves the viewport sideways', () => {
    expect(placeTooltip(at(400, 20), { width: 200, height: 24 }, viewport).left).toBe(TOOLTIP_MARGIN);
    expect(placeTooltip(at(400, 1600), size, viewport).left).toBe(1600 - TOOLTIP_MARGIN - 40);
  });
});

describe('TooltipLayer', () => {
  it('shows an icon button tooltip in a body portal, outside the table, while hovered', () => {
    render(
      <>
        <table>
          <tbody>
            <tr>
              <td data-testid="cell">
                <IconButton icon="edit" label="Edit" onClick={() => {}} />
              </td>
            </tr>
          </tbody>
        </table>
        <TooltipLayer />
      </>,
    );
    const button = screen.getByRole('button', { name: 'Edit' });
    expect(document.querySelector('.janus-tooltip')).toBeNull();

    act(() => {
      fireEvent.pointerOver(button);
    });
    const tip = document.querySelector('.janus-tooltip');
    expect(tip?.textContent).toBe('Edit');
    // Rendered on <body>, so no sticky cell or header stacking context can cover it.
    expect(tip?.parentElement).toBe(document.body);
    expect(screen.getByTestId('cell').contains(tip)).toBe(false);
    // Not part of the accessible name: the button's aria-label already says it.
    expect(tip?.getAttribute('aria-hidden')).toBe('true');

    act(() => {
      fireEvent.pointerOut(button, { relatedTarget: document.body });
    });
    expect(document.querySelector('.janus-tooltip')).toBeNull();
  });

  it('follows a label change while shown and hides on Escape', async () => {
    const { rerender } = render(
      <>
        <IconButton icon="copy" label="Copy" onClick={() => {}} />
        <TooltipLayer />
      </>,
    );
    act(() => {
      fireEvent.pointerOver(screen.getByRole('button', { name: 'Copy' }));
    });
    expect(document.querySelector('.janus-tooltip')?.textContent).toBe('Copy');

    rerender(
      <>
        <IconButton icon="copy" label="Copied" onClick={() => {}} />
        <TooltipLayer />
      </>,
    );
    // The layer follows the attribute through a MutationObserver.
    await waitFor(() => expect(document.querySelector('.janus-tooltip')?.textContent).toBe('Copied'));

    act(() => {
      fireEvent.keyDown(document, { key: 'Escape' });
    });
    expect(document.querySelector('.janus-tooltip')).toBeNull();
  });
});
