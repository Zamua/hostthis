// Renders a bare Mermaid paste. The source arrives as raw bytes; mermaid turns
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

    var dark = matchMedia("(prefers-color-scheme: dark)").matches;
    mermaid.initialize({
      startOnLoad: false,
      theme: dark ? "dark" : "default",
      // Mermaid's own sanitizer. The source is untrusted, and the diagram is
      // rendered into this same origin.
      securityLevel: "strict",
    });

    var out = await mermaid.render("diagram", src);
    content.innerHTML = out.svg;

    var title = src.split("\n").find(function (l) { return l.trim(); });
    if (title) document.title = title.trim().slice(0, 80);
  } catch (e) {
    fail("Diagram failed to render.\n\n" + (e && e.message ? e.message : String(e)));
  }
})();
