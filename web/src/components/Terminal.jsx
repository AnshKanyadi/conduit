import { useEffect, useRef, useImperativeHandle, forwardRef } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import '@xterm/xterm/css/xterm.css';

/**
 * Xterm.js colour theme — Apple Pro aesthetic.
 *
 * ANSI colours mapped to Apple's semantic colour palette so that typical
 * shell output (git diff, ls --color, etc.) looks intentional rather than
 * garish. Backgrounds stay pure black; foreground is a calm grey-white.
 */
const XTERM_THEME = {
  background:    '#000000',
  foreground:    '#c7c7c7',
  cursor:        '#c7c7c7',
  cursorAccent:  '#000000',
  selectionBackground: 'rgba(255, 255, 255, 0.12)',

  // Normal ANSI colours
  black:         '#1c1c1e',  // dark gray (not pure black so it's visible)
  red:           '#ff453a',  // Apple red
  green:         '#30d158',  // Apple green
  yellow:        '#ffd60a',  // Apple yellow
  blue:          '#0a84ff',  // Apple blue
  magenta:       '#bf5af2',  // Apple purple
  cyan:          '#5ac8fa',  // Apple light blue
  white:         '#c7c7c7',  // terminal foreground

  // Bright ANSI colours (bold text, etc.)
  brightBlack:   '#636366',  // Apple gray 6
  brightRed:     '#ff6961',
  brightGreen:   '#34c759',
  brightYellow:  '#ffd60a',
  brightBlue:    '#409cff',
  brightMagenta: '#da8fff',
  brightCyan:    '#70d7ff',
  brightWhite:   '#f5f5f7',  // Apple near-white
};

/**
 * Terminal — wraps xterm.js and exposes an imperative handle.
 *
 * The terminal is always mounted in the DOM (even when the ConnectOverlay
 * is visible above it) so that fitAddon can measure character dimensions
 * before a connection is made. This lets us send accurate rows/cols in the
 * SYNC frame for new sessions.
 *
 * Imperative handle:
 *   ref.current.rows      → current row count
 *   ref.current.cols      → current column count
 *   ref.current.write(u8) → write Uint8Array to terminal
 *   ref.current.focus()   → focus the xterm element
 */
const Terminal = forwardRef(function Terminal({ onData, active }, ref) {
  const containerRef = useRef(null);
  const termRef      = useRef(null);
  const fitRef       = useRef(null);
  const onDataRef    = useRef(onData);

  // Keep onData current without re-running the setup effect.
  useEffect(() => { onDataRef.current = onData; }, [onData]);

  // Expose imperative API to parent.
  useImperativeHandle(ref, () => ({
    get rows() { return termRef.current?.rows ?? 24; },
    get cols() { return termRef.current?.cols ?? 80; },
    write(data) { termRef.current?.write(data); },
    focus() { termRef.current?.focus(); },
    clear() { termRef.current?.clear(); },
  }), []);

  // Mount xterm once. Dispose on unmount.
  useEffect(() => {
    const term = new XTerm({
      theme:            XTERM_THEME,
      fontFamily:       '"SF Mono", "JetBrains Mono", "Fira Code", "Cascadia Code", Menlo, monospace',
      fontSize:         13,
      lineHeight:       1.25,
      letterSpacing:    0,
      cursorBlink:      true,
      cursorStyle:      'block',
      allowTransparency: false,
      scrollback:       5000,
      convertEol:       false,
      // Smooth scrolling
      smoothScrollDuration: 80,
    });

    const fitAddon   = new FitAddon();
    const linksAddon = new WebLinksAddon();

    term.loadAddon(fitAddon);
    term.loadAddon(linksAddon);
    term.open(containerRef.current);
    fitAddon.fit();

    termRef.current = term;
    fitRef.current  = fitAddon;

    // Route user keystrokes to the WS via onData.
    const disposeData = term.onData((data) => {
      onDataRef.current?.(data);
    });

    // Resize observer keeps the terminal sized to its container.
    const ro = new ResizeObserver(() => {
      // rAF avoids layout thrashing when the resize fires mid-paint.
      requestAnimationFrame(() => fitAddon.fit());
    });
    ro.observe(containerRef.current);

    return () => {
      ro.disconnect();
      disposeData.dispose();
      term.dispose();
      termRef.current = null;
      fitRef.current  = null;
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Focus the terminal whenever it becomes active.
  useEffect(() => {
    if (active) termRef.current?.focus();
  }, [active]);

  return (
    <div
      ref={containerRef}
      className="terminal-container"
      style={{
        // Slightly dim the terminal under the overlay so it doesn't distract.
        opacity:    active ? 1 : 0.04,
        transition: 'opacity 0.25s ease',
      }}
    />
  );
});

export { Terminal };
