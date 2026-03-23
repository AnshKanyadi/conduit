import { useState, useCallback } from 'react';

const STATUS_LABELS = {
  idle:         'Ready',
  connecting:   'Connecting',
  handshaking:  'Authenticating',
  connected:    'Connected',
  disconnected: 'Disconnected',
};

export function TopBar({ phase, sessionId, onDisconnect }) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!sessionId) return;
    navigator.clipboard.writeText(String(sessionId)).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [sessionId]);

  return (
    <header className="topbar">
      <div className="topbar-left">
        <span className="topbar-wordmark">conduit</span>
      </div>

      <div className="topbar-right">
        {sessionId && (
          <button
            className="session-badge"
            onClick={handleCopy}
            title="Click to copy session ID"
            aria-label={`Session ${sessionId} — click to copy`}
          >
            <span className="session-badge-label">session</span>
            <span className="session-badge-id">#{sessionId}</span>
            <span className="session-badge-copy">{copied ? '✓' : '⌘C'}</span>
          </button>
        )}

        <div className="status-indicator">
          <span className={`status-dot status-dot--${phase}`} />
          <span className="status-label">{STATUS_LABELS[phase] ?? phase}</span>
        </div>

        {phase === 'connected' && (
          <button
            className="btn btn--secondary"
            style={{ width: 'auto', padding: '4px 10px', fontSize: '11px' }}
            onClick={onDisconnect}
            title="Disconnect and return to start"
          >
            Disconnect
          </button>
        )}
      </div>

      {/* Toast for copy confirmation */}
      {copied && (
        <div className="copied-toast" aria-live="polite">
          Session ID copied
        </div>
      )}
    </header>
  );
}
