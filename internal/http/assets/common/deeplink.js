// Fragment resolution shared by every client-render shell.
//
// The browser resolves a fragment once, at parse time. Every shell here renders
// AFTER an async ?raw fetch, so that native attempt always runs against an
// empty document and lands at the top of the page. A shell must therefore
// resolve the fragment itself once its content exists, and again on hashchange
// (which fires without a reload when only the fragment changes).
(function () {
  "use strict";

  // parse maps a fragment onto the addressing schemes in docs/SPEC.md
  // "Deep links". Returns null for an empty or unrecognised fragment; a shell
  // ignores targets whose type it does not address.
  function parse(hash) {
    var h = (hash || location.hash || "").replace(/^#/, "");
    if (!h) return null;
    var m;
    // Optional F<n> scopes the lines to one file of a multi-file diff, since
    // the same line number occurs in every file. Bare L<n> means the first.
    if ((m = h.match(/^(?:F(\d+))?L(\d+)(?:-L?(\d+))?$/))) {
      var from = parseInt(m[2], 10);
      var to = m[3] ? parseInt(m[3], 10) : from;
      return {
        type: "line",
        file: m[1] ? parseInt(m[1], 10) : 1,
        from: Math.min(from, to),
        to: Math.max(from, to),
      };
    }
    if ((m = h.match(/^page=(\d+)$/))) return { type: "page", n: parseInt(m[1], 10) };
    if ((m = h.match(/^row=(\d+)$/))) return { type: "row", n: parseInt(m[1], 10) };
    return { type: "id", id: decodeURIComponent(h) };
  }

  // onResolve registers the shell's handler and drives it: once now (content is
  // assumed present by the time a shell calls this) and on every later
  // hashchange.
  function onResolve(fn) {
    var run = function () {
      var t = parse();
      if (t) fn(t);
    };
    run();
    addEventListener("hashchange", run);
  }

  // reveal scrolls el into view and flashes it, so the reader's eye lands on
  // the addressed thing rather than on wherever the scroll happened to stop.
  function reveal(el) {
    if (!el) return;
    el.scrollIntoView({ block: "center", behavior: "smooth" });
    el.classList.add("dl-flash");
    setTimeout(function () {
      el.classList.remove("dl-flash");
    }, 1600);
  }

  // setHash rewrites the fragment WITHOUT adding a history entry and without
  // re-triggering the shell's own handler, which is what lets a viewer update
  // the address bar as the reader clicks around. Back still leaves the paste
  // rather than replaying every line they looked at.
  function setHash(frag) {
    if (("#" + frag) === location.hash) return;
    history.replaceState(null, "", "#" + frag);
  }

  // clearHash drops the fragment without adding a history entry and without
  // reloading. The path is restated explicitly because assigning an empty hash
  // leaves a bare "#" in the address bar, which still reads as a link to
  // somewhere and would be copied along with the URL.
  function clearHash() {
    if (!location.hash) return;
    history.replaceState(null, "", location.pathname + location.search);
  }

  // slug converts heading text to an id, matching the anchor style used across
  // markdown renderers so links written elsewhere resolve here.
  function slug(text) {
    return String(text)
      .trim()
      .toLowerCase()
      .replace(/[^\w\s-]/g, "")
      .replace(/\s+/g, "-")
      .replace(/-+/g, "-");
  }

  window.HostthisDeepLink = {
    parse: parse,
    onResolve: onResolve,
    reveal: reveal,
    setHash: setHash,
    clearHash: clearHash,
    slug: slug,
  };
})();
