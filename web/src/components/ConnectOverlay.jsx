import { useState, useEffect, useCallback } from 'react';

/**
 * ConnectOverlay — shown when the terminal is not yet connected.
 *
 * Handles three distinct sub-states:
 *   - idle:         initial visit, "New session" as primary CTA
 *   - disconnected: lost connection; offer to rejoin last session
 *   - connecting/handshaking: loading state, disable inputs
 */
export function ConnectOverlay({ phase, lastSessionId, error, onConnect }) {
  const [input, setInput] = useState('');
  const isLoading = phase === 'connecting' || phase === 'handshaking';

  // Pre-fill session ID when reconnecting.
  useEffect(() => {
    if (phase === 'disconnected' && lastSessionId) {
      setInput(String(lastSessionId));
    }
  }, [phase, lastSessionId]);

  const handleNew = useCallback(() => {
    onConnect(0);
  }, [onConnect]);

  const handleRejoin = useCallback(() => {
    const id = parseInt(input.replace(/\D/g, ''), 10);
    if (id > 0) onConnect(id);
  }, [input, onConnect]);

  const handleKeyDown = useCallback((e) => {
    if (e.key === 'Enter') {
      const id = parseInt(input.replace(/\D/g, ''), 10);
      if (id > 0) handleRejoin();
      else handleNew();
    }
  }, [input, handleNew, handleRejoin]);

  const parsedId = parseInt(input.replace(/\D/g, ''), 10);
  const canRejoin = parsedId > 0 && !isLoading;

  const isDisconnected = phase === 'disconnected';

  return (
    <div className="overlay" role="dialog" aria-label="Connect to Conduit">
      <div className="overlay-content">

        {/* Wordmark */}
        <div style={{ textAlign: 'center' }}>
          <h1 className="overlay-wordmark">conduit</h1>
          <p className="overlay-tagline">distributed terminal sessions</p>
        </div>

        {/* If disconnected, show the last session as a pill */}
        {isDisconnected && lastSessionId && (
          <div className="overlay-session-hint">
            <span className="overlay-session-hint-label">last session</span>
            <span className="overlay-session-hint-id">#{lastSessionId}</span>
          </div>
        )}

        {/* Form */}
        <div className="overlay-form">
          <input
            className="overlay-input"
            type="text"
            placeholder="Session ID  —  leave empty for new"
            value={input}
            onChange={e => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={isLoading}
            autoFocus
            aria-label="Session ID to rejoin"
            spellCheck={false}
            autoComplete="off"
          />

          <div className="overlay-actions">
            {canRejoin ? (
              <>
                <button
                  className="btn btn--primary"
                  onClick={handleRejoin}
                  disabled={isLoading}
                >
                  {isLoading ? 'Connecting…' : `Rejoin #${parsedId}`}
                </button>
                <button
                  className="btn btn--secondary"
                  onClick={handleNew}
                  disabled={isLoading}
                >
                  New session instead
                </button>
              </>
            ) : (
              <button
                className="btn btn--primary"
                onClick={handleNew}
                disabled={isLoading}
              >
                {isLoading
                  ? (phase === 'handshaking' ? 'Authenticating…' : 'Connecting…')
                  : (isDisconnected ? 'New session' : 'New session')}
              </button>
            )}
          </div>

          {/* Error / status message */}
          {error && (
            <p className="overlay-message overlay-message--error" role="alert">
              {error}
            </p>
          )}

          {!error && isDisconnected && (
            <p className="overlay-message overlay-message--info">
              Session disconnected. Start a new one or paste an ID to rejoin.
            </p>
          )}

          {!error && !isDisconnected && !isLoading && (
            <p className="overlay-message overlay-message--info">
              Press ↵ to connect
            </p>
          )}
        </div>

        {/* Keyboard hint */}
        {!isLoading && (
          <p style={{ fontSize: '11px', color: 'var(--text-tertiary)', textAlign: 'center' }}>
            Share a session ID to collaborate in the same terminal
          </p>
        )}
      </div>
    </div>
  );
}
