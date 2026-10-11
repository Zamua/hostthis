// The pending and failed pages, byte for byte as hostthisd serves them
// (internal/http statusPage). testdata/shells-golden.json pins both.

function statusPage(title, refresh, style, card) {
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
${refresh}<title>${title}</title>
<style>
  :root { color-scheme: light dark; }
  html, body { height: 100%; margin: 0; }
  body {
    display: flex; align-items: center; justify-content: center;
    font: 15px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    background: #0e0e10; color: #e6e6e6;
  }
${style}</style>
</head>
<body>
  <div class="card">
${card}  </div>
</body>
</html>
`;
}

// Refreshes itself every second until the paste is ready.
export const LOADING_PAGE = statusPage("preparing your paste",
  "<meta http-equiv=\"refresh\" content=\"1\">\n",
  `  .card { text-align: center; padding: 2rem; }
  .dot {
    display: inline-block; width: .6rem; height: .6rem; margin: 0 .15rem;
    border-radius: 50%; background: currentColor; opacity: .25;
    animation: pulse 1s infinite ease-in-out;
  }
  .dot:nth-child(2) { animation-delay: .15s; }
  .dot:nth-child(3) { animation-delay: .3s; }
  @keyframes pulse { 0%,100% { opacity: .25; } 50% { opacity: 1; } }
  .muted { color: #8a8a8a; margin-top: .75rem; font-size: 13px; }
`,
  `    <div><span class="dot"></span><span class="dot"></span><span class="dot"></span></div>
    <p>preparing your paste</p>
    <p class="muted">this page refreshes automatically</p>
`);

export const FAILED_PAGE = statusPage("paste unavailable", "",
  `  .card { text-align: center; padding: 2rem; max-width: 28rem; }
  h1 { font-size: 1.1rem; margin: 0 0 .5rem; }
  .muted { color: #8a8a8a; font-size: 13px; }
`,
  `    <h1>this paste could not be saved</h1>
    <p class="muted">the upload did not finish writing to storage. try uploading it again.</p>
`);
