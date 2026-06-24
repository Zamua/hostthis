// Client-side markdown renderer bootstrap. Fetches the raw markdown
// bytes for this paste (?raw=1 forces the raw branch on the server),
// renders them with marked, sanitizes the result with DOMPurify, and
// drops the HTML into #content. No server-side render: the server only
// streamed us the fixed shell + the raw bytes.
(async function () {
  var content = document.getElementById("content");
  try {
    var resp = await fetch(location.pathname + "?raw=1");
    if (!resp.ok) {
      content.textContent = "Failed to load paste.";
      return;
    }
    var md = await resp.text();
    var html = DOMPurify.sanitize(marked.parse(md));
    content.innerHTML = html;
    // Use the first <h1> as the document title, mirroring the old
    // server-side title extraction. Leave the generic title if none.
    var h1 = content.querySelector("h1");
    if (h1 && h1.textContent.trim()) {
      document.title = h1.textContent.trim();
    }
  } catch (e) {
    content.textContent = "Failed to load paste.";
  }
})();
