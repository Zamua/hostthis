// Placeholder bootstrap: the knowledge base interface (file tree, breadcrumbs,
// table of contents, search, markdown rendering) lands separately. What is here
// exercises the two server contracts it will be built on - the file list and
// the raw bytes of one document.
(function () {
  "use strict";
  const out = document.getElementById("doc");
  const show = (text) => {
    out.textContent = text;
    out.removeAttribute("aria-busy");
  };
  const path = decodeURIComponent(location.pathname);
  const request = /\.(md|markdown)$/i.test(path)
    ? fetch(path + "?raw=1", { credentials: "same-origin" }).then((r) => r.text())
    : fetch("?files=1", { credentials: "same-origin" })
        .then((r) => r.json())
        .then((files) => files.join("\n"));
  request.then(show, (err) => show(String(err)));
})();
