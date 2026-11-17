import { describe, expect, it, afterEach } from 'vitest';
import { render } from '@testing-library/react';
import { useRef, useState } from 'react';
import { useFitToViewport } from './useFitToViewport';

function rect(r: { top: number; left: number; width: number; height: number }): DOMRect {
  return { ...r, right: r.left + r.width, bottom: r.top + r.height, x: r.left, y: r.top, toJSON: () => r } as DOMRect;
}

function Harness({ box }: { box: { top: number; left: number; width: number; height: number } }) {
  const ref = useRef<HTMLDivElement>(null);
  const [open] = useState(true);
  useFitToViewport(ref, open);
  return (
    <div
      data-testid="menu"
      ref={(el) => {
        if (el) el.getBoundingClientRect = () => rect(box);
        (ref as { current: HTMLDivElement | null }).current = el;
      }}
    />
  );
}

const innerHeight = window.innerHeight;
afterEach(() => {
  Object.defineProperty(window, 'innerHeight', { value: innerHeight, configurable: true });
});

describe('useFitToViewport', () => {
  it('caps a menu that runs past the bottom of the window and lets it scroll', () => {
    Object.defineProperty(window, 'innerHeight', { value: 600, configurable: true });
    const { getByTestId } = render(<Harness box={{ top: 250, left: 300, width: 220, height: 520 }} />);
    const menu = getByTestId('menu');
    expect(menu.style.maxHeight).toBe('338px'); // 600 - 250 - 12
    expect(menu.style.overflowY).toBe('auto');
  });

  it('leaves a menu that fits alone', () => {
    Object.defineProperty(window, 'innerHeight', { value: 900, configurable: true });
    const { getByTestId } = render(<Harness box={{ top: 250, left: 300, width: 220, height: 520 }} />);
    expect(getByTestId('menu').style.maxHeight).toBe('');
    expect(getByTestId('menu').style.translate).toBe('');
  });

  it('shifts a menu back inside the left edge', () => {
    const { getByTestId } = render(<Harness box={{ top: 10, left: -40, width: 220, height: 100 }} />);
    expect(getByTestId('menu').style.translate).toBe('52px 0');
  });
});
