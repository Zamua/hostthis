// Trivial progressive-enhancement script. The plain static demo has no
// framework and no client-side router: every page is a real file. This
// just proves a .js asset round-trips byte-identically and is served with
// the text/javascript content-type.
document.addEventListener("DOMContentLoaded", () => {
  const main = document.querySelector("main");
  if (main) {
    main.dataset.enhanced = "true";
  }
});
