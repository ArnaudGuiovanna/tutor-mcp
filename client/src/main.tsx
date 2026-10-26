import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
// Self-hosted fonts: the Tauri CSP only allows 'self', so Google Fonts cannot be used.
import '@fontsource-variable/inter/opsz.css';
import '@fontsource-variable/jetbrains-mono';
import './styles.css';

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
