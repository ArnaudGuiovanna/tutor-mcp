'use client';

import { useState } from 'react';

export function CodeBlock({ code, label = 'Example' }: { code: string; label?: string }) {
  const [copied, setCopied] = useState<string | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  const status = copied === code ? 'Copied' : failed === code ? 'Select and copy the code below' : 'Copy';

  async function copy() {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(code);
      setFailed(null);
    } catch {
      setFailed(code);
    }
  }

  return (
    <div className="code-block">
      <div className="code-heading"><span>{label}</span><button type="button" onClick={copy} aria-label={`Copy ${label}`} aria-live="polite">{status}</button></div>
      <pre tabIndex={0}><code>{code}</code></pre>
    </div>
  );
}
