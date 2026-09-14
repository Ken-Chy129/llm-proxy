import React from 'react';

export default function App() {
  return (
    <main style={{ maxWidth: 720, margin: '64px auto', padding: 24, fontFamily: 'system-ui' }}>
      <h1>LLM Proxy</h1>
      <p>Private, non-streaming OpenAI-compatible API.</p>
      <pre>POST /api/v1/chat/completions{'\n'}GET  /api/v1/models</pre>
      <p>Use the publication Base URL and your authorized AnyGen platform key.</p>
      <p>Credentials are never displayed here. The provision command verifies the API directly.</p>
    </main>
  );
}
