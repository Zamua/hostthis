// Client-side markdown renderer bootstrap. Fetches the raw markdown
// bytes for this paste (?raw=1 forces the raw branch on the server),
// renders them with marked, sanitizes the result with DOMPurify, and
// drops the HTML into #content. No server-side render: the server only
// streamed us the fixed shell + the raw bytes.
(async function () {
  "use strict";
  var DL = window.HostthisDeepLink;
  var content = document.getElementById("content");

  // Fenced blocks are rendered by the same libraries the standalone kinds use,
  // fetched ONLY once a fence of that language is known to be present. Mermaid
  // alone is ~3.4 MB, and most documents contain neither kind of block.
  function loadScript(src) {
    return new Promise(function (resolve, reject) {
      var s = document.createElement("script");
      s.src = src;
      s.onload = resolve;
      s.onerror = function () { reject(new Error(src + " failed to load")); };
      document.head.appendChild(s);
    });
  }

  function loadStyle(href, media) {
    var l = document.createElement("link");
    l.rel = "stylesheet";
    l.href = href;
    if (media) l.media = media;
    document.head.appendChild(l);
  }

  function loadMermaid() {
    if (window.mermaid) return Promise.resolve(window.mermaid);
    return loadScript("/_hostthis/mermaid.min.js").then(function () { return window.mermaid; });
  }

  // tintDiff colours a fenced diff line by line. textContent is read and
  // written per line, so the user's bytes are never interpreted as markup.
  function tintDiff(code) {
    var lines = code.textContent.replace(/\n$/, "").split("\n");
    code.textContent = "";
    lines.forEach(function (line) {
      var cls = "d-line";
      if (line.startsWith("+++") || line.startsWith("---")) cls += " d-file";
      else if (line.startsWith("@@")) cls += " d-hunk";
      else if (line.startsWith("+")) cls += " d-ins";
      else if (line.startsWith("-")) cls += " d-del";
      var span = document.createElement("span");
      span.className = cls;
      // The line's own newline is NOT re-added: every line is a block, so the
      // break comes from layout. Adding one too would double-space the block,
      // and copying still yields newlines because block elements produce them.
      span.textContent = line;
      code.appendChild(span);
    });
    code.parentElement.classList.add("diff-code");
  }

  async function renderMermaidBlocks(blocks) {
    var mermaid;
    try {
      mermaid = await loadMermaid();
    } catch (e) {
      blocks.forEach(function (b) { b.el.textContent = b.src; });
      return;
    }
    mermaid.initialize({
      startOnLoad: false,
      theme: matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "default",
      // The source is untrusted and renders into this paste's own origin.
      securityLevel: "strict",
    });
    for (var i = 0; i < blocks.length; i++) {
      try {
        var out = await mermaid.render("mmd-" + i, blocks[i].src);
        blocks[i].el.innerHTML = out.svg;
      } catch (e) {
        // A broken diagram keeps its source visible rather than vanishing.
        blocks[i].el.textContent = blocks[i].src;
        blocks[i].el.className = "mermaid-block mermaid-err";
      }
    }
  }

  // Heading anchors, added here because marked no longer emits ids: without
  // them a #heading fragment has nothing to resolve against.
  function addHeadingIds(root) {
    var used = Object.create(null);
    root.querySelectorAll("h1,h2,h3,h4,h5,h6").forEach(function (h) {
      if (h.id) return;
      var base = DL.slug(h.textContent) || "section";
      var id = base, n = 1;
      while (used[id]) id = base + "-" + n++;
      used[id] = true;
      h.id = id;
      h.classList.add("anchored");
      h.title = "link to this section";
      h.addEventListener("click", function () {
        DL.setHash(id);
        DL.reveal(h);
      });
    });
  }

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

    addHeadingIds(content);

    // marked renders a fenced ```mermaid block as <pre><code class="language-mermaid">.
    var blocks = [];
    content.querySelectorAll("pre > code.language-mermaid").forEach(function (code) {
      var holder = document.createElement("div");
      holder.className = "mermaid-block";
      code.parentElement.replaceWith(holder);
      blocks.push({ el: holder, src: code.textContent });
    });
    if (blocks.length) renderMermaidBlocks(blocks);

    // A ```diff fence is a LANGUAGE TAG, the same as ```java: it asks for
    // highlighting inside a code block, not for the standalone diff viewer's
    // chrome. Tinting by leading character is the whole grammar, so it needs
    // no highlighter library.
    content.querySelectorAll("pre > code.language-diff, pre > code.language-patch")
      .forEach(tintDiff);

    // Resolve the fragment only now. The browser already tried at parse time,
    // when #content was empty, so without this a deep link lands at the top.
    DL.onResolve(function (t) {
      if (t.type !== "id") return;
      var el = document.getElementById(t.id);
      if (el) DL.reveal(el);
    });
  } catch (e) {
    content.textContent = "Failed to load paste.";
  }
})();
