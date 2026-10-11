// Line numbering with click/tap range selection, shared by every viewer whose
// content is a sequence of addressable lines.
//
// The diff viewer grew this first and keeps its own copy, because its rows are
// produced by diff2html and carry two numbering schemes (old file and new
// file) that this does not model. Everything here is the single-numbering
// case: text and logs.
(function () {
  "use strict";

  var DL = window.HostthisDeepLink;

  // Touch has no shift key, so a range would be unreachable on a phone. The
  // pointer type is read from the event that STARTED the click rather than
  // from a media query, so a hybrid laptop behaves per-input.
  var lastTouch = false;
  document.addEventListener("pointerdown", function (e) {
    lastTouch = e.pointerType === "touch" || e.pointerType === "pen";
  }, true);

  // attach wires a container whose children each carry data-line="<n>".
  // Returns { select, clear, current } so a viewer that re-renders (a log
  // being filtered) can restore the selection afterwards.
  function attach(mount, opts) {
    opts = opts || {};
    var anchor = null;
    var hinted = false;

    function rows() {
      return Array.prototype.slice.call(mount.querySelectorAll("[data-line]"));
    }

    function rowFor(n) {
      return mount.querySelector('[data-line="' + n + '"]');
    }

    function clearMarks() {
      mount.querySelectorAll(".lg-sel, .lg-arrive").forEach(function (el) {
        el.classList.remove("lg-sel", "lg-arrive");
      });
    }

    function clear() {
      clearMarks();
      anchor = null;
      DL.clearHash();
    }

    // Endpoints are ROWS, not numbers: a filtered log hides lines, so the
    // numbers between two visible rows are not the rows between them.
    function paint(a, b, arrived) {
      clearMarks();
      var all = rows();
      var i = all.indexOf(a), j = all.indexOf(b);
      if (i < 0 || j < 0) return null;
      if (i > j) { var t = i; i = j; j = t; }
      var span = all.slice(i, j + 1);
      span.forEach(function (el) { el.classList.add("lg-sel"); });
      var lo = span[0].dataset.line;
      var hi = span[span.length - 1].dataset.line;
      DL.setHash("L" + lo + (lo === hi ? "" : "-L" + hi));
      if (arrived) {
        span.forEach(function (el) { el.classList.add("lg-arrive"); });
        span[0].scrollIntoView({ block: "center", behavior: "smooth" });
      }
      return span;
    }

    function select(el, extend) {
      // Tapping inside the selection clears it. On a mouse that only matters
      // for a single line, since Escape handles the rest; on touch there is
      // no Escape, so it is the only way out of a range.
      if (el.classList.contains("lg-sel")) {
        var lit = mount.querySelectorAll(".lg-sel");
        if (lit.length === 1 || lastTouch) {
          clear();
          return;
        }
      }
      var from = extend && anchor ? anchor : null;
      if (!from) {
        anchor = el;
        if (lastTouch && opts.hint) hint();
      }
      paint(from || el, el, false);
    }

    function hint() {
      if (hinted) return;
      hinted = true;
      var h = document.createElement("div");
      h.id = "lg-hint";
      h.textContent = "tap another line to extend";
      document.body.appendChild(h);
      requestAnimationFrame(function () { h.classList.add("on"); });
      setTimeout(function () {
        h.classList.remove("on");
        setTimeout(function () { h.remove(); }, 400);
      }, 2600);
    }

    mount.addEventListener("click", function (e) {
      var num = e.target.closest(".lg-num");
      if (!num) return;
      e.preventDefault();
      var el = num.closest("[data-line]");
      if (!el) return;
      // On touch a second tap extends, because there is no shift key. On a
      // mouse it replaces, which is what a plain click means everywhere else.
      var extend = e.shiftKey || (lastTouch && anchor && anchor !== el);
      select(el, extend);
    });

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") clear();
    });

    // Resolves a fragment onto rows, clamped to the ones that exist so a
    // hand-written range past the end still lands somewhere.
    function resolve(t, arrived) {
      if (!t || t.type !== "line") return;
      var all = rows(), a = null, b = null;
      for (var i = 0; i < all.length; i++) {
        var n = parseInt(all[i].dataset.line, 10);
        if (n >= t.from && n <= t.to) {
          if (!a) a = all[i];
          b = all[i];
        }
      }
      if (!a) return;
      anchor = a;
      paint(a, b, arrived !== false);
    }

    return {
      select: select,
      clear: clear,
      resolve: resolve,
      rowFor: rowFor,
      current: function () {
        return rows().filter(function (el) { return el.classList.contains("lg-sel"); })
          .map(function (el) { return el.dataset.line; });
      },
    };
  }

  window.HostthisLineGutter = { attach: attach };
})();
