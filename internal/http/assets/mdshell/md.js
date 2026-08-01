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

  // diff2html needs highlight.js present BEFORE it draws, and its own
  // stylesheet plus the light/dark hljs themes the diff shell uses.
  var diffLibs = null;
  function loadDiff2Html() {
    if (diffLibs) return diffLibs;
    loadStyle("/_hostthis/diff2html.min.css");
    loadStyle("/_hostthis/hljs-light.css", "(prefers-color-scheme: light)");
    loadStyle("/_hostthis/hljs-dark.css", "(prefers-color-scheme: dark)");
    diffLibs = loadScript("/_hostthis/highlight.min.js")
      .then(function () { return loadScript("/_hostthis/diff2html-ui-base.min.js"); });
    return diffLibs;
  }

  async function renderDiffBlocks(blocks) {
    try {
      await loadDiff2Html();
    } catch (e) {
      return; // the source stays visible in its <pre>
    }
    blocks.forEach(function (b) {
      try {
        var ui = new Diff2HtmlUI(b.el, b.src, {
          // No file list for an inline block: a fenced diff is one hunk in
          // the middle of prose, not a changeset to navigate.
          drawFileList: false,
          matching: "lines",
          outputFormat: "line-by-line",
          highlight: true,
          colorScheme: "auto",
        }, window.hljs);
        ui.draw();
        ui.highlightCode();
      } catch (err) {
        b.el.textContent = b.src;
        b.el.className = "diff-block diff-err";
      }
    });
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

    // Same treatment for a fenced ```diff block: the rich viewer, not a grey
    // code box. A document whose FIRST fence is a diff is detected as the diff
    // kind outright; this covers a diff quoted inside prose.
    var diffs = [];
    content.querySelectorAll("pre > code.language-diff, pre > code.language-patch").forEach(function (code) {
      var holder = document.createElement("div");
      holder.className = "diff-block";
      code.parentElement.replaceWith(holder);
      diffs.push({ el: holder, src: code.textContent });
    });
    if (diffs.length) renderDiffBlocks(diffs);

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
