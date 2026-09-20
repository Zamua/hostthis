// File navigation for the knowledge base shell: the base's paths as drill-down
// navigation, ONE level at a time. The sidebar shows the folder the reader is
// in and its children; entering a child folder re-renders the sidebar and
// leaves the document alone. Markdown opens in the shell; every other file is a
// link to its own URL, where the server serves it raw, marked with an
// external-link icon so the difference is visible before the click.
//
// One level rather than a recursive tree because indentation and row count both
// grow with depth, and a ten-level base spends the column on whitespace. Search
// reaches any file at any depth without walking to it, which is what makes
// navigating one level at a time enough.
(function () {
  "use strict";

  var NS = "http://www.w3.org/2000/svg";
  var PATHS = {
    chevron: "M6 4.5l4 3.5-4 3.5",
    folder: "M2.5 12.5v-9h3.4l1.3 1.8h6.3v7.2z",
    doc: "M4.5 2.5h4.8l2.7 2.7v8.3h-7.5zM9.3 2.5v2.7h2.7",
    external: "M9.5 3.5h3v3M12.5 3.5l-4.5 4.5M11 9.5V13H3V5h3.5",
  };

  // icon builds an SVG node rather than assigning markup, so no code path here
  // can grow into one that writes a user-controlled string as HTML.
  function icon(name, cls) {
    var svg = document.createElementNS(NS, "svg");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "1.4");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    svg.setAttribute("class", "kb-ico" + (cls ? " " + cls : ""));
    var p = document.createElementNS(NS, "path");
    p.setAttribute("d", PATHS[name]);
    svg.appendChild(p);
    return svg;
  }

  // nest turns flat paths into directory nodes. Ordering is directories first
  // then files, each alphabetically, which is what a reader scanning for a
  // section expects; the file list's own path-sort drives the search index
  // instead, where the bound has to fall in a fixed place.
  function nest(files) {
    var root = { name: "", path: "", dir: true, children: [], index: {} };
    files.forEach(function (full) {
      var parts = full.split("/");
      var node = root;
      for (var i = 0; i < parts.length; i++) {
        if (i === parts.length - 1) {
          node.children.push({ name: parts[i], path: full, dir: false, children: [] });
          break;
        }
        var key = "d:" + parts[i];
        var next = node.index[key];
        if (!next) {
          next = {
            name: parts[i],
            path: parts.slice(0, i + 1).join("/"),
            dir: true,
            children: [],
            index: {},
          };
          node.index[key] = next;
          node.children.push(next);
        }
        node = next;
      }
    });
    sort(root);
    return root;
  }

  function sort(node) {
    node.children.sort(function (a, b) {
      if (a.dir !== b.dir) return a.dir ? -1 : 1;
      return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });
    });
    node.children.forEach(function (c) {
      if (c.dir) sort(c);
    });
  }

  // dirIndex maps every folder's path to its node, so the sidebar can open a
  // folder named by a breadcrumb or by the document being read.
  function dirIndex(root) {
    var map = { "": root };
    (function walk(node) {
      node.children.forEach(function (child) {
        if (!child.dir) return;
        map[child.path] = child;
        walk(child);
      });
    })(root);
    return map;
  }

  function parentOf(path) {
    var cut = path.lastIndexOf("/");
    return cut < 0 ? "" : path.slice(0, cut);
  }

  function nameOf(path) { return path.slice(path.lastIndexOf("/") + 1); }

  function build(files, opts) {
    var root = nest(files);
    var dirs = dirIndex(root);

    var el = document.createElement("div");
    el.className = "kb-tree-body";

    var here = "";          // the folder the sidebar is showing
    var currentFile = null; // the document being read, marked when it is here
    var anchors = {};       // path -> the file anchor in THIS render

    function row(tag, cls) {
      var e = document.createElement(tag);
      if (tag === "button") e.type = "button";
      e.className = "kb-row " + cls;
      return e;
    }

    function renderUp() {
      var parent = parentOf(here);
      var btn = row("button", "kb-up");
      btn.dataset.parent = parent;
      btn.setAttribute("aria-label", "Up to " + (parent || "root"));
      btn.appendChild(icon("chevron", "kb-chev kb-chev-back"));
      var label = document.createElement("span");
      label.textContent = parent === "" ? "root" : nameOf(parent);
      btn.appendChild(label);
      btn.addEventListener("click", function () { show(parent); });
      return btn;
    }

    function renderHere() {
      var head = document.createElement("p");
      head.className = "kb-tree-here";
      head.dataset.dir = here;
      head.textContent = here === "" ? "Files" : nameOf(here);
      if (here) head.title = here;
      return head;
    }

    function renderDir(node) {
      var li = document.createElement("li");
      var btn = row("button", "kb-dir");
      btn.dataset.dir = node.path;
      btn.appendChild(icon("folder"));
      var label = document.createElement("span");
      label.textContent = node.name;
      btn.appendChild(label);
      btn.appendChild(icon("chevron", "kb-chev kb-chev-into"));
      // Sidebar navigation only: which document is open is the reader's
      // business, and a folder is not a document.
      btn.addEventListener("click", function () { show(node.path); });
      li.appendChild(btn);
      return li;
    }

    function renderFile(node) {
      var li = document.createElement("li");
      var a = row("a", "kb-file");
      a.dataset.path = node.path;
      a.href = opts.hrefFor(node.path);
      var doc = opts.isDoc(node.path);
      a.appendChild(icon("doc"));
      var label = document.createElement("span");
      label.textContent = node.name;
      a.appendChild(label);
      if (doc) {
        a.addEventListener("click", function (ev) {
          if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
          ev.preventDefault();
          opts.onOpen(node.path);
        });
      } else {
        // A non-markdown file is never rendered in the shell: it serves raw at
        // its own URL, so it opens as its own page.
        a.classList.add("kb-ext");
        a.dataset.external = "1";
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        a.appendChild(icon("external", "kb-ico-ext"));
      }
      anchors[node.path] = a;
      li.appendChild(a);
      return li;
    }

    function render() {
      el.textContent = "";
      anchors = {};
      // The root has no parent, so nothing to go up to.
      if (here !== "") el.appendChild(renderUp());
      el.appendChild(renderHere());
      var ul = document.createElement("ul");
      ul.className = "kb-tree-list";
      (dirs[here] || root).children.forEach(function (child) {
        ul.appendChild(child.dir ? renderDir(child) : renderFile(child));
      });
      el.appendChild(ul);
      mark();
    }

    function mark() {
      var a = currentFile ? anchors[currentFile] : null;
      if (!a) return;
      a.classList.add("current");
      a.setAttribute("aria-current", "page");
      // inline as well as block: a long row can sit outside the pane
      // horizontally, where revealing it vertically alone shows nothing.
      a.scrollIntoView({ block: "nearest", inline: "nearest" });
    }

    function show(dir) {
      here = Object.prototype.hasOwnProperty.call(dirs, dir) ? dir : "";
      render();
    }

    render();

    return {
      el: el,
      // show moves the sidebar to a folder, for a breadcrumb or a listing.
      show: show,
      // setCurrent follows the reader: a document opened from a search hit, a
      // link or a typed URL puts the sidebar in that document's folder.
      setCurrent: function (path) {
        currentFile = path || null;
        show(path ? parentOf(path) : here);
      },
      at: function () { return here; },
    };
  }

  window.HostthisKBTree = { build: build, icon: icon };
})();
