// Knowledge base shell: one page that answers the root, every markdown path
// and a route-shaped miss, and renders whichever document the URL names.
//
// The page is content-independent (its ETag is the shell version), so
// everything about this base is discovered at runtime: the file list comes from
// "?files=1" and each document's bytes from "?raw=1", the two contracts the
// server offers. Nothing is templated into the page.
(function () {
  "use strict";

  var DL = window.HostthisDeepLink;
  var Tree = window.HostthisKBTree;
  var Search = window.HostthisKBSearch;

  var docEl = document.getElementById("kb-doc");
  var mainEl = document.getElementById("kb-main");
  var treeEl = document.getElementById("kb-tree");
  var tocEl = document.getElementById("kb-toc");
  var tocList = document.getElementById("kb-toc-list");
  var crumbsEl = document.getElementById("kb-crumbs");
  var rawLink = document.getElementById("kb-raw");
  var searchEl = document.getElementById("kb-search");
  var resultsEl = document.getElementById("kb-results");
  var hitsEl = document.getElementById("kb-hits");
  var noteEl = document.getElementById("kb-index-note");
  var emptyEl = document.getElementById("kb-results-empty");
  var scrimEl = document.getElementById("kb-scrim");

  var files = [];
  var fileSet = Object.create(null);
  var base = "";
  var tree = null;
  var index = null;
  var headings = [];
  var rendered = null;   // the path whose bytes are currently painted
  var pending = null;    // the path whose fetch is in flight
  var fragmentWired = false;

  function isDoc(p) { return /\.(md|markdown)$/i.test(p); }
  function has(p) { return Object.prototype.hasOwnProperty.call(fileSet, p); }

  function decode(s) {
    try { return decodeURIComponent(s); } catch (e) { return s; }
  }

  function urlFor(p) {
    return base + "/" + p.split("/").map(encodeURIComponent).join("/");
  }

  function rawURL(p) { return urlFor(p) + "?raw=1"; }

  // resolveBase recovers the prefix this base is served under: nothing on a
  // slug subdomain, "/p/<slug>" in path mode. A pathname that IS one of the
  // base's files settles it; otherwise only the path-mode shape can match.
  function resolveBase(paths) {
    var here = decode(location.pathname).replace(/^\/+/, "");
    if (here && paths.indexOf(here) >= 0) return "";
    var m = location.pathname.match(/^\/p\/[a-z0-9]{8}(?=\/|$)/);
    return m ? m[0] : "";
  }

  // In path mode a base is printed as "<apex>/p/<slug>", with no trailing
  // slash, so a relative link in the root document resolves a level ABOVE the
  // base and leaves it. The slash is added before anything renders, so every
  // link a document draws resolves inside the base.
  function normalizeRootURL() {
    if (!base || location.pathname !== base) return;
    history.replaceState(null, "", base + "/" + location.search + location.hash);
  }

  function pathOf(pathname) {
    var p = pathname;
    if (base && p.indexOf(base) === 0) p = p.slice(base.length);
    return decode(p).replace(/^\/+/, "");
  }

  function currentPath() { return pathOf(location.pathname); }

  // rootDoc is what the root renders: README.md, else the markdown that sorts
  // first, else nothing and the root falls back to a listing.
  function rootDoc() {
    if (has("README.md")) return "README.md";
    var i;
    for (i = 0; i < files.length; i++) {
      if (files[i].toLowerCase() === "readme.md") return files[i];
    }
    for (i = 0; i < files.length; i++) {
      if (isDoc(files[i])) return files[i];
    }
    return null;
  }

  function childrenOf(prefix) {
    return files.filter(function (p) { return p.indexOf(prefix) === 0 && p !== prefix; });
  }

  function plainClick(ev) {
    return ev.button === 0 && !ev.metaKey && !ev.ctrlKey && !ev.shiftKey && !ev.altKey;
  }

  // -- chrome ---------------------------------------------------------------

  // The trail is ONE line. When the path does not fit, its middle collapses
  // into a button that expands it again, and the expanded trail may wrap. Fit
  // is MEASURED at every render: four long names overflow a bar that ten short
  // ones sit in comfortably, so counting segments decides it wrong.
  var crumbPath = "";
  var crumbIsFile = false;
  var crumbExpanded = false;

  function setCrumbs(path, isFile) {
    crumbPath = path;
    crumbIsFile = isFile;
    crumbExpanded = false;
    drawCrumbs();
  }

  function crumbsOverflow() { return crumbsEl.scrollWidth > crumbsEl.clientWidth; }

  function makeSep() {
    var sep = document.createElement("span");
    sep.className = "kb-sep";
    sep.textContent = "/";
    return sep;
  }

  function makeRootCrumb() {
    var a = document.createElement("a");
    a.className = "kb-crumb";
    a.dataset.crumbKind = "root";
    a.href = base + "/";
    a.textContent = "root";
    a.addEventListener("click", function (ev) {
      if (!plainClick(ev)) return;
      ev.preventDefault();
      navigate("", "");
    });
    return a;
  }

  function makeCrumb(name, path, isFileCrumb, isLast) {
    var crumb;
    if (isFileCrumb) {
      crumb = document.createElement("span");
      crumb.dataset.crumbKind = "file";
    } else {
      crumb = document.createElement("button");
      crumb.type = "button";
      crumb.dataset.crumbKind = "dir";
      // A folder crumb moves the SIDEBAR. Which document is open is the
      // reader's business, and a folder is not a document.
      crumb.addEventListener("click", function () {
        if (tree) tree.show(path);
      });
    }
    crumb.className = "kb-crumb";
    crumb.textContent = name;
    if (isLast) crumb.setAttribute("aria-current", "page");
    return crumb;
  }

  function makeMoreCrumb() {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "kb-crumb kb-crumb-more";
    btn.dataset.crumbKind = "ellipsis";
    btn.textContent = "…";
    btn.setAttribute("aria-expanded", String(crumbExpanded));
    btn.setAttribute("aria-label", crumbExpanded ? "Collapse this path" : "Show every folder in this path");
    btn.addEventListener("click", function () {
      crumbExpanded = !crumbExpanded;
      drawCrumbs();
      var moved = crumbsEl.querySelector(".kb-crumb-more");
      if (moved) moved.focus();
    });
    return btn;
  }

  // hideOrder is the order middle segments give way in: the centre first, then
  // outward taking the root side before the file side, so the folders nearest
  // the current file survive longest. The first and last segments never go.
  function hideOrder(n) {
    var centre = Math.floor((n - 1) / 2);
    var order = [centre];
    var left = centre - 1;
    var right = centre + 1;
    while (left >= 1 || right <= n - 2) {
      if (left >= 1) order.push(left--);
      if (right <= n - 2) order.push(right++);
    }
    return order;
  }

  function drawCrumbs() {
    crumbsEl.textContent = "";
    crumbsEl.classList.toggle("expanded", crumbExpanded);
    crumbsEl.classList.remove("tight");
    crumbsEl.appendChild(makeRootCrumb());
    if (!crumbPath) return;

    var parts = crumbPath.replace(/\/+$/, "").split("/");
    var items = parts.map(function (name, i) {
      var last = i === parts.length - 1;
      var sep = makeSep();
      var crumb = makeCrumb(name, parts.slice(0, i + 1).join("/"), last && crumbIsFile, last);
      crumbsEl.appendChild(sep);
      crumbsEl.appendChild(crumb);
      return { sep: sep, crumb: crumb };
    });

    if (crumbExpanded) {
      crumbsEl.appendChild(makeSep());
      crumbsEl.appendChild(makeMoreCrumb());
      return;
    }
    // Two segments leave no middle to collapse, and a trail that fits keeps
    // every segment: the control exists only where it does something.
    if (items.length < 3 || !crumbsOverflow()) return;

    var centre = Math.floor((items.length - 1) / 2);
    crumbsEl.insertBefore(makeSep(), items[centre].sep);
    crumbsEl.insertBefore(makeMoreCrumb(), items[centre].sep);

    var order = hideOrder(items.length);
    for (var i = 0; i < order.length && crumbsOverflow(); i++) {
      items[order[i]].sep.hidden = true;
      items[order[i]].crumb.hidden = true;
    }
    // Root, ellipsis and file alone can still exceed the bar. The file name is
    // then the one that gives, ellipsised in place rather than clipped.
    if (crumbsOverflow()) crumbsEl.classList.add("tight");
  }

  function buildTOC() {
    tocList.textContent = "";
    headings.forEach(function (h) {
      var li = document.createElement("li");
      var a = document.createElement("a");
      a.className = "kb-toc-link lvl-" + h.level;
      a.href = "#" + h.id;
      a.dataset.targetId = h.id;
      a.textContent = h.text;
      a.addEventListener("click", function (ev) {
        ev.preventDefault();
        DL.setHash(h.id);
        DL.reveal(h.el);
        closeDrawers();
      });
      li.appendChild(a);
      tocList.appendChild(li);
    });
    tocEl.hidden = headings.length === 0;
    spy();
  }

  function setActiveTOC(id) {
    var links = tocList.querySelectorAll(".kb-toc-link");
    for (var i = 0; i < links.length; i++) {
      if (links[i].dataset.targetId === id) links[i].setAttribute("aria-current", "true");
      else links[i].removeAttribute("aria-current");
    }
  }

  // spy marks the heading the reader is under. The anchor is the top of the
  // scrolling pane plus a margin, so a heading counts as reached once it sits
  // at the top of the view rather than when it first appears at the bottom.
  function spy() {
    if (!headings.length) return;
    var edge = mainEl.getBoundingClientRect().top + 80;
    var active = headings[0];
    for (var i = 0; i < headings.length; i++) {
      if (headings[i].el.getBoundingClientRect().top > edge) break;
      active = headings[i];
    }
    setActiveTOC(active.id);
  }

  // -- rendering ------------------------------------------------------------

  // Heading anchors, added here because marked does not emit ids: without them
  // a #heading fragment has nothing to resolve against.
  function addHeadingIds(root) {
    var used = Object.create(null);
    headings = [];
    root.querySelectorAll("h1,h2,h3,h4,h5,h6").forEach(function (h) {
      var text = h.textContent.trim();
      var id = h.id;
      if (!id) {
        var slug = DL.slug(text) || "section";
        id = slug;
        var n = 1;
        while (used[id]) id = slug + "-" + n++;
        h.id = id;
      }
      used[id] = true;
      h.classList.add("anchored");
      h.title = "link to this section";
      h.addEventListener("click", function () {
        DL.setHash(id);
        DL.reveal(h);
      });
      headings.push({ el: h, id: id, text: text, level: parseInt(h.tagName.slice(1), 10) });
    });
  }

  function resolveTarget(target) {
    var t = target || DL.parse();
    if (!t || t.type !== "id") return;
    var el = document.getElementById(t.id);
    if (el) DL.reveal(el);
  }

  // The browser resolved the fragment at parse time, against an empty page, so
  // the shell resolves it again once content exists (docs/SPEC.md "Deep
  // links"). onResolve also registers the hashchange handler, so it is wired
  // once and every later render resolves directly.
  function afterPaint() {
    if (fragmentWired) {
      resolveTarget();
      return;
    }
    fragmentWired = true;
    DL.onResolve(resolveTarget);
  }

  function openDoc(path) {
    if (path === rendered) {
      if (tree) tree.setCurrent(path);
      setCrumbs(path, true);
      afterPaint();
      return;
    }
    pending = path;
    docEl.setAttribute("aria-busy", "true");
    docEl.dataset.path = path;
    if (tree) tree.setCurrent(path);
    setCrumbs(path, true);
    rawLink.hidden = false;
    rawLink.href = rawURL(path);
    fetch(rawURL(path), { credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.text();
      })
      .then(function (text) {
        if (pending !== path) return;   // a later navigation won the race
        if (index) index.seed(path, text);
        paint(text, path);
      })
      .catch(function (err) {
        if (pending !== path) return;
        fail("This document could not be loaded.", path + ": " + err.message, path);
      });
  }

  function paint(text, path) {
    docEl.innerHTML = DOMPurify.sanitize(marked.parse(text));
    docEl.removeAttribute("aria-busy");
    rendered = path;
    addHeadingIds(docEl);
    buildTOC();
    var h1 = docEl.querySelector("h1");
    document.title = (h1 && h1.textContent.trim()) || path;
    mainEl.scrollTop = 0;
    afterPaint();
  }

  // begin clears the content area for something that is not a rendered
  // document: a listing, or an error.
  function begin(path, isFile) {
    pending = null;
    rendered = null;
    headings = [];
    docEl.removeAttribute("aria-busy");
    docEl.textContent = "";
    docEl.dataset.path = path;
    if (tree) tree.setCurrent(isFile ? path : null);
    setCrumbs(path, isFile);
    rawLink.hidden = true;
    buildTOC();
  }

  // listing stands in for a document where there is none: the root of a base
  // holding no markdown, and any directory path. The shell builds it from the
  // file list; no response templates content into the page.
  function listing(prefix) {
    begin(prefix, false);
    // The sidebar follows the reader into the folder being listed.
    if (tree) tree.show(prefix.replace(/\/+$/, ""));
    var h = document.createElement("h1");
    h.textContent = prefix ? prefix.replace(/\/+$/, "") : "Files";
    docEl.appendChild(h);
    var kids = childrenOf(prefix);
    if (!kids.length) {
      var none = document.createElement("p");
      none.textContent = "This knowledge base holds no files.";
      docEl.appendChild(none);
      return;
    }
    var ul = document.createElement("ul");
    ul.id = "kb-listing";
    kids.forEach(function (p) {
      var li = document.createElement("li");
      li.appendChild(fileLink(p, p.slice(prefix.length)));
      ul.appendChild(li);
    });
    docEl.appendChild(ul);
    document.title = prefix ? prefix : "hostthis";
  }

  function fileLink(path, label) {
    var doc = isDoc(path);
    var a = document.createElement("a");
    a.href = urlFor(path);
    a.dataset.path = path;
    a.appendChild(Tree.icon(doc ? "doc" : "external", doc ? "" : "kb-ico-ext"));
    var span = document.createElement("span");
    span.textContent = label || path;
    a.appendChild(span);
    if (doc) {
      a.addEventListener("click", function (ev) {
        if (!plainClick(ev)) return;
        ev.preventDefault();
        navigate(path, "");
      });
    } else {
      a.dataset.external = "1";
      a.target = "_blank";
      a.rel = "noopener noreferrer";
    }
    return a;
  }

  // fail is the one way content goes wrong: an inline panel in the content
  // area, never a blank page.
  function fail(title, detail, path) {
    begin(path || docEl.dataset.path || "", false);
    var box = document.createElement("div");
    box.id = "kb-error";
    var h = document.createElement("h1");
    h.textContent = title;
    box.appendChild(h);
    if (detail) {
      var p = document.createElement("p");
      var code = document.createElement("code");
      code.textContent = detail;
      p.appendChild(code);
      box.appendChild(p);
    }
    var back = document.createElement("p");
    var a = document.createElement("a");
    a.href = base + "/";
    a.id = "kb-error-home";
    a.textContent = "Back to this knowledge base";
    a.addEventListener("click", function (ev) {
      if (!plainClick(ev)) return;
      ev.preventDefault();
      navigate("", "");
    });
    back.appendChild(a);
    box.appendChild(back);
    docEl.appendChild(box);
    document.title = "hostthis";
  }

  function show(path) {
    if (path === "") {
      var root = rootDoc();
      if (root) openDoc(root);
      else listing("");
      return;
    }
    if (has(path)) {
      if (isDoc(path)) {
        openDoc(path);
        return;
      }
      // Reachable only from a hand-typed URL: the server serves a
      // non-markdown file raw, so the shell never renders at its path.
      begin(path, false);
      var p = document.createElement("p");
      p.appendChild(fileLink(path, path));
      docEl.appendChild(p);
      return;
    }
    var dir = path.replace(/\/+$/, "") + "/";
    if (childrenOf(dir).length) {
      listing(dir);
      return;
    }
    fail("No file at this path.", path, path);
  }

  function navigate(path, hash) {
    var url = (path === "" ? base + "/" : urlFor(path)) + (hash || "");
    if (url !== location.pathname + location.hash) history.pushState(null, "", url);
    closeDrawers();
    hideResults();
    show(path);
  }

  // -- search ---------------------------------------------------------------

  var searchTimer = null;

  function runQuery() {
    var q = searchEl.value;
    if (q.trim().length < 2) {
      hideResults();
      return;
    }
    renderResults(index ? index.query(q) : { hits: [], truncated: false });
  }

  function renderResults(res) {
    hitsEl.textContent = "";
    resultsEl.hidden = false;
    updateNote();
    res.hits.forEach(function (hit) {
      hitsEl.appendChild(renderHit(hit));
    });
    emptyEl.hidden = res.hits.length > 0;
    if (res.truncated) {
      var more = document.createElement("p");
      more.id = "kb-results-truncated";
      more.className = "kb-more";
      more.textContent = "Showing the first " + res.hits.length + " matches.";
      hitsEl.appendChild(more);
    }
  }

  function renderHit(hit) {
    var a = document.createElement("a");
    a.className = "kb-hit";
    a.dataset.path = hit.path;
    a.dataset.kind = hit.kind;
    var hash = hit.heading ? "#" + hit.heading.id : "";
    if (hash) a.dataset.hash = hash;
    a.href = urlFor(hit.path) + hash;

    var head = document.createElement("div");
    head.className = "kb-hit-head";
    var kind = document.createElement("span");
    kind.className = "kb-hit-kind";
    kind.textContent = hit.kind;
    var path = document.createElement("span");
    path.className = "kb-hit-path";
    path.textContent = hit.path;
    head.appendChild(kind);
    head.appendChild(path);
    a.appendChild(head);

    if (hit.heading || hit.snippet) {
      var snip = document.createElement("div");
      snip.className = "kb-hit-snip";
      if (hit.heading) {
        snip.textContent = hit.heading.text;
      } else {
        // Built from text nodes: a document's own bytes are never markup here.
        snip.appendChild(document.createTextNode(hit.snippet.before));
        var mark = document.createElement("mark");
        mark.textContent = hit.snippet.match;
        snip.appendChild(mark);
        snip.appendChild(document.createTextNode(hit.snippet.after));
      }
      a.appendChild(snip);
    }

    if (hit.doc) {
      a.addEventListener("click", function (ev) {
        if (!plainClick(ev)) return;
        ev.preventDefault();
        navigate(hit.path, hash);
      });
    } else {
      a.dataset.external = "1";
      a.target = "_blank";
      a.rel = "noopener noreferrer";
    }
    return a;
  }

  function updateNote() {
    var st = index ? index.state() : null;
    if (!st) return;
    document.body.dataset.kbIndex = st.done ? (st.partial ? "partial" : "complete") : "indexing";
    noteEl.dataset.indexed = String(st.indexed);
    noteEl.dataset.total = String(st.total);
    if (!st.done) {
      noteEl.hidden = false;
      noteEl.textContent = "Indexing text: " + st.indexed + " of " + st.total +
        " documents so far. File names already match across the whole base.";
      return;
    }
    if (!st.partial) {
      // Emptied as well as hidden: a progress line left in the element is read
      // back by anything that reads its text rather than its visibility.
      noteEl.hidden = true;
      noteEl.textContent = "";
      return;
    }
    // The reasons are independent and can both hold, so they are reported from
    // what actually stopped the walk rather than inferred from the count.
    var why = [];
    if (st.bounded) {
      why.push("the index stops at " + st.maxFiles + " documents or " +
        Math.round(st.maxBytes / (1024 * 1024)) + " MiB of markdown");
    }
    if (st.failures) {
      why.push(st.failures + " document(s) could not be read");
    }
    noteEl.hidden = false;
    noteEl.textContent = "Partial index: text and headings cover " + st.indexed + " of " +
      st.total + " documents, because " + why.join(" and ") + ". File names still match across the whole base.";
  }

  function hideResults() {
    resultsEl.hidden = true;
    hitsEl.textContent = "";
  }

  function moveHit(dir) {
    var hits = hitsEl.querySelectorAll(".kb-hit");
    if (!hits.length) return;
    var at = -1;
    for (var i = 0; i < hits.length; i++) {
      if (hits[i] === document.activeElement) at = i;
    }
    var next = at + dir;
    if (next < 0) {
      searchEl.focus();
      return;
    }
    if (next >= hits.length) next = hits.length - 1;
    hits[next].focus();
  }

  function wireSearch() {
    searchEl.addEventListener("input", function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(runQuery, 120);
    });
    searchEl.addEventListener("focus", function () {
      if (searchEl.value.trim().length >= 2) runQuery();
    });
    searchEl.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape") {
        searchEl.value = "";
        hideResults();
        searchEl.blur();
        return;
      }
      if (ev.key === "ArrowDown") {
        ev.preventDefault();
        moveHit(1);
        return;
      }
      if (ev.key === "Enter") {
        var first = hitsEl.querySelector(".kb-hit");
        if (first) {
          ev.preventDefault();
          first.click();
        }
      }
    });
    resultsEl.addEventListener("keydown", function (ev) {
      if (ev.key === "ArrowDown") { ev.preventDefault(); moveHit(1); }
      else if (ev.key === "ArrowUp") { ev.preventDefault(); moveHit(-1); }
      else if (ev.key === "Escape") { hideResults(); searchEl.focus(); }
    });
    document.addEventListener("click", function (ev) {
      if (resultsEl.hidden) return;
      if (resultsEl.contains(ev.target) || ev.target === searchEl) return;
      hideResults();
    });
  }

  // -- panes on a narrow screen ---------------------------------------------

  var narrowTree = matchMedia("(max-width: 760px)");
  var narrowToc = matchMedia("(max-width: 1100px)");

  // Wide: the pane is a column, so the button hides it. Narrow: it is a drawer
  // over the document, so the same button opens it.
  function togglePane(btn, drawer, hide, narrow) {
    var cls = narrow.matches ? drawer : hide;
    var on = document.body.classList.toggle(cls);
    btn.setAttribute("aria-expanded", String(narrow.matches ? on : !on));
    // A pane changes what the bar has to work with, and the trail's collapse is
    // a measurement rather than a count.
    drawCrumbs();
  }

  function closeDrawers() {
    document.body.classList.remove("tree-open", "toc-open");
    drawCrumbs();
  }

  function wireToggles() {
    var filesBtn = document.getElementById("kb-files-toggle");
    var tocBtn = document.getElementById("kb-toc-toggle");
    filesBtn.addEventListener("click", function () {
      togglePane(filesBtn, "tree-open", "tree-hidden", narrowTree);
    });
    tocBtn.addEventListener("click", function () {
      togglePane(tocBtn, "toc-open", "toc-hidden", narrowToc);
    });
    scrimEl.addEventListener("click", closeDrawers);
  }

  // -- start ----------------------------------------------------------------

  function wireDocLinks() {
    // A link to another document in this base navigates the shell; anything
    // else follows to its own URL, where the server serves it.
    docEl.addEventListener("click", function (ev) {
      if (ev.defaultPrevented || !plainClick(ev)) return;
      var a = ev.target.closest ? ev.target.closest("a[href]") : null;
      if (!a || a.dataset.external === "1") return;
      var url;
      try { url = new URL(a.getAttribute("href"), location.href); } catch (e) { return; }
      if (url.origin !== location.origin) return;
      if (url.pathname === location.pathname && url.hash) return;   // same document
      var p = pathOf(url.pathname);
      if (!isDoc(p) || !has(p)) return;
      ev.preventDefault();
      navigate(p, url.hash);
    });
  }

  function wireHistory() {
    addEventListener("popstate", function () {
      var p = currentPath();
      var want = p === "" ? rootDoc() : p;
      if (want && want === rendered) {
        resolveTarget();
        return;
      }
      show(p);
    });
  }

  function wireResize() {
    addEventListener("resize", drawCrumbs);
  }

  function wireScroll() {
    var ticking = false;
    mainEl.addEventListener("scroll", function () {
      if (ticking) return;
      ticking = true;
      requestAnimationFrame(function () {
        ticking = false;
        spy();
      });
    }, { passive: true });
  }

  function start(list) {
    files = list.filter(function (p) { return typeof p === "string" && p !== ""; });
    files.forEach(function (p) { fileSet[p] = true; });
    base = resolveBase(files);
    normalizeRootURL();

    tree = Tree.build(files, {
      isDoc: isDoc,
      hrefFor: urlFor,
      onOpen: function (path) { navigate(path, ""); },
    });
    treeEl.textContent = "";
    treeEl.appendChild(tree.el);
    treeEl.dataset.treeReady = "1";
    treeEl.dataset.files = String(files.length);

    wireDocLinks();
    wireHistory();
    wireScroll();
    wireResize();
    wireToggles();
    wireSearch();

    show(currentPath());

    index = Search.create({
      files: files,
      isDoc: isDoc,
      rawURL: rawURL,
      slug: DL.slug,
      onProgress: function () {
        updateNote();
        if (!resultsEl.hidden) runQuery();
      },
    });
    index.start();
  }

  fetch("?files=1", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    })
    .then(start)
    .catch(function (err) {
      treeEl.dataset.treeReady = "error";
      fail("This knowledge base could not be listed.", String(err && err.message ? err.message : err), "");
    });
})();
