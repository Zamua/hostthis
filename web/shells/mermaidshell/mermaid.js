// Renders a bare Mermaid paste. The source arrives as raw bytes; pele turns
// it into SVG in the browser, so the server never renders a diagram.
(async function () {
  "use strict";
  var content = document.getElementById("content");

  function fail(msg) {
    content.innerHTML = "";
    var pre = document.createElement("div");
    pre.className = "err";
    // textContent, not innerHTML: the message embeds the user's own source.
    pre.textContent = msg;
    content.appendChild(pre);
  }

  try {
    var resp = await fetch(location.pathname + "?raw=1");
    if (!resp.ok) return fail("Failed to load paste.");
    var src = await resp.text();

    content.innerHTML = "";
    // pele escapes diagram text and filters links and styles itself. mount()
    // measures text with the page's own font and fits the container width.
    var diagram = pele.mount(content, src, { idPrefix: "diagram-", onError: function () {} });
    if (diagram.error) {
      var e = diagram.error;
      diagram.destroy();
      return fail("Diagram failed to render.\n\n" + (e && e.message ? e.message : String(e)));
    }

    var title = src.split("\n").find(function (l) { return l.trim(); });
    if (title) document.title = title.trim().slice(0, 80);
  } catch (e) {
    fail("Diagram failed to render.\n\n" + (e && e.message ? e.message : String(e)));
  }
})();
