import { useRef, useCallback } from 'react';

import { useConduit } from './hooks/useConduit.js';
import { Terminal }   from './components/Terminal.jsx';
import { TopBar }     from './components/TopBar.jsx';
import { ConnectOverlay } from './components/ConnectOverlay.jsx';

export default function App() {
  // Imperative handle into the xterm instance.
  const termRef = useRef(null);

  const {
    phase,
    sessionId,
    error,
    PHASES,
    connect,
    disconnect,
    sendKeystroke,
  } = useConduit({
    // Called before SYNC so the PTY is sized to match the visible terminal.
    getTerminalDims: () => ({
      rows: termRef.current?.rows ?? 24,
      cols: termRef.current?.cols ?? 80,
    }),
    onOutput: useCallback((data) => {
      termRef.current?.write(data);
    }, []),
  });

  const isConnected = phase === PHASES.CONNECTED;
  const showOverlay = !isConnected;

  // Route keystrokes from xterm → WS.
  const handleData = useCallback((data) => {
    if (isConnected && sessionId) {
      sendKeystroke(data, sessionId);
    }
  }, [isConnected, sessionId, sendKeystroke]);

  return (
    <div className="app">
      <TopBar
        phase={phase}
        sessionId={sessionId}
        onDisconnect={disconnect}
      />

      <div className="terminal-wrapper">
        {/* Terminal is always mounted so we can measure its dimensions
            before connecting. The overlay sits on top when not connected. */}
        <Terminal
          ref={termRef}
          onData={handleData}
          active={isConnected}
        />

        {showOverlay && (
          <ConnectOverlay
            phase={phase}
            lastSessionId={sessionId}
            error={error}
            onConnect={connect}
          />
        )}
      </div>
    </div>
  );
}
