// Flame graph renderer for folded stack profiles.
//
// The whole algorithm is a prefix-tree aggregation plus a rectangle layout,
// which is why this is written here rather than pulled in as a library.
(function () {
  "use strict";

  var DL = window.HostthisDeepLink;
  var mount = document.getElementById("flame");
  var tip = document.getElementById("tip");
  var crumbs = document.getElementById("crumbs");
  var filterBox = document.getElementById("filter");
  var matched = document.getElementById("matched");
  var resetBtn = document.getElementById("reset");

  var ROW = 19;
  // Below about a third of a pixel a frame cannot be seen or hit, and a deep
  // profile has tens of thousands of them. Skipping the subpixel frames is
  // what keeps the DOM proportional to what is visible rather than to the
  // profile's size.
  var MIN_FRAC = 0.0004;

  var root = null;     // whole profile
  var focus = null;    // currently zoomed node
  var query = "";

  // ---- parse -------------------------------------------------------------

  // Folded format: "frame;frame;frame <count>". Frames may contain spaces, so
  // the count is the last whitespace-separated field, never the second.
  function parseFolded(text) {
    var r = { name: "all", value: 0, children: Object.create(null), depth: 0 };
    var lines = text.split("\n");
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].replace(/\r$/, "");
      if (!line.trim() || line[0] === "#") continue;
      var cut = line.lastIndexOf(" ");
      var tab = line.lastIndexOf("\t");
      if (tab > cut) cut = tab;
      if (cut < 0) continue;
      var n = parseInt(line.slice(cut + 1), 10);
      if (!isFinite(n) || n <= 0) continue;
      var frames = line.slice(0, cut).trim().split(";");
      var node = r;
      r.value += n;
      for (var f = 0; f < frames.length; f++) {
        var name = frames[f];
        if (!name) continue;
        var kid = node.children[name];
        if (!kid) {
          kid = { name: name, value: 0, children: Object.create(null),
                  depth: node.depth + 1, parent: node };
          node.children[name] = kid;
        }
        kid.value += n;
        node = kid;
      }
    }
    return r;
  }

  // Children are laid out widest-first so the eye finds the expensive path at
  // the left edge of every row instead of hunting along it.
  function order(node) {
    var kids = Object.keys(node.children).map(function (k) { return node.children[k]; });
    kids.sort(function (a, b) { return b.value - a.value || (a.name < b.name ? -1 : 1); });
    node.kids = kids;
    kids.forEach(order);
    return node;
  }

  function stackOf(node) {
    var parts = [];
    for (var n = node; n && n.parent; n = n.parent) parts.unshift(n.name);
    return parts.join(";");
  }

  // ---- colour ------------------------------------------------------------

  // Hue is derived from the frame name so a frame keeps its colour across
  // renders and across profiles, which is what makes two graphs comparable by
  // eye. The warm band is the flame-graph convention.
  function hue(name) {
    var h = 0;
    for (var i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) | 0;
    return Math.abs(h) % 46;
  }

  // ---- render ------------------------------------------------------------

  function render() {
    var f = focus || root;
    mount.textContent = "";
    var frag = document.createDocumentFragment();
    var maxDepth = { v: 0 };
    // The focused node spans the full width, and its ancestors are drawn above
    // it full-width too, so zooming keeps the path visible rather than
    // discarding the context that led there.
    var chain = [];
    for (var n = f; n; n = n.parent) chain.unshift(n);
    chain.forEach(function (node, i) {
      frag.appendChild(box(node, 0, 1, i, true));
      maxDepth.v = i;
    });
    layout(f, 0, 1, chain.length - 1, frag, maxDepth);
    mount.appendChild(frag);
    mount.style.height = ((maxDepth.v + 1) * ROW + 8) + "px";
    mount.setAttribute("aria-busy", "false");
    drawCrumbs(chain);
    applyQuery();
  }

  function layout(node, x0, width, depth, frag, maxDepth) {
    var acc = x0;
    for (var i = 0; i < node.kids.length; i++) {
      var kid = node.kids[i];
      var w = width * (kid.value / node.value);
      if (w >= MIN_FRAC) {
        frag.appendChild(box(kid, acc, w, depth + 1, false));
        if (depth + 1 > maxDepth.v) maxDepth.v = depth + 1;
        layout(kid, acc, w, depth + 1, frag, maxDepth);
      }
      acc += w;
    }
  }

  function box(node, x, w, depth, isChain) {
    var d = document.createElement("div");
    d.className = "fr" + (isChain ? " chain" : "");
    d.style.left = (x * 100) + "%";
    d.style.width = (w * 100) + "%";
    d.style.top = (depth * ROW) + "px";
    d.style.setProperty("--h", hue(node.name));
    d.textContent = node.name;
    d.dataset.name = node.name;
    d._node = node;
    return d;
  }

  function drawCrumbs(chain) {
    crumbs.textContent = "";
    var total = root.value;
    chain.forEach(function (node, i) {
      if (i) crumbs.appendChild(document.createTextNode(" / "));
      var a = document.createElement("button");
      a.type = "button";
      a.className = "crumb";
      a.textContent = node.name;
      a.onclick = function () { setFocus(node); };
      crumbs.appendChild(a);
    });
    var f = chain[chain.length - 1];
    var pctText = ((f.value / total) * 100).toFixed(1) + "% of " + total + " samples";
    var s = document.createElement("span");
    s.className = "share";
    s.textContent = pctText;
    crumbs.appendChild(s);
  }

  // ---- interaction -------------------------------------------------------

  function setFocus(node, push) {
    focus = node === root ? null : node;
    render();
    if (push !== false) writeHash();
  }

  // The fragment states BOTH axes, so zooming does not discard an active
  // search and searching does not discard an active zoom.
  function writeHash() {
    var parts = [];
    if (focus) parts.push("focus=" + encodeURIComponent(stackOf(focus)));
    if (query) parts.push("q=" + encodeURIComponent(query));
    if (parts.length) DL.setHash(parts.join("&"));
    else DL.clearHash();
  }

  function applyQuery() {
    var q = query.toLowerCase();
    var hit = 0;
    var seen = Object.create(null);
    mount.querySelectorAll(".fr").forEach(function (el) {
      var on = !!q && el.dataset.name.toLowerCase().indexOf(q) >= 0;
      el.classList.toggle("hit", on);
    });
    if (q) {
      // Matched share is summed over the tree, not over the rendered boxes:
      // subpixel frames are not in the DOM, and a node's ancestors would be
      // double-counted. Descending stops at a match so a matching frame's
      // matching child is counted once.
      (function walk(node) {
        if (node.parent && node.name.toLowerCase().indexOf(q) >= 0) {
          hit += node.value;
          return;
        }
        node.kids.forEach(walk);
      })(root);
      matched.textContent = hit
        ? ((hit / root.value) * 100).toFixed(1) + "% matched"
        : "no match";
    } else {
      matched.textContent = "";
    }
  }

  mount.addEventListener("click", function (e) {
    var el = e.target.closest(".fr");
    if (el && el._node) setFocus(el._node);
  });

  mount.addEventListener("mousemove", function (e) {
    var el = e.target.closest(".fr");
    if (!el || !el._node) { tip.hidden = true; return; }
    var n = el._node;
    tip.textContent = n.name + "  -  " + n.value + " samples ("
      + ((n.value / root.value) * 100).toFixed(2) + "%)";
    tip.hidden = false;
    var pad = 12;
    var x = Math.min(e.clientX + pad, innerWidth - tip.offsetWidth - 4);
    tip.style.left = Math.max(4, x) + "px";
    tip.style.top = (e.clientY + pad) + "px";
  });
  mount.addEventListener("mouseleave", function () { tip.hidden = true; });

  resetBtn.onclick = function () { setFocus(root); };

  filterBox.addEventListener("input", function () {
    query = filterBox.value.trim();
    applyQuery();
    writeHash();
  });

  // A stack from an older recording may name frames this profile no longer
  // has. Resolving to the deepest ancestor that DOES exist keeps such a link
  // useful instead of dropping the reader at the root with no explanation.
  function findStack(path) {
    var node = root;
    var parts = path.split(";");
    for (var i = 0; i < parts.length; i++) {
      var kid = node.children[parts[i]];
      if (!kid) break;
      node = kid;
    }
    return node;
  }

  // ---- boot --------------------------------------------------------------

  fetch(location.pathname + "?raw=1", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("fetch failed: " + r.status);
      return r.text();
    })
    .then(function (text) {
      root = order(parseFolded(text));
      if (!root.value) {
        mount.textContent = "empty profile";
        mount.setAttribute("aria-busy", "false");
        return;
      }
      render();
      // Resolution runs only after the tree exists. The browser already tried
      // the fragment at parse time against an empty document.
      DL.onResolve(function (t) {
        if (t.type !== "flame") return;
        query = t.q;
        filterBox.value = t.q;
        // Focus is applied before the highlight because render() repaints the
        // boxes; highlighting first would mark a DOM that is about to be
        // rebuilt, and an arriving link would show its matches nowhere.
        setFocus(t.stack ? findStack(t.stack) : root, false);
      });
    })
    .catch(function (err) {
      mount.textContent = "could not load profile: " + err.message;
      mount.setAttribute("aria-busy", "false");
    });
})();
