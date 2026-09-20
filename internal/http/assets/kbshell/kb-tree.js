// File tree for the knowledge base shell: the base's paths as a nested,
// collapsible list. Markdown opens in the shell; every other file is a link to
// its own URL, where the server serves it raw, marked with an external-link
// icon so the difference is visible before the click.
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

  function build(files, opts) {
    var root = nest(files);
    // A small base is easier to read whole; a large one opens on the top level
    // and expands to wherever the reader actually is.
    var openAll = files.length <= 30;
    var anchors = {};
    var dirs = {};
    var current = null;

    function renderList(node) {
      var ul = document.createElement("ul");
      node.children.forEach(function (child) {
        ul.appendChild(child.dir ? renderDir(child) : renderFile(child));
      });
      return ul;
    }

    function renderDir(node) {
      var li = document.createElement("li");
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "kb-row kb-dir";
      btn.dataset.dir = node.path;
      btn.setAttribute("aria-expanded", openAll ? "true" : "false");
      btn.appendChild(icon("chevron", "kb-chev"));
      btn.appendChild(icon("folder"));
      var label = document.createElement("span");
      label.textContent = node.name;
      btn.appendChild(label);
      var kids = renderList(node);
      kids.hidden = !openAll;
      btn.addEventListener("click", function () {
        var open = btn.getAttribute("aria-expanded") === "true";
        btn.setAttribute("aria-expanded", open ? "false" : "true");
        kids.hidden = open;
      });
      dirs[node.path] = { btn: btn, kids: kids };
      li.appendChild(btn);
      li.appendChild(kids);
      return li;
    }

    function renderFile(node) {
      var li = document.createElement("li");
      var a = document.createElement("a");
      a.className = "kb-row kb-file";
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

    function expandTo(path) {
      var parts = path.split("/");
      for (var i = 1; i < parts.length; i++) {
        var d = dirs[parts.slice(0, i).join("/")];
        if (d) {
          d.btn.setAttribute("aria-expanded", "true");
          d.kids.hidden = false;
        }
      }
    }

    var el = renderList(root);
    el.className = "kb-tree-root";
    return {
      el: el,
      setCurrent: function (path) {
        if (current && anchors[current]) {
          anchors[current].classList.remove("current");
          anchors[current].removeAttribute("aria-current");
        }
        current = path;
        if (!path || !anchors[path]) return;
        expandTo(path);
        anchors[path].classList.add("current");
        anchors[path].setAttribute("aria-current", "page");
        anchors[path].scrollIntoView({ block: "nearest" });
      },
    };
  }

  window.HostthisKBTree = { build: build, icon: icon };
})();
